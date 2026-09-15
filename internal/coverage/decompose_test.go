package coverage

import (
	"bytes"
	"errors"
	"flag"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/eventlog"
)

// The phase 2 definition of done, first half: decomposition matches its
// golden fixture, byte for byte.
//
// A change in geometry is supposed to show up here and nowhere else. When it
// is intended, regenerate the fixtures and review the diff:
//
//	go test ./internal/coverage -run TestDecomposeGolden -update

var update = flag.Bool("update", false, "rewrite the golden fixtures in testdata/")

type decomposeCase struct {
	name   string
	area   func(t *testing.T) domain.Area
	tactic Tactic
	params Params
	convex bool
}

// Test polygons are literal metre offsets rather than computed with math.Cos
// and math.Sin, so the fixture inputs are the same bits on every platform.
func decomposeCases() []decomposeCase {
	return []decomposeCase{
		{
			name:   "rect_north_south",
			area:   rectArea,
			tactic: Tactic{Pattern: domain.PatternParallelLanes, Orientation: domain.OrientationNorthSouth, Lanes: 4, OverlapPct: 10},
			params: Params{SwathM: 240, AltM: 120},
			convex: true,
		},
		{
			// A near-rectangle about 1600 m by 700 m, tilted about 30 degrees.
			name: "rotated_long_axis",
			area: func(t *testing.T) domain.Area {
				return mustGrid(t, "rotated", polyMetres(
					[2]float64{350, 0}, [2]float64{1750, 800}, [2]float64{1400, 1400}, [2]float64{0, 600},
				), testCellM)
			},
			tactic: Tactic{Pattern: domain.PatternParallelLanes, Orientation: domain.OrientationLongAxis, Lanes: 4, OverlapPct: 10},
			params: Params{SwathM: 240, AltM: 120},
			convex: true,
		},
		{
			// A U open to the north: east-west passes over the arms cross the
			// notch, so they split into runs and the turns need route repair.
			name: "concave_u_shape",
			area: func(t *testing.T) domain.Area {
				return mustGrid(t, "u_shape", polyMetres(
					[2]float64{0, 0}, [2]float64{1000, 0}, [2]float64{1000, 1000}, [2]float64{700, 1000},
					[2]float64{700, 300}, [2]float64{300, 300}, [2]float64{300, 1000}, [2]float64{0, 1000},
				), testCellM)
			},
			tactic: Tactic{Pattern: domain.PatternParallelLanes, Orientation: domain.OrientationEastWest, Lanes: 3, OverlapPct: 0},
			params: Params{SwathM: 200, AltM: 90},
		},
	}
}

// fixture is what a golden file holds: the inputs next to the output, so a
// reviewer reading a diff sees what produced it.
type fixture struct {
	Area   string        `json:"area"`
	Tactic Tactic        `json:"tactic"`
	Params Params        `json:"params"`
	Lanes  []domain.Lane `json:"lanes"`
}

func TestDecomposeGolden(t *testing.T) {
	for _, tc := range decomposeCases() {
		t.Run(tc.name, func(t *testing.T) {
			area := tc.area(t)
			lanes, err := Decompose(area, tc.tactic, tc.params)
			if err != nil {
				t.Fatalf("Decompose: %v", err)
			}
			got := append(eventlog.MustCanonical(fixture{
				Area: area.Name, Tactic: tc.tactic, Params: tc.params, Lanes: lanes,
			}), '\n')

			path := filepath.Join("testdata", "decompose_"+tc.name+".golden.json")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v (run with -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("decomposition differs from %s\n  reproduce: go test ./internal/coverage -run TestDecomposeGolden/%s -count=1\n  regenerate after an intended change: go test ./internal/coverage -run TestDecomposeGolden -update",
					path, tc.name)
			}
		})
	}
}

// every decision-path package asserts identical output
// across repeated runs.
func TestDecomposeDeterminism(t *testing.T) {
	for _, tc := range decomposeCases() {
		t.Run(tc.name, func(t *testing.T) {
			area := tc.area(t)
			first, err := Decompose(area, tc.tactic, tc.params)
			if err != nil {
				t.Fatalf("Decompose: %v", err)
			}
			want := eventlog.MustHashOf(first)
			for i := 1; i < 100; i++ {
				again, err := Decompose(tc.area(t), tc.tactic, tc.params)
				if err != nil {
					t.Fatalf("repetition %d: %v", i, err)
				}
				if got := eventlog.MustHashOf(again); got != want {
					t.Fatalf("repetition %d: hash %s, want %s", i, got, want)
				}
			}
		})
	}
}

func TestDecomposeProperties(t *testing.T) {
	for _, tc := range decomposeCases() {
		t.Run(tc.name, func(t *testing.T) {
			area := tc.area(t)
			g := area.Grid
			lanes, err := Decompose(area, tc.tactic, tc.params)
			if err != nil {
				t.Fatalf("Decompose: %v", err)
			}
			if len(lanes) != tc.tactic.Lanes {
				t.Fatalf("%d lanes, want %d", len(lanes), tc.tactic.Lanes)
			}

			// The lanes partition the AO: I1 at plan time.
			owner := map[domain.CellID]int{}
			for i, l := range lanes {
				if l.Index != i || l.ID != laneID(i) || l.Assigned() {
					t.Fatalf("lane %d: index %d id %s assigned %q", i, l.Index, l.ID, l.AssignedTo)
				}
				for j, id := range l.Cells {
					if j > 0 && l.Cells[j-1] >= id {
						t.Fatalf("lane %d: cells not a sorted set at %d", i, j)
					}
					if !g.IsInAO(id) {
						t.Fatalf("lane %d: cell %d outside the AO", i, id)
					}
					if prev, dup := owner[id]; dup {
						t.Fatalf("cell %d owned by lanes %d and %d", id, prev, i)
					}
					owner[id] = i
				}
			}
			if len(owner) != countInAO(g) {
				t.Fatalf("lanes own %d cells, the AO has %d", len(owner), countInAO(g))
			}

			for i, l := range lanes {
				if len(l.Waypoints) == 0 {
					t.Fatalf("lane %d has no waypoint", i)
				}
				var prev vec
				for j, wp := range l.Waypoints {
					p := toLocal(g, wp)
					id, ok := g.CellOf(wp)
					if !ok || !g.IsInAO(id) || wp.AltM != tc.params.AltM {
						t.Fatalf("lane %d waypoint %d is not in an in-AO cell at the plan altitude: %+v", i, j, wp)
					}
					if !segmentClear(g, p, p) {
						t.Fatalf("lane %d waypoint %d is closer than %v m to a cell outside the AO", i, j, Clearance(g))
					}
					if j > 0 && !segmentClear(g, prev, p) {
						t.Fatalf("lane %d segment %d comes closer than %v m to a cell outside the AO", i, j, Clearance(g))
					}
					prev = p
				}
			}

			// Consecutive lanes start in opposite senses.
			f := frameFor(t, area, tc.tactic.Orientation)
			for i, l := range lanes {
				if len(l.Waypoints) < 2 {
					continue
				}
				_, v0 := f.uv(toLocal(g, l.Waypoints[0]))
				_, v1 := f.uv(toLocal(g, l.Waypoints[1]))
				if forward := v1 > v0; forward != (i%2 == 0) {
					t.Fatalf("lane %d starts forward=%v, want %v", i, forward, i%2 == 0)
				}
			}

			// Every cell a lane owns lies within half a swath of its path, so
			// a vector whose footprint is half the swath explores the whole
			// lane by flying it: the swath is honoured, not approximated.
			for i, l := range lanes {
				for _, id := range l.Cells {
					if d := distToPath(g, cellLocal(g, id), l.Waypoints); d > tc.params.SwathM/2+1e-6 {
						t.Fatalf("lane %d: cell %d is %.1f m from the path, half the swath is %v m", i, id, d, tc.params.SwathM/2)
					}
				}
			}
		})
	}
}

func TestDecomposeRouteRepair(t *testing.T) {
	tc := decomposeCases()[2]
	area := tc.area(t)
	g := area.Grid
	r := newRouter(g)

	f := frameFor(t, area, tc.tactic.Orientation)
	cells := projectCells(g, f, inAOCells(g))
	raw := sweep(g, f, cells, 0, 1000, tc.params.SwathM, true)
	short := false
	for i := 1; i < len(raw); i++ {
		if !segmentClear(g, raw[i-1], raw[i]) {
			short = true
		}
	}
	if !short {
		t.Fatal("fixture no longer exercises repair: no raw segment comes too close to the outside of the U")
	}
	repaired, ok := r.repair(raw)
	if !ok {
		t.Fatal("repair failed on a connected AO")
	}
	for i := 1; i < len(repaired); i++ {
		if !segmentClear(g, repaired[i-1], repaired[i]) {
			t.Fatalf("repaired segment %d still comes too close to the outside", i)
		}
	}
}

func TestDecomposeErrors(t *testing.T) {
	area := rectArea(t)
	ok := Tactic{Pattern: domain.PatternParallelLanes, Orientation: domain.OrientationNorthSouth, Lanes: 4, OverlapPct: 10}
	params := Params{SwathM: 240, AltM: 120}

	with := func(mod func(*Tactic)) Tactic {
		t := ok
		mod(&t)
		return t
	}
	cases := []struct {
		name   string
		tactic Tactic
		params Params
		want   error
	}{
		{"spiral", with(func(t *Tactic) { t.Pattern = domain.PatternSpiral }), params, ErrUnsupportedPattern},
		{"perimeter", with(func(t *Tactic) { t.Pattern = domain.PatternPerimeter }), params, ErrUnsupportedPattern},
		{"unknown pattern", with(func(t *Tactic) { t.Pattern = "zigzag" }), params, ErrInvalidTactic},
		{"unknown orientation", with(func(t *Tactic) { t.Orientation = "diagonal" }), params, ErrInvalidTactic},
		{"zero lanes", with(func(t *Tactic) { t.Lanes = 0 }), params, ErrInvalidTactic},
		{"seventeen lanes", with(func(t *Tactic) { t.Lanes = 17 }), params, ErrInvalidTactic},
		{"negative overlap", with(func(t *Tactic) { t.OverlapPct = -1 }), params, ErrInvalidTactic},
		{"overlap 51", with(func(t *Tactic) { t.OverlapPct = 51 }), params, ErrInvalidTactic},
		{"zero swath", ok, Params{SwathM: 0, AltM: 120}, ErrInvalidTactic},
		{"nan altitude", ok, Params{SwathM: 240, AltM: math.NaN()}, ErrInvalidTactic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decompose(area, tc.tactic, tc.params)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
		})
	}

	// ExpandablePatterns is what the planner is offered, so it must be exactly
	// the patterns Decompose does not refuse as unsupported.
	for _, p := range []domain.Pattern{domain.PatternParallelLanes, domain.PatternSpiral, domain.PatternPerimeter} {
		_, err := Decompose(area, with(func(t *Tactic) { t.Pattern = p }), params)
		if listed := slices.Contains(ExpandablePatterns(), p); listed == errors.Is(err, ErrUnsupportedPattern) {
			t.Errorf("pattern %s: listed expandable %v, Decompose error %v", p, listed, err)
		}
	}

	// 16 strips across a 500 m square are 31 m wide against 50 m cells, so
	// some strip holds no cell centre, and the error names it.
	square := mustGrid(t, "square", polyMetres(
		[2]float64{0, 0}, [2]float64{500, 0}, [2]float64{500, 500}, [2]float64{0, 500},
	), testCellM)
	var empty *EmptyLaneError
	_, err := Decompose(square, with(func(t *Tactic) { t.Lanes = 16 }), params)
	if !errors.Is(err, ErrEmptyLane) || !errors.As(err, &empty) {
		t.Fatalf("err %v, want an EmptyLaneError", err)
	}
}

func TestSweepFrameCanonical(t *testing.T) {
	for _, dir := range []vec{{0, 1}, {0, -1}} {
		f := newSweepFrame(dir)
		if f.a != (vec{0, 1}) || f.n != (vec{1, 0}) {
			t.Fatalf("direction %+v: frame %+v, want a north, n east", dir, f)
		}
	}
	for _, dir := range []vec{{1, 0}, {-1, 0}} {
		f := newSweepFrame(dir)
		if f.a != (vec{1, 0}) || f.n != (vec{0, 1}) {
			t.Fatalf("direction %+v: frame %+v, want a east, n north", dir, f)
		}
	}
}

func TestLongAxis(t *testing.T) {
	// A 400 m by 100 m rectangle: the long side runs east.
	rect := []vec{{0, 0}, {400, 0}, {400, 100}, {0, 100}}
	if a := newSweepFrame(longAxis(rect)).a; a != (vec{1, 0}) {
		t.Fatalf("long axis %+v, want east", a)
	}
	// The same rectangle stood on end runs north, whatever the vertex order.
	tall := []vec{{100, 400}, {0, 0}, {0, 400}, {100, 0}}
	if a := newSweepFrame(longAxis(tall)).a; a != (vec{0, 1}) {
		t.Fatalf("long axis %+v, want north", a)
	}
	// A 3-4-5 slope: collinear points give their line.
	line := []vec{{0, 0}, {3, 4}, {6, 8}}
	if a := newSweepFrame(longAxis(line)).a; math.Abs(a.x-0.6) > 1e-12 || math.Abs(a.y-0.8) > 1e-12 {
		t.Fatalf("long axis %+v, want (0.6, 0.8)", a)
	}
	if a := longAxis([]vec{{5, 5}}); a != north {
		t.Fatalf("single point axis %+v, want north", a)
	}
}

func frameFor(t *testing.T, area domain.Area, o domain.Orientation) sweepFrame {
	t.Helper()
	ring := make([]vec, len(area.Polygon.Ring))
	for i, v := range area.Polygon.Ring {
		ring[i] = toLocal(area.Grid, v)
	}
	axis, ok := orientationAxis(o, ring)
	if !ok {
		t.Fatalf("orientation %q", o)
	}
	return newSweepFrame(axis)
}

// distToPath is the distance in metres from p to the nearest segment of a
// waypoint path, in the local frame.
func distToPath(g domain.Grid, p vec, wps []domain.Position) float64 {
	best := math.Inf(1)
	for i := range wps {
		a := toLocal(g, wps[i])
		b := a
		if i+1 < len(wps) {
			b = toLocal(g, wps[i+1])
		}
		best = math.Min(best, distToSegment(p, a, b))
	}
	return best
}

func distToSegment(p, a, b vec) float64 {
	d := b.sub(a)
	l2 := dot(d, d)
	if l2 == 0 {
		return norm(p.sub(a))
	}
	s := math.Max(0, math.Min(1, dot(p.sub(a), d)/l2))
	return norm(p.sub(vec{a.x + d.x*s, a.y + d.y*s}))
}
