package world

import "math"

// Battery model.
//
// Distance is what a battery pays for, on the same terms the allocator
// reasons in (spec section 5.7): MaxRangeM is the distance a vehicle covers
// at cruise on a full battery, so covering d metres costs d / MaxRangeM of
// the charge, and staying aloft while doing so is already in that figure. An
// airborne vehicle holding in place, covering no distance at all, pays the
// scenario's hover drain per minute instead; a vehicle on the ground pays
// nothing standing still. The charge is kept as a float so that a tick's
// tiny drain accumulates, and reported as its floor, the integer percentage
// of spec section 3.

// airborneAltM is the altitude above which an aerial vehicle is flying
// rather than standing on its pad.
const airborneAltM = 0.5

type battery struct {
	pct float64
}

// spend charges a tick: movedM metres covered, dtS seconds elapsed, airborne
// or not. Hovering is charged only for a tick in which the vehicle covered
// no ground.
func (b *battery) spend(movedM, maxRangeM float64, airborne bool, hoverPctPerMin, dtS float64) {
	if maxRangeM > 0 {
		b.pct -= movedM * 100 / maxRangeM
	}
	if airborne && movedM == 0 {
		b.pct -= hoverPctPerMin * dtS / 60
	}
	b.pct = math.Max(b.pct, 0)
}

// drain removes pct percent at once, the battery_drain fault.
func (b *battery) drain(pct float64) { b.pct = math.Max(b.pct-pct, 0) }

// empty reports a battery with nothing left: the vehicle is down.
func (b battery) empty() bool { return b.pct <= 0 }

// reported is the charge as telemetry carries it, an integer in [0, 100].
func (b battery) reported() int {
	return int(math.Min(math.Max(math.Floor(b.pct), 0), 100))
}
