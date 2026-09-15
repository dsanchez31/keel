package coverage

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/eventlog"
)

// The phase 2 definition of done, second half: redecomposition after a
// simulated loss covers every previously uncovered cell exactly once.

const testSensorRadiusM = 120

var redecomposeParams = Params{SwathM: 240, AltM: 120}

// missionAtLoss decomposes the test rectangle into four assigned lanes, flies
// each vector along its lane for the given fraction of the path length,
// painting its footprint, and returns the grid and the lanes at that moment.
func missionAtLoss(t *testing.T, fraction float64) (domain.Grid, []domain.Lane) {
	t.Helper()
	area := rectArea(t)
	lanes, err := Decompose(area, Tactic{
		Pattern: domain.PatternParallelLanes, Orientation: domain.OrientationNorthSouth, Lanes: 4, OverlapPct: 10,
	}, redecomposeParams)
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	g := area.Grid.Clone()
	for i := range lanes {
		lanes[i].AssignedTo = domain.VectorID(fmt.Sprintf("DRONE-%02d", i+1))
		fly(g, lanes[i].Waypoints, fraction*lanes[i].LengthM())
	}
	return g, lanes
}

// fly paints the sensor footprint every 10 m along a waypoint path, up to
// budgetM metres of it.
func fly(g domain.Grid, wps []domain.Position, budgetM float64) {
	const stepM = 10
	flown := 0.0
	for i := 1; i < len(wps) && flown < budgetM; i++ {
		a, b := wps[i-1], wps[i]
		segM := domain.HaversineM(a, b)
		for s := 0.0; s <= segM && flown < budgetM; s += stepM {
			f := s / segM
			Paint(g, domain.Position{Lat: a.Lat + (b.Lat-a.Lat)*f, Lon: a.Lon + (b.Lon-a.Lon)*f}, testSensorRadiusM)
			flown += stepM
		}
	}
}

// release is the release_lanes action as the engine performs it: the lane
// loses its owner and keeps its cells.
func release(lanes []domain.Lane, v domain.VectorID) []domain.Lane {
	out := domain.CloneLanes(lanes)
	for i := range out {
		if out[i].AssignedTo == v {
			out[i].AssignedTo = ""
		}
	}
	return out
}

func TestRedecomposeAfterLoss(t *testing.T) {
	g, lanes := missionAtLoss(t, 0.25)
	before := domain.CloneLanes(lanes)
	lanes = release(lanes, "DRONE-02")

	res, err := Redecompose(g, lanes, 3, 0, redecomposeParams)
	if err != nil {
		t.Fatalf("Redecompose: %v", err)
	}

	wantPool := uncoveredOf(g, before[1].Cells)
	if len(wantPool) == 0 {
		t.Fatal("fixture explored all of DRONE-02's lane, nothing to redistribute")
	}
	if !slices.Equal(res.Pool, wantPool) {
		t.Fatalf("pool %d cells, want the %d uncovered cells of lane-01", len(res.Pool), len(wantPool))
	}
	if !slices.Equal(res.Retired, []domain.LaneID{"lane-01"}) {
		t.Fatalf("retired %v, want [lane-01]", res.Retired)
	}
	wantKept := []domain.Lane{before[0], before[2], before[3]}
	if !reflect.DeepEqual(res.Kept, wantKept) {
		t.Fatal("assigned lanes changed: a redecomposition must leave working vectors alone")
	}

	if len(res.New) != 3 {
		t.Fatalf("%d new lanes, want 3", len(res.New))
	}
	owner := map[domain.CellID]domain.LaneID{}
	for _, l := range res.Kept {
		for _, id := range l.Cells {
			owner[id] = l.ID
		}
	}
	sizes := make([]int, 0, len(res.New))
	for i, l := range res.New {
		if l.Index != 4+i || l.ID != laneID(4+i) || l.Assigned() {
			t.Fatalf("new lane %d: index %d id %s assigned %q, want index %d unassigned", i, l.Index, l.ID, l.AssignedTo, 4+i)
		}
		if len(l.Cells) == 0 {
			t.Fatalf("new lane %s is empty", l.ID)
		}
		sizes = append(sizes, len(l.Cells))
		for _, id := range l.Cells {
			if prev, dup := owner[id]; dup {
				t.Fatalf("cell %d owned by %s and %s", id, prev, l.ID)
			}
			if _, pooled := slices.BinarySearch(res.Pool, id); !pooled {
				t.Fatalf("new lane %s owns cell %d, which was not pooled", l.ID, id)
			}
			owner[id] = l.ID
		}

		visitsPool := false
		var prev vec
		for j, wp := range l.Waypoints {
			p := toLocal(g, wp)
			id, ok := g.CellOf(wp)
			if !ok || !g.IsInAO(id) || !segmentClear(g, p, p) {
				t.Fatalf("new lane %s waypoint %d is not in an in-AO cell with the route clearance", l.ID, j)
			}
			if j > 0 && !segmentClear(g, prev, p) {
				t.Fatalf("new lane %s segment %d comes closer than the clearance to the outside", l.ID, j)
			}
			prev = p
			if _, pooled := slices.BinarySearch(l.Cells, id); pooled && g.Centre(id) == (domain.Position{Lat: wp.Lat, Lon: wp.Lon}) {
				visitsPool = true
			}
		}
		for _, id := range l.Cells {
			if d := distToPath(g, cellLocal(g, id), l.Waypoints); d > redecomposeParams.SwathM/2+1e-6 {
				t.Fatalf("new lane %s: pooled cell %d is %.1f m from the path, half the swath is %v m", l.ID, id, d, redecomposeParams.SwathM/2)
			}
		}
		if !visitsPool {
			t.Fatalf("new lane %s visits no pooled cell centre: the round would make no progress", l.ID)
		}
	}
	if slices.Max(sizes)-slices.Min(sizes) > 1 {
		t.Fatalf("new lane sizes %v are not balanced", sizes)
	}

	// Every previously uncovered cell is now owned exactly once, by a kept
	// lane or a new one.
	for _, id := range Uncovered(g) {
		if _, ok := owner[id]; !ok {
			t.Fatalf("uncovered cell %d has no owner after redecomposition", id)
		}
	}
	for _, id := range res.Pool {
		n := 0
		for _, l := range res.New {
			if _, ok := slices.BinarySearch(l.Cells, id); ok {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("pooled cell %d is in %d new lanes, want exactly 1", id, n)
		}
	}
}

func TestRedecomposeTwoLosses(t *testing.T) {
	g, lanes := missionAtLoss(t, 0.3)
	lanes = release(release(lanes, "DRONE-01"), "DRONE-04")
	res, err := Redecompose(g, lanes, 2, 0, redecomposeParams)
	if err != nil {
		t.Fatalf("Redecompose: %v", err)
	}
	want := append(uncoveredOf(g, lanes[0].Cells), uncoveredOf(g, lanes[3].Cells)...)
	slices.Sort(want)
	if !slices.Equal(res.Pool, want) {
		t.Fatalf("pool %d cells, want %d", len(res.Pool), len(want))
	}
	if !slices.Equal(res.Retired, []domain.LaneID{"lane-00", "lane-03"}) {
		t.Fatalf("retired %v", res.Retired)
	}
	var union []domain.CellID
	for _, l := range res.New {
		union = append(union, l.Cells...)
	}
	slices.Sort(union)
	if !slices.Equal(union, res.Pool) {
		t.Fatal("new lanes do not partition the pool")
	}
}

func TestRedecomposeEdges(t *testing.T) {
	g, lanes := missionAtLoss(t, 0.25)
	lanes = release(lanes, "DRONE-02")

	if _, err := Redecompose(g, lanes, 0, 0, redecomposeParams); !errors.Is(err, ErrNoEligibleVector) {
		t.Fatalf("k=0: err %v, want ErrNoEligibleVector", err)
	}
	if _, err := Redecompose(g, lanes, 3, 0, Params{}); !errors.Is(err, ErrInvalidTactic) {
		t.Fatalf("zero swath: err %v, want ErrInvalidTactic", err)
	}

	// More vectors than pooled cells: one lane per cell, no empty lane.
	small := domain.CloneLanes(lanes)
	pool := uncoveredOf(g, small[1].Cells)
	small[1].Cells = pool[:2]
	res, err := Redecompose(g, small, 5, 0, redecomposeParams)
	if err != nil {
		t.Fatalf("Redecompose: %v", err)
	}
	if len(res.New) != 2 {
		t.Fatalf("%d new lanes for 2 pooled cells, want 2", len(res.New))
	}
	for _, l := range res.New {
		if len(l.Cells) != 1 || len(l.Waypoints) != 1 {
			t.Fatalf("lane %s: %d cells %d waypoints, want 1 and 1", l.ID, len(l.Cells), len(l.Waypoints))
		}
	}

	// A released lane that was already fully explored is retired and
	// replaced by nothing.
	done := domain.CloneLanes(lanes)
	gDone := g.Clone()
	for _, id := range done[1].Cells {
		gDone.Explored[id] = true
	}
	res, err = Redecompose(gDone, done, 0, 0, redecomposeParams)
	if err != nil {
		t.Fatalf("fully explored lane: %v", err)
	}
	if len(res.New) != 0 || len(res.Pool) != 0 || !slices.Equal(res.Retired, []domain.LaneID{"lane-01"}) {
		t.Fatalf("fully explored lane: new %d pool %d retired %v", len(res.New), len(res.Pool), res.Retired)
	}
}

// The index floor is what keeps a lane id unique for a whole mission. Lanes
// 04 to 08 existed once and were retired, so the caller's floor is 9: the new
// lanes start there, not after the highest index still present. A floor
// below the highest present index changes nothing.
func TestRedecomposeNeverReusesAnIndex(t *testing.T) {
	g, lanes := missionAtLoss(t, 0.25)
	lanes = release(lanes, "DRONE-02")

	res, err := Redecompose(g, lanes, 3, 9, redecomposeParams)
	if err != nil {
		t.Fatalf("Redecompose: %v", err)
	}
	for i, l := range res.New {
		if l.Index != 9+i || l.ID != laneID(9+i) {
			t.Fatalf("new lane %d: index %d id %s, want %d", i, l.Index, l.ID, 9+i)
		}
	}

	res, err = Redecompose(g, lanes, 3, 2, redecomposeParams)
	if err != nil {
		t.Fatalf("Redecompose: %v", err)
	}
	if res.New[0].Index != 4 {
		t.Fatalf("first new index %d with a floor below the lanes, want 4", res.New[0].Index)
	}
}

func TestRedecomposeDeterminism(t *testing.T) {
	g, lanes := missionAtLoss(t, 0.25)
	lanes = release(lanes, "DRONE-02")
	first, err := Redecompose(g, lanes, 3, 0, redecomposeParams)
	if err != nil {
		t.Fatalf("Redecompose: %v", err)
	}
	want := eventlog.MustHashOf(first)
	for i := 1; i < 100; i++ {
		again, err := Redecompose(g, lanes, 3, 0, redecomposeParams)
		if err != nil {
			t.Fatalf("repetition %d: %v", i, err)
		}
		if got := eventlog.MustHashOf(again); got != want {
			t.Fatalf("repetition %d: hash %s, want %s", i, got, want)
		}
	}
}
