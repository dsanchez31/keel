package world

import (
	"math"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
)

// at is a position east and north of a fixed origin, in metres, at altM.
func at(eastM, northM, altM float64) domain.Position {
	p := offset(domain.Position{Lat: 45, Lon: 5}, eastM, northM)
	p.AltM = altM
	return p
}

// An aerial vehicle climbs and flies at once, each capped, and lands exactly
// on its target when the slower of the two is done.
func TestAerialClimbsAndFliesToTheTarget(t *testing.T) {
	p, target := at(0, 0, 0), at(0, 1000, 120)
	var ticks int
	var moved float64
	for ; ticks < 10000; ticks++ {
		m := stepAerial(p, target, 0, 20, 5, 0.1)
		if m.speed > 20+1e-9 {
			t.Fatalf("tick %d: horizontal speed %v above cruise", ticks, m.speed)
		}
		if math.Abs(m.pos.AltM-p.AltM) > 0.5+1e-9 {
			t.Fatalf("tick %d: climbed %v m in one tick, cap is 0.5", ticks, m.pos.AltM-p.AltM)
		}
		moved += m.movedM
		p = m.pos
		if m.arrived {
			break
		}
	}
	// 1000 m at 20 m/s is 50 s; the 120 m climb at 5 m/s takes 24 s and
	// happens during it. Five hundred steps through degrees can leave a
	// residue of nanometres, which costs one more tick.
	if n := ticks + 1; n != 500 && n != 501 {
		t.Fatalf("arrived after %d ticks, want 500", n)
	}
	if p != target {
		t.Fatalf("stopped at %v, want exactly %v", p, target)
	}
	if want := math.Hypot(1000, 0) + 0; moved < want || moved > want+120 {
		t.Fatalf("paid for %v m, want between the horizontal 1000 m and the slant path", moved)
	}
}

func TestAerialHoldsHeadingInAPureClimb(t *testing.T) {
	m := stepAerial(at(0, 0, 0), at(0, 0, 50), 123, 20, 5, 0.1)
	if m.heading != 123 || m.speed != 0 {
		t.Fatalf("heading %v speed %v, want 123 and 0", m.heading, m.speed)
	}
}

func TestGroundFollowsItsRouteOnTheGround(t *testing.T) {
	route := []domain.Position{at(100, 0, 50), at(100, 100, 50)}
	p := at(0, 0, 10)
	var ticks int
	for ticks = 1; ticks < 1000; ticks++ {
		var m move
		m, route = stepGround(p, route, 5, 0.1)
		if m.pos.AltM != 0 {
			t.Fatalf("ground vehicle at altitude %v", m.pos.AltM)
		}
		if m.speed > 5+1e-9 {
			t.Fatalf("speed %v above cruise", m.speed)
		}
		p = m.pos
		if m.arrived {
			break
		}
	}
	// 200 m at 5 m/s, a corner included: 40 s, give or take the residue of
	// stepping through degrees.
	if ticks != 400 && ticks != 401 {
		t.Fatalf("arrived after %d ticks, want 400", ticks)
	}
	if want := at(100, 100, 0); math.Abs(p.Lat-want.Lat) > 1e-12 || math.Abs(p.Lon-want.Lon) > 1e-12 {
		t.Fatalf("stopped at %v, want %v", p, want)
	}
}
