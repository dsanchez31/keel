package world

import (
	"math"

	"github.com/dsanchez31/keel/internal/domain"
)

// Kinematics.
//
// Deliberately simple (design section 10.1): no wind, no attitude, no
// acceleration. An aerial vehicle flies straight at its cruise speed and
// climbs or descends at most at the scenario's climb rate, both at once. A
// ground vehicle drives at its cruise speed along a route over the terrain,
// on the ground. Arrival is exact: a vehicle that would overshoot its target
// stops on it, so a vehicle parked on a waypoint stays there rather than
// oscillating around it.
//
// Every step is a function of the position, the target and the parameters,
// computed in a local planar frame at the vehicle's latitude, with each
// product feeding a sum rounded explicitly. The simulator is outside the
// decision path, but a DST seed replays only if the world it drives replays
// too.

// move is the outcome of one kinematic step.
type move struct {
	pos     domain.Position
	heading float64 // degrees, the direction of horizontal travel
	speed   float64 // m/s, horizontal
	movedM  float64 // 3D distance covered, what the battery pays for
	arrived bool
}

// stepAerial moves from p toward target for dtS seconds. heading is the
// current heading, kept when the step is purely vertical.
func stepAerial(p, target domain.Position, heading, cruiseMps, climbMps, dtS float64) move {
	east, north := enu(p, target)
	horiz := math.Hypot(east, north)
	dz := target.AltM - p.AltM

	reach := float64(cruiseMps * dtS)
	climb := float64(climbMps * dtS)
	m := move{heading: headingOf(east, north, heading)}

	next := p
	h := horiz
	if horiz <= reach {
		next.Lat, next.Lon = target.Lat, target.Lon
	} else {
		f := reach / horiz
		next = offset(p, float64(east*f), float64(north*f))
		h = reach
	}
	v := math.Abs(dz)
	if v <= climb {
		next.AltM = target.AltM
	} else {
		v = climb
		next.AltM = p.AltM + math.Copysign(climb, dz)
	}
	m.pos = next
	m.speed = h / dtS
	m.movedM = math.Hypot(h, v)
	m.arrived = horiz <= reach && math.Abs(dz) <= climb
	return m
}

// stepGround drives from p along route for dtS seconds, on the ground. It
// returns the move and the route still ahead; the route is exhausted on
// arrival.
func stepGround(p domain.Position, route []domain.Position, cruiseMps, dtS float64) (move, []domain.Position) {
	budget := float64(cruiseMps * dtS)
	m := move{pos: p}
	m.pos.AltM = 0
	var travelled float64
	for len(route) > 0 && budget > 0 {
		east, north := enu(m.pos, route[0])
		d := math.Hypot(east, north)
		m.heading = headingOf(east, north, m.heading)
		if d <= budget {
			m.pos = domain.Position{Lat: route[0].Lat, Lon: route[0].Lon}
			budget -= d
			travelled += d
			route = route[1:]
			continue
		}
		f := budget / d
		m.pos = offset(m.pos, float64(east*f), float64(north*f))
		m.pos.AltM = 0
		travelled += budget
		budget = 0
	}
	m.speed = travelled / dtS
	m.movedM = travelled
	m.arrived = len(route) == 0
	return m, route
}

// headingOf is the bearing of an east, north displacement, keeping the
// previous heading for a displacement of zero.
func headingOf(east, north, prev float64) float64 {
	if east == 0 && north == 0 {
		return prev
	}
	return domain.NormaliseDeg(math.Atan2(east, north) * 180 / math.Pi)
}
