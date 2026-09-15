// Package domain holds the shared vocabulary of the decision path.
//
// It sits below internal/engine, internal/coverage, internal/assign and
// internal/doctrine so that all four can speak the same types without an
// import cycle. internal/engine/types.go re-exports everything here as
// aliases, so engine.VectorState and domain.VectorState are one type.
//
// The package is pure: no I/O, no clock, no concurrency, no map iteration in
// anything it exposes. Every slice that represents a set is documented as
// sorted, and is sorted at construction rather than at encode time.
package domain

import "math"

// EarthRadiusM is the mean Earth radius used by every distance computation in
// the decision path. A single constant, because two of them silently disagree.
const EarthRadiusM = 6371008.8

// Position is a WGS84 point. Latitude and longitude are decimal degrees,
// altitude is metres above the WGS84 ellipsoid.
type Position struct {
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
	AltM float64 `json:"alt_m"`
}

// Finite reports whether every component is a real number. NaN and infinity
// are bugs, not values: the canonical encoder refuses them.
func (p Position) Finite() bool {
	return !math.IsNaN(p.Lat) && !math.IsInf(p.Lat, 0) &&
		!math.IsNaN(p.Lon) && !math.IsInf(p.Lon, 0) &&
		!math.IsNaN(p.AltM) && !math.IsInf(p.AltM, 0)
}

// Polygon is a closed WGS84 ring. The first vertex is not repeated at the end;
// closure is implicit.
type Polygon struct {
	Ring []Position `json:"ring"`
}

// Contains reports whether p lies inside the polygon, by ray casting on the
// lon/lat plane. Deterministic: no tolerance, no early exit on ordering.
//
// The planar approximation is adequate at the scale of an AO (kilometres) and
// is the same approximation the grid itself uses.
func (poly Polygon) Contains(p Position) bool {
	n := len(poly.Ring)
	if n < 3 {
		return false
	}
	inside := false
	j := n - 1
	for i := range n {
		vi, vj := poly.Ring[i], poly.Ring[j]
		if (vi.Lat > p.Lat) != (vj.Lat > p.Lat) {
			x := (vj.Lon-vi.Lon)*(p.Lat-vi.Lat)/(vj.Lat-vi.Lat) + vi.Lon
			if p.Lon < x {
				inside = !inside
			}
		}
		j = i
	}
	return inside
}

// BBox is the axis-aligned bounding box of the ring in degrees.
func (poly Polygon) BBox() (minLat, minLon, maxLat, maxLon float64) {
	if len(poly.Ring) == 0 {
		return 0, 0, 0, 0
	}
	minLat, maxLat = poly.Ring[0].Lat, poly.Ring[0].Lat
	minLon, maxLon = poly.Ring[0].Lon, poly.Ring[0].Lon
	for _, v := range poly.Ring[1:] {
		minLat = math.Min(minLat, v.Lat)
		maxLat = math.Max(maxLat, v.Lat)
		minLon = math.Min(minLon, v.Lon)
		maxLon = math.Max(maxLon, v.Lon)
	}
	return minLat, minLon, maxLat, maxLon
}

// Centroid is the arithmetic mean of the ring vertices. Vertices are summed in
// ring order, which is fixed, so the result is reproducible.
func (poly Polygon) Centroid() Position {
	if len(poly.Ring) == 0 {
		return Position{}
	}
	var sumLat, sumLon float64
	for _, v := range poly.Ring {
		sumLat += v.Lat
		sumLon += v.Lon
	}
	n := float64(len(poly.Ring))
	return Position{Lat: sumLat / n, Lon: sumLon / n}
}

// MetresPerDegreeLat is constant to the precision this system needs.
func MetresPerDegreeLat() float64 { return EarthRadiusM * math.Pi / 180 }

// MetresPerDegreeLon shrinks with latitude. Taken at the AO's reference
// latitude and held fixed for the whole mission, so the projection used to
// build the grid is the projection used to walk it.
func MetresPerDegreeLon(refLat float64) float64 {
	return EarthRadiusM * math.Pi / 180 * math.Cos(refLat*math.Pi/180)
}

// Fused multiply-add.
//
// The Go specification lets a compiler fuse x*y + z into a single FMA
// instruction, which rounds once where the source rounds twice. Several
// backends do fuse (arm64, ppc64le and s390x among them); amd64 does not.
// The same inputs then differ in their last bit across architectures, and
// the canonical encoder renders every bit. An explicit float64(...) conversion rounds to
// float64 and the specification forbids fusing across it, so in the decision
// path every product that feeds an addition or a subtraction is wrapped in one.
//
// This covers the code in this repository only. The transcendental functions
// of package math are implemented in Go and are compiled under the same rules,
// which is why design.md section 10.2 still scopes the cross-platform claim to
// what has been tested.

// HaversineM is the great-circle distance in metres, ignoring altitude.
func HaversineM(a, b Position) float64 {
	lat1 := a.Lat * math.Pi / 180
	lat2 := b.Lat * math.Pi / 180
	dLat := lat2 - lat1
	dLon := (b.Lon - a.Lon) * math.Pi / 180
	s := float64(math.Sin(dLat/2)*math.Sin(dLat/2)) +
		float64(math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLon/2)*math.Sin(dLon/2))
	return 2 * EarthRadiusM * math.Asin(math.Sqrt(math.Min(1, s)))
}

// BearingDeg is the initial great-circle bearing from a to b, in degrees
// clockwise from true north, normalised into [0, 360).
func BearingDeg(a, b Position) float64 {
	lat1 := a.Lat * math.Pi / 180
	lat2 := b.Lat * math.Pi / 180
	dLon := (b.Lon - a.Lon) * math.Pi / 180
	y := math.Sin(dLon) * math.Cos(lat2)
	x := float64(math.Cos(lat1)*math.Sin(lat2)) - float64(math.Sin(lat1)*math.Cos(lat2)*math.Cos(dLon))
	deg := math.Atan2(y, x) * 180 / math.Pi
	return NormaliseDeg(deg)
}

// NormaliseDeg folds an angle into [0, 360).
func NormaliseDeg(d float64) float64 {
	d = math.Mod(d, 360)
	if d < 0 {
		d += 360
	}
	return d
}
