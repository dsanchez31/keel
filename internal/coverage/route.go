package coverage

import (
	"errors"
	"fmt"
	"math"

	"github.com/dsanchez31/keel/internal/domain"
)

// Route clearance and repair.
//
// The geofence is checked at grid resolution: a vector scanning in a cell
// outside the AO has breached it (spec section 6.2, within). A vector never
// flies its route exactly. It turns toward the next waypoint once within the
// arrival radius of the current one, cutting the corner, and the position it
// reports carries GPS error. A route that merely stays over in-AO cells, or
// grazes the corner of a cell outside, is then one metre of deviation away
// from a breach, which sends a healthy vector home.
//
// So every segment of a route keeps a clearance of a quarter cell from every
// cell outside the AO, checked exactly: the distance from the segment to each
// such cell's square, not samples along it. A quarter cell is the most a cell
// centre on the boundary of the AO can guarantee (half a cell to the edge of
// its own cell) with room to spare, and it leaves the engine's arrival radius
// and the position error a known budget (spec section 5.5 step 7). A segment
// short of the clearance is replaced by a shortest path over in-AO cell
// centres, whose steps keep at least half a cell, shortened again by line of
// sight under the same check.

// clearanceFraction is the route clearance as a fraction of the cell edge.
const clearanceFraction = 0.25

// Clearance is the distance in metres every route over g keeps from every
// cell outside the AO.
func Clearance(g domain.Grid) float64 { return float64(g.CellM * clearanceFraction) }

// ErrClearance reports a grid whose route clearance does not cover how far a
// vehicle strays from its route: the geofence would trip on the system's own
// routes.
var ErrClearance = errors.New("coverage: the route clearance does not cover the arrival budget")

// ErrArrival reports an arrival budget that cannot be checked: a radius that
// is not positive, an error that is negative, either not finite.
var ErrArrival = errors.New("coverage: invalid arrival budget")

// Arrival is how far a vehicle strays from a route: it turns for the next
// waypoint within RadiusM of the current one, the engine's arrival radius,
// and reports its position within ErrorM, the position error the engine
// budgets for.
type Arrival struct {
	RadiusM float64
	ErrorM  float64
}

// Check reports whether routes over g keep room for a. The clearance must
// exceed the radius plus the error (spec section 5.5 step 7). Gate 3 and the
// engine at plan approval both hold a plan to this one rule.
func (a Arrival) Check(g domain.Grid) error {
	if !(a.RadiusM > 0) || math.IsInf(a.RadiusM, 0) || !(a.ErrorM >= 0) || math.IsInf(a.ErrorM, 0) {
		return fmt.Errorf("%w: arrival radius %v m, position error %v m", ErrArrival, a.RadiusM, a.ErrorM)
	}
	c, budget := Clearance(g), a.RadiusM+a.ErrorM
	if !(c > budget) {
		return fmt.Errorf("%w: routes over %v m cells keep %v m from the outside of the AO, which does not exceed the arrival radius (%v m) plus the position error (%v m)",
			ErrClearance, g.CellM, c, a.RadiusM, a.ErrorM)
	}
	return nil
}

// localCell is the cell containing a point of the local frame.
func localCell(g domain.Grid, p vec) (domain.CellID, bool) {
	col := int(math.Floor(p.x / g.CellM))
	row := int(math.Floor(p.y / g.CellM))
	if row < 0 || col < 0 || row >= g.Rows || col >= g.Cols {
		return 0, false
	}
	return g.CellAt(row, col), true
}

// segmentClear reports whether every point of the segment from a to b is at
// least the clearance away from every cell outside the AO, the space beyond
// the raster counting as outside. A point is the segment from itself to
// itself.
func segmentClear(g domain.Grid, a, b vec) bool {
	c := Clearance(g)
	row0 := int(math.Floor((math.Min(a.y, b.y) - c) / g.CellM))
	row1 := int(math.Floor((math.Max(a.y, b.y) + c) / g.CellM))
	col0 := int(math.Floor((math.Min(a.x, b.x) - c) / g.CellM))
	col1 := int(math.Floor((math.Max(a.x, b.x) + c) / g.CellM))
	for row := row0; row <= row1; row++ {
		for col := col0; col <= col1; col++ {
			if row >= 0 && col >= 0 && row < g.Rows && col < g.Cols && g.InAO[g.CellAt(row, col)] {
				continue
			}
			lo := vec{float64(col) * g.CellM, float64(row) * g.CellM}
			hi := vec{lo.x + g.CellM, lo.y + g.CellM}
			if segmentBoxDistance(a, b, lo, hi) < c {
				return false
			}
		}
	}
	return true
}

// segmentBoxDistance is the distance between the segment from a to b and the
// closed axis-aligned box [lo, hi]: zero when they meet, otherwise attained
// at an end of the segment or at a corner of the box, both being convex.
func segmentBoxDistance(a, b, lo, hi vec) float64 {
	if segmentHitsBox(a, b, lo, hi) {
		return 0
	}
	d := math.Min(pointBoxDistance(a, lo, hi), pointBoxDistance(b, lo, hi))
	for _, k := range [4]vec{lo, {hi.x, lo.y}, hi, {lo.x, hi.y}} {
		d = math.Min(d, pointSegmentDistance(k, a, b))
	}
	return d
}

// segmentHitsBox clips the segment against the box (Liang and Barsky). A
// segment touching the box's boundary hits it.
func segmentHitsBox(a, b, lo, hi vec) bool {
	d := b.sub(a)
	t0, t1 := 0.0, 1.0
	for _, pq := range [4][2]float64{
		{-d.x, a.x - lo.x}, {d.x, hi.x - a.x},
		{-d.y, a.y - lo.y}, {d.y, hi.y - a.y},
	} {
		p, q := pq[0], pq[1]
		if p == 0 {
			if q < 0 {
				return false
			}
			continue
		}
		r := q / p
		if p < 0 {
			if r > t1 {
				return false
			}
			t0 = math.Max(t0, r)
		} else {
			if r < t0 {
				return false
			}
			t1 = math.Min(t1, r)
		}
	}
	return true
}

func pointBoxDistance(p, lo, hi vec) float64 {
	dx := math.Max(0, math.Max(lo.x-p.x, p.x-hi.x))
	dy := math.Max(0, math.Max(lo.y-p.y, p.y-hi.y))
	return norm(vec{dx, dy})
}

// pointSegmentDistance is the distance from p to the segment from a to b.
func pointSegmentDistance(p, a, b vec) float64 {
	d := b.sub(a)
	l2 := dot(d, d)
	if l2 == 0 {
		return norm(p.sub(a))
	}
	s := math.Max(0, math.Min(1, dot(p.sub(a), d)/l2))
	return norm(p.sub(vec{a.x + float64(d.x*s), a.y + float64(d.y*s)}))
}

// neighbours8 is the fixed BFS neighbour order as (row, column) offsets:
// north, east, south, west, then north-east, south-east, south-west,
// north-west. A fixed order makes the first shortest path found the same one
// on every run.
var neighbours8 = [8][2]int{
	{1, 0}, {0, 1}, {-1, 0}, {0, -1},
	{1, 1}, {-1, 1}, {-1, -1}, {1, -1},
}

// router holds the BFS buffers for one decomposition, so repairing many
// segments over a large raster does not allocate a raster-sized array each
// time. It is local to one call, never shared.
type router struct {
	g       domain.Grid
	parent  []int32
	touched []domain.CellID
}

func newRouter(g domain.Grid) *router {
	parent := make([]int32, g.Count())
	for i := range parent {
		parent[i] = -1
	}
	return &router{g: g, parent: parent}
}

// path returns a shortest 8-connected path of in-AO cells from a to b, both
// ends included, or nil when b is unreachable. A diagonal step is allowed only
// when both orthogonal cells it passes between are in the AO, so every step
// between two centres keeps at least half a cell from any cell outside.
func (r *router) path(a, b domain.CellID) []domain.CellID {
	g := r.g
	if !g.IsInAO(a) || !g.IsInAO(b) {
		return nil
	}
	defer r.reset()
	r.visit(a, a)
	queue := []domain.CellID{a}
	for len(queue) > 0 && r.parent[b] < 0 {
		id := queue[0]
		queue = queue[1:]
		row, col := g.RowCol(id)
		for _, d := range neighbours8 {
			nr, nc := row+d[0], col+d[1]
			if !r.open(nr, nc) {
				continue
			}
			if d[0] != 0 && d[1] != 0 && (!r.open(row+d[0], col) || !r.open(row, col+d[1])) {
				continue
			}
			n := g.CellAt(nr, nc)
			if r.parent[n] >= 0 {
				continue
			}
			r.visit(n, id)
			queue = append(queue, n)
		}
	}
	if r.parent[b] < 0 {
		return nil
	}
	var rev []domain.CellID
	for id := b; ; id = domain.CellID(r.parent[id]) {
		rev = append(rev, id)
		if id == a {
			break
		}
	}
	out := make([]domain.CellID, len(rev))
	for i, id := range rev {
		out[len(rev)-1-i] = id
	}
	return out
}

func (r *router) open(row, col int) bool {
	g := r.g
	return row >= 0 && col >= 0 && row < g.Rows && col < g.Cols && g.InAO[g.CellAt(row, col)]
}

func (r *router) visit(id, from domain.CellID) {
	r.parent[id] = int32(from)
	r.touched = append(r.touched, id)
}

func (r *router) reset() {
	for _, id := range r.touched {
		r.parent[id] = -1
	}
	r.touched = r.touched[:0]
}

// repair returns the route through wps with every segment short of the
// clearance replaced by a detour: from the point, over the centres of a
// shortest cell path, to the next point, shortened by line of sight.
// Consecutive duplicates are dropped. It fails only when a point lies outside
// the AO or two points are not connected over in-AO cells, which a grid built
// by NewGrid and waypoints taken from its cells rule out.
func (r *router) repair(wps []vec) ([]vec, bool) {
	out := make([]vec, 0, len(wps))
	for _, p := range wps {
		if len(out) > 0 && out[len(out)-1] == p {
			continue
		}
		if len(out) == 0 || segmentClear(r.g, out[len(out)-1], p) {
			out = append(out, p)
			continue
		}
		from := out[len(out)-1]
		ca, oka := localCell(r.g, from)
		cb, okb := localCell(r.g, p)
		if !oka || !okb {
			return nil, false
		}
		cells := r.path(ca, cb)
		if cells == nil {
			return nil, false
		}
		detour := []vec{from}
		for _, id := range cells {
			if c := cellLocal(r.g, id); c != detour[len(detour)-1] {
				detour = append(detour, c)
			}
		}
		if p != detour[len(detour)-1] {
			detour = append(detour, p)
		}
		out = append(out, pull(r.g, detour)[1:]...)
	}
	return out, true
}

// pull shortens a path by line of sight: from each kept point it jumps to the
// farthest following point reachable by a clear segment, extending greedily
// until the first segment that is not. The ends are always kept.
func pull(g domain.Grid, path []vec) []vec {
	if len(path) <= 2 {
		return path
	}
	out := []vec{path[0]}
	i := 0
	for i < len(path)-1 {
		j := i + 1
		for j+1 < len(path) && segmentClear(g, path[i], path[j+1]) {
			j++
		}
		out = append(out, path[j])
		i = j
	}
	return out
}
