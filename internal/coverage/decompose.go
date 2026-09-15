package coverage

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/dsanchez31/keel/internal/domain"
)

// Tactic is what the planner chose: four scalars and nothing that can hold a
// coordinate. Decompose turns it into geometry.
type Tactic struct {
	Pattern     domain.Pattern     `json:"pattern"`
	Orientation domain.Orientation `json:"orientation"`
	Lanes       int                `json:"lanes"`
	OverlapPct  int                `json:"overlap_pct"`
}

// Params is the geometry the tactic does not carry, supplied by the caller
// from the fleet rather than by the model.
type Params struct {
	// SwathM is the maximum spacing between two adjacent passes of a lane,
	// in metres. The caller derives it from the sensor footprint of the
	// vectors that can fly the plan.
	SwathM float64 `json:"swath_m"`
	// AltM is the altitude given to every waypoint.
	AltM float64 `json:"alt_m"`
}

// Bounds on the tactic, from spec section 5.1. They hold whatever the model
// proposed.
const (
	MaxLanes      = 16
	MaxOverlapPct = 50
)

var (
	// ErrUnsupportedPattern reports a valid Plan IR pattern that has no
	// expansion. Only parallel_lanes has one (spec section 5.5).
	ErrUnsupportedPattern = errors.New("coverage: pattern has no expansion")
	// ErrInvalidTactic reports a tactic or parameters outside their bounds.
	ErrInvalidTactic = errors.New("coverage: invalid tactic")
	// ErrEmptyLane is the sentinel an EmptyLaneError unwraps to.
	ErrEmptyLane = errors.New("coverage: lane contains no cell")
)

// EmptyLaneError reports a strip of the AO that holds no cell centre, so the
// lane it would become has nothing to cover. It names the lane so the
// feasibility gate can say which one.
type EmptyLaneError struct{ Index int }

func (e *EmptyLaneError) Error() string {
	return fmt.Sprintf("coverage: lane %d contains no cell", e.Index)
}

func (e *EmptyLaneError) Unwrap() error { return ErrEmptyLane }

// runGapCells is the gap along a pass, in cells, beyond which two cells are
// in separate runs. Inside one run consecutive cells are never further apart
// than a cell diagonal (√2 cells), whatever the sweep angle.
const runGapCells = 1.5

// Decompose expands a parallel_lanes tactic over an AO into lanes.
//
// It is deterministic and LLM free: the same area, tactic and parameters give
// byte-identical lanes on every run. Following spec section 5.5:
//
//  1. The lane direction comes from the orientation, long_axis taking the
//     longer side of the AO's minimum-area bounding rectangle.
//  2. The AO is projected across the lanes and the span is cut into equal
//     strips. A cell belongs to the lowest-index strip containing its centre,
//     so the lanes partition the AO and no cell has two owners (I1).
//  3. Overlap widens the extent a lane's passes sweep, never its ownership.
//  4. Each lane is swept by parallel passes no further apart than SwathM,
//     alternating direction, with even lanes starting in the forward sense
//     and odd lanes in the reverse one. A pass runs along its band's centre
//     line, and the corner cells a slanted edge leaves out of reach are
//     waypoints of their own, so every cell of the lane lies within half a
//     swath of its path.
//
// Every waypoint lies in an in-AO cell, and every segment between two
// waypoints keeps a quarter cell from every cell outside the AO.
func Decompose(area domain.Area, t Tactic, p Params) ([]domain.Lane, error) {
	if err := validate(t, p); err != nil {
		return nil, err
	}
	g := area.Grid
	if g.Count() == 0 || len(area.Polygon.Ring) < 3 {
		return nil, fmt.Errorf("%w: area %q has no raster", ErrInvalidArea, area.Name)
	}

	ring := make([]vec, len(area.Polygon.Ring))
	for i, v := range area.Polygon.Ring {
		ring[i] = toLocal(g, v)
	}
	axis, _ := orientationAxis(t.Orientation, ring)
	f := newSweepFrame(axis)

	umin, umax := math.Inf(1), math.Inf(-1)
	for _, v := range ring {
		u := dot(v, f.n)
		umin, umax = math.Min(umin, u), math.Max(umax, u)
	}
	w := (umax - umin) / float64(t.Lanes)
	if !(w > 0) {
		return nil, fmt.Errorf("%w: area %q has no width across the lanes", ErrInvalidArea, area.Name)
	}
	half := w * float64(t.OverlapPct) / 200

	cells := projectCells(g, f, inAOCells(g))
	owned := make([][]domain.CellID, t.Lanes)
	for _, c := range cells {
		i := int(math.Ceil((c.u-umin)/w)) - 1
		i = max(0, min(t.Lanes-1, i))
		owned[i] = append(owned[i], c.id)
	}

	r := newRouter(g)
	lanes := make([]domain.Lane, 0, t.Lanes)
	for i := range t.Lanes {
		if len(owned[i]) == 0 {
			return nil, &EmptyLaneError{Index: i}
		}
		lo := umin + float64(float64(i)*w)
		hi := umin + float64(float64(i+1)*w)
		lo, hi = math.Max(umin, lo-half), math.Min(umax, hi+half)

		var band []uvCell
		for _, c := range cells {
			if c.u >= lo && c.u <= hi {
				band = append(band, c)
			}
		}
		l, err := buildLane(r, f, i, owned[i], band, lo, hi, p)
		if err != nil {
			return nil, err
		}
		lanes = append(lanes, l)
	}
	return lanes, nil
}

// ExpandablePatterns lists the patterns Decompose expands, sorted. The Plan IR
// accepts others (spec section 5.1), which fail with ErrUnsupportedPattern; the
// planner is offered only these (planir.ModelSchema).
func ExpandablePatterns() []domain.Pattern {
	return []domain.Pattern{domain.PatternParallelLanes}
}

func validate(t Tactic, p Params) error {
	switch t.Pattern {
	case domain.PatternParallelLanes:
	case domain.PatternSpiral, domain.PatternPerimeter:
		return fmt.Errorf("%w: %s", ErrUnsupportedPattern, t.Pattern)
	default:
		return fmt.Errorf("%w: unknown pattern %q", ErrInvalidTactic, t.Pattern)
	}
	if _, ok := orientationAxis(t.Orientation, nil); !ok {
		return fmt.Errorf("%w: unknown orientation %q", ErrInvalidTactic, t.Orientation)
	}
	if t.Lanes < 1 || t.Lanes > MaxLanes {
		return fmt.Errorf("%w: lanes %d outside 1..%d", ErrInvalidTactic, t.Lanes, MaxLanes)
	}
	if t.OverlapPct < 0 || t.OverlapPct > MaxOverlapPct {
		return fmt.Errorf("%w: overlap %d%% outside 0..%d", ErrInvalidTactic, t.OverlapPct, MaxOverlapPct)
	}
	return validateParams(p)
}

func validateParams(p Params) error {
	if !(p.SwathM > 0) || math.IsInf(p.SwathM, 0) {
		return fmt.Errorf("%w: swath %v m is not a finite positive number", ErrInvalidTactic, p.SwathM)
	}
	if math.IsNaN(p.AltM) || math.IsInf(p.AltM, 0) {
		return fmt.Errorf("%w: altitude %v m is not finite", ErrInvalidTactic, p.AltM)
	}
	return nil
}

// uvCell is a cell with its coordinates in a sweep frame.
type uvCell struct {
	id   domain.CellID
	u, v float64
}

func inAOCells(g domain.Grid) []domain.CellID {
	var out []domain.CellID
	for i, in := range g.InAO {
		if in {
			out = append(out, domain.CellID(i))
		}
	}
	return out
}

func projectCells(g domain.Grid, f sweepFrame, ids []domain.CellID) []uvCell {
	out := make([]uvCell, len(ids))
	for i, id := range ids {
		u, v := f.uv(cellLocal(g, id))
		out[i] = uvCell{id: id, u: u, v: v}
	}
	return out
}

// buildLane assembles one lane: its owned cells as a sorted set, and the
// boustrophedon path over the band of cells its passes sweep. The one lane
// builder serves Decompose and Redecompose, in the frame f they cut in.
//
// A lane always visits the centre of at least one cell it owns: when the
// passes need none, the owned cell nearest the end of the path is appended.
// That is what lets a redecomposition promise progress (spec section 6.5.1):
// a vector flying the lane explores that cell whatever else it misses.
func buildLane(r *router, f sweepFrame, index int, owned []domain.CellID, band []uvCell, lo, hi float64, p Params) (domain.Lane, error) {
	raw := sweep(r.g, f, band, lo, hi, p.SwathM, index%2 == 0)
	if !visitsCentre(r.g, raw, owned) && len(raw) > 0 && len(owned) > 0 {
		end := raw[len(raw)-1]
		best := owned[0]
		for _, id := range owned[1:] {
			if domain.CompareCost(norm(cellLocal(r.g, id).sub(end)), norm(cellLocal(r.g, best).sub(end))) < 0 {
				best = id
			}
		}
		raw = append(raw, cellLocal(r.g, best))
	}
	pts, ok := r.repair(raw)
	if !ok {
		return domain.Lane{}, fmt.Errorf("%w: lane %d crosses between disconnected parts of the AO", ErrDisconnectedAO, index)
	}
	wps := make([]domain.Position, len(pts))
	for i, pt := range pts {
		wps[i] = fromLocal(r.g, pt)
		wps[i].AltM = p.AltM
	}
	l := domain.Lane{
		ID:        laneID(index),
		Index:     index,
		Cells:     slices.Clone(owned),
		Waypoints: wps,
	}
	l.SortCells()
	return l, nil
}

// visitsCentre reports whether a path passes through the centre of one of
// the cells.
func visitsCentre(g domain.Grid, path []vec, cells []domain.CellID) bool {
	for _, pt := range path {
		id, ok := localCell(g, pt)
		if !ok || cellLocal(g, id) != pt {
			continue
		}
		if slices.Contains(cells, id) {
			return true
		}
	}
	return false
}

func laneID(index int) domain.LaneID { return domain.LaneID(fmt.Sprintf("lane-%02d", index)) }

// sweep returns the waypoints, in the local frame, of a boustrophedon over
// band, whose across-lane coordinates lie in [lo, hi].
//
// The extent is cut into the fewest equal bands no wider than swathM, one pass
// each. The cells of a band, ordered along the lane by v then CellID, split
// into runs wherever consecutive cells are more than runGapCells apart, which
// is where a pass crosses a concavity. Each run is swept by runPath. Passes
// alternate direction, the first one running forward (increasing v) when
// forward is set.
func sweep(g domain.Grid, f sweepFrame, band []uvCell, lo, hi, swathM float64, forward bool) []vec {
	width := hi - lo
	k := 1
	if width > 0 {
		k = max(1, int(math.Ceil(width/swathM)))
	}
	bw := width / float64(k)

	passes := make([][]uvCell, k)
	for _, c := range band {
		j := 0
		if bw > 0 {
			j = max(0, min(k-1, int(math.Floor((c.u-lo)/bw))))
		}
		passes[j] = append(passes[j], c)
	}

	gap := runGapCells * g.CellM
	var out []vec
	dir := forward
	for j, pass := range passes {
		if len(pass) == 0 {
			continue
		}
		slices.SortFunc(pass, func(a, b uvCell) int {
			if c := cmp.Compare(a.v, b.v); c != 0 {
				return c
			}
			return cmp.Compare(a.id, b.id)
		})
		centre := lo + float64((float64(j)+0.5)*bw)

		var runs [][]vec
		start := 0
		for i := 1; i <= len(pass); i++ {
			if i < len(pass) && pass[i].v-pass[i-1].v <= gap {
				continue
			}
			runs = append(runs, runPath(g, f, pass[start:i], centre, swathM/2))
			start = i
		}
		if !dir {
			slices.Reverse(runs)
		}
		for _, run := range runs {
			if !dir {
				slices.Reverse(run)
			}
			out = append(out, run...)
		}
		dir = !dir
	}
	return out
}

// coverTolM absorbs the last-bit difference between two computations of the
// same distance, so a cell exactly half a swath from its pass counts as
// covered.
const coverTolM = 1e-6

// pathPoint is a waypoint of a run with its along-lane coordinate.
type pathPoint struct {
	p vec
	v float64
}

// runPath returns the waypoints of one run, in increasing v, such that every
// cell of the run lies within radius of the path.
//
// The pass runs along the band's centre line, across-lane coordinate centre:
// every cell of the band is within half the band's width of it, which is at
// most half the swath. The line is walked from the run's first cell to its
// last in steps of an eighth of a cell, and every stretch at least one cell
// long along which it keeps the route clearance becomes a leg of the pass,
// entered and left at its ends. A straight line made of clear pieces is
// clear, so the legs need no repair, and repair only ever bridges a real gap
// between two of them. A run whose centre line has no such stretch, too
// narrow or too ragged, is swept from its first cell to its last instead.
//
// Where the AO boundary crosses the band at a slant, the cells before the
// first leg and after the last one fall outside that reach. They are covered
// by adding cells as waypoints, the one farthest from the path first (lowest
// CellID among equals), each inserted at its place along the lane, until no
// cell of the run is farther than radius. Every addition is a cell centre
// then on the path, so the loop ends after at most one addition per cell.
func runPath(g domain.Grid, f sweepFrame, run []uvCell, centre, radius float64) []vec {
	first := run[0]
	last := len(run) - 1
	for last > 0 && run[last-1].v == run[len(run)-1].v {
		last--
	}
	lastCell := run[last]

	pts := clearLegs(g, f, centre, first.v, lastCell.v)
	if len(pts) == 0 {
		pts = append(pts, pathPoint{cellLocal(g, first.id), first.v})
		if lastCell.id != first.id {
			pts = append(pts, pathPoint{cellLocal(g, lastCell.id), lastCell.v})
		}
	}

	for {
		worst, worstD := -1, radius+coverTolM
		for i, c := range run {
			d := distToPoints(cellLocal(g, c.id), pts)
			if d > worstD || (d == worstD && worst >= 0 && c.id < run[worst].id) {
				worst, worstD = i, d
			}
		}
		if worst < 0 {
			break
		}
		c := run[worst]
		at := len(pts)
		for i, q := range pts {
			if q.v > c.v {
				at = i
				break
			}
		}
		pts = slices.Insert(pts, at, pathPoint{cellLocal(g, c.id), c.v})
	}

	out := make([]vec, len(pts))
	for i, q := range pts {
		out[i] = q.p
	}
	return out
}

// clearLegs walks the centre line at across-lane u from along-lane from to
// to, in steps of an eighth of a cell, and returns the two ends of every
// stretch at least one cell long that keeps the route clearance, in order.
func clearLegs(g domain.Grid, f sweepFrame, u, from, to float64) []pathPoint {
	step := g.CellM / 8
	minLeg := g.CellM
	var legs []pathPoint
	var start, prev pathPoint
	open := false
	closeLeg := func() {
		if open && prev.v-start.v >= minLeg {
			legs = append(legs, start, prev)
		}
		open = false
	}
	for s := 0; ; s++ {
		v := from + float64(float64(s)*step)
		if v > to {
			v = to
		}
		pt := pathPoint{f.point(u, v), v}
		switch {
		case open && segmentClear(g, prev.p, pt.p):
			prev = pt
		case segmentClear(g, pt.p, pt.p):
			closeLeg()
			start, prev, open = pt, pt, true
		default:
			closeLeg()
		}
		if v == to {
			break
		}
	}
	closeLeg()
	return legs
}

// distToPoints is the distance from p to the polyline through pts.
func distToPoints(p vec, pts []pathPoint) float64 {
	best := math.Inf(1)
	for i := range pts {
		b := pts[i].p
		if i+1 < len(pts) {
			b = pts[i+1].p
		}
		best = math.Min(best, pointSegmentDistance(p, pts[i].p, b))
	}
	return best
}
