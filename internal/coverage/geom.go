package coverage

import (
	"cmp"
	"math"
	"slices"

	"github.com/dsanchez31/keel/internal/domain"
)

// The local planar frame.
//
// Decomposition works in metres east (x) and north (y) of the grid origin,
// with the same equirectangular projection at the reference latitude that the
// grid itself is built with. A cell centre is then exactly
// ((col+0.5)·CellM, (row+0.5)·CellM), with no trigonometry in it, and the
// only transcendental call in the whole of decomposition is the cosine that
// fixes metres per degree of longitude.
//
// Every product that feeds a sum is wrapped in float64(...) so no platform
// fuses it into an FMA (see internal/domain/geo.go).

// vec is a point or a direction in the local frame, in metres.
type vec struct{ x, y float64 }

func (a vec) sub(b vec) vec { return vec{a.x - b.x, a.y - b.y} }

func (a vec) neg() vec { return vec{-a.x, -a.y} }

func dot(a, b vec) float64 { return float64(a.x*b.x) + float64(a.y*b.y) }

// cross is the z component of (a − o) × (b − o): positive when o, a, b turn
// counter-clockwise.
func cross(o, a, b vec) float64 {
	return float64((a.x-o.x)*(b.y-o.y)) - float64((a.y-o.y)*(b.x-o.x))
}

// norm is the Euclidean length. math.Sqrt is correctly rounded by IEEE 754 on
// every platform, unlike the trigonometric functions.
func norm(a vec) float64 { return math.Sqrt(dot(a, a)) }

// unit scales a to length 1. The zero vector has no direction and is
// returned unchanged; callers never pass one.
func unit(a vec) vec {
	l := norm(a)
	if l == 0 {
		return a
	}
	return vec{a.x / l, a.y / l}
}

// toLocal projects a WGS84 position into the grid's local frame.
func toLocal(g domain.Grid, p domain.Position) vec {
	return vec{
		x: (p.Lon - g.Origin.Lon) * domain.MetresPerDegreeLon(g.RefLat),
		y: (p.Lat - g.Origin.Lat) * domain.MetresPerDegreeLat(),
	}
}

// fromLocal is the WGS84 position, at altitude zero, of a point of the local
// frame. For a cell centre it computes exactly what Grid.Centre does, so a
// waypoint on a cell centre is the same bits whichever way it was derived.
func fromLocal(g domain.Grid, p vec) domain.Position {
	return domain.Position{
		Lat: g.Origin.Lat + p.y/domain.MetresPerDegreeLat(),
		Lon: g.Origin.Lon + p.x/domain.MetresPerDegreeLon(g.RefLat),
	}
}

// cellLocal is the centre of a cell in the local frame.
func cellLocal(g domain.Grid, id domain.CellID) vec {
	row, col := g.RowCol(id)
	return vec{(float64(col) + 0.5) * g.CellM, (float64(row) + 0.5) * g.CellM}
}

// sweepFrame is the pair of directions a decomposition works in: a runs
// along the lanes, n runs across them. Lane 0 has the lowest u = p·n.
type sweepFrame struct{ a, n vec }

var north = vec{0, 1}

// newSweepFrame canonicalises a lane direction. The sign of a direction is
// meaningless (a lane running north is a lane running south), so it is fixed
// here: a points north, or due east when horizontal, and n is the
// perpendicular pointing east, or due north when vertical. Two callers that
// derive the same line from different edges then build the same frame.
func newSweepFrame(dir vec) sweepFrame {
	a := unit(dir)
	if a.y < 0 || (a.y == 0 && a.x < 0) {
		a = a.neg()
	}
	n := vec{a.y, -a.x}
	if n.x < 0 || (n.x == 0 && n.y < 0) {
		n = n.neg()
	}
	return sweepFrame{a: a, n: n}
}

// uv returns the across-lane (u) and along-lane (v) coordinates of p.
func (f sweepFrame) uv(p vec) (u, v float64) { return dot(p, f.n), dot(p, f.a) }

// point is the inverse of uv: the point of the local frame at across-lane u
// and along-lane v. The frame is orthonormal, so no division is involved.
func (f sweepFrame) point(u, v float64) vec {
	return vec{float64(u*f.n.x) + float64(v*f.a.x), float64(u*f.n.y) + float64(v*f.a.y)}
}

// convexHull returns the convex hull of pts in counter-clockwise order,
// without collinear points and without repeating the first vertex (Andrew's
// monotone chain). Points are sorted by x then y first, so the result does not
// depend on the input order. Fewer than three distinct non-collinear points
// yield the distinct extreme points only.
func convexHull(pts []vec) []vec {
	p := slices.Clone(pts)
	slices.SortFunc(p, func(a, b vec) int {
		if c := cmp.Compare(a.x, b.x); c != 0 {
			return c
		}
		return cmp.Compare(a.y, b.y)
	})
	p = slices.Compact(p)
	if len(p) < 3 {
		return p
	}
	hull := make([]vec, 0, 2*len(p))
	for _, q := range p {
		for len(hull) >= 2 && cross(hull[len(hull)-2], hull[len(hull)-1], q) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, q)
	}
	lower := len(hull) + 1
	for i := len(p) - 2; i >= 0; i-- {
		q := p[i]
		for len(hull) >= lower && cross(hull[len(hull)-2], hull[len(hull)-1], q) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, q)
	}
	hull = hull[:len(hull)-1]
	if len(hull) < 3 {
		// Every point was collinear: keep the two extremes.
		return []vec{p[0], p[len(p)-1]}
	}
	return hull
}

// longAxis is the direction of the longer side of the minimum-area rectangle
// enclosing pts, found by rotating a rectangle onto each hull edge in turn.
//
// Areas are compared with the cost epsilon and ties keep the lowest hull edge
// index; a square keeps the edge direction. Collinear points give the line
// they lie on, and a single point, having no axis, gives north.
func longAxis(pts []vec) vec {
	hull := convexHull(pts)
	switch len(hull) {
	case 0, 1:
		return north
	case 2:
		return hull[1].sub(hull[0])
	}

	bestArea := math.Inf(1)
	var best vec
	for i := range hull {
		e := unit(hull[(i+1)%len(hull)].sub(hull[i]))
		n := vec{-e.y, e.x}
		minE, maxE := math.Inf(1), math.Inf(-1)
		minN, maxN := math.Inf(1), math.Inf(-1)
		for _, q := range hull {
			pe, pn := dot(q, e), dot(q, n)
			minE, maxE = math.Min(minE, pe), math.Max(maxE, pe)
			minN, maxN = math.Min(minN, pn), math.Max(maxN, pn)
		}
		lenE, lenN := maxE-minE, maxN-minN
		if domain.CompareCost(lenE*lenN, bestArea) >= 0 {
			continue
		}
		bestArea = lenE * lenN
		if lenE >= lenN {
			best = e
		} else {
			best = n
		}
	}
	return best
}

// orientationAxis resolves a plan orientation to a lane direction over the
// AO ring, already projected into the local frame.
func orientationAxis(o domain.Orientation, ring []vec) (vec, bool) {
	switch o {
	case domain.OrientationNorthSouth:
		return north, true
	case domain.OrientationEastWest:
		return vec{1, 0}, true
	case domain.OrientationLongAxis:
		return longAxis(ring), true
	default:
		return vec{}, false
	}
}
