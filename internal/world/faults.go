package world

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"

	"github.com/dsanchez31/keel/internal/domain"
)

// Fault injection.
//
// A fault changes what a vehicle does or what it says, never what the engine
// decides: the engine learns of it from the telemetry that follows, or from
// the fault_injected event the driver records beside the injection so replay
// reproduces the fault itself (spec section 4.5). A fault with a duration
// ends by itself; a duration of zero lasts for the rest of the run.
//
//   - kill: the vehicle is lost. No frame leaves it and no command reaches it.
//   - link_loss: the radio link is cut both ways.
//   - battery_drain: Magnitude percent of charge is gone at once.
//   - gps_drift: the reported position walks away from the truth at Magnitude
//     metres per second, in a direction drawn when the fault starts. When the
//     fault ends the receiver recovers and the offset is gone.
//   - stale_telemetry: the vehicle keeps sending the last frame it sent
//     before the fault, unchanged, timestamp included. The engine sees a link
//     that answers and says nothing new, and ages it.

// ErrUnknownVector reports a fault on a vector the world does not simulate.
var ErrUnknownVector = errors.New("world: unknown vector")

// ErrInvalidFault reports a fault the world cannot apply.
var ErrInvalidFault = errors.New("world: invalid fault")

// forever marks a fault with no end.
const forever = math.MaxInt64

type faultState struct {
	killed bool

	linkCutFrom, linkCutUntil int64

	driftFrom, driftUntil int64
	driftMps              float64
	driftE, driftN        float64 // unit direction

	staleFrom, staleUntil int64
	lastFrame             *domain.VectorState // the last frame sent, the one a stale link repeats
	staleFrame            *domain.VectorState
}

func until(nowMs, durationMs int64) int64 {
	if durationMs <= 0 {
		return forever
	}
	return nowMs + durationMs
}

// inject applies f at mission time nowMs. World.Inject has held it against
// Fault.Check already.
func (fs *faultState) inject(f domain.Fault, nowMs int64, bat *battery, rng *rand.Rand) error {
	switch f.Kind {
	case domain.FaultKill:
		fs.killed = true
	case domain.FaultLinkLoss:
		fs.linkCutFrom, fs.linkCutUntil = nowMs, until(nowMs, f.DurationMs)
	case domain.FaultBatteryDrain:
		bat.drain(f.Magnitude)
	case domain.FaultGPSDrift:
		fs.driftFrom, fs.driftUntil, fs.driftMps = nowMs, until(nowMs, f.DurationMs), f.Magnitude
		fs.driftE, fs.driftN = direction(rng)
	case domain.FaultStaleTelemetry:
		fs.staleFrom, fs.staleUntil = nowMs, until(nowMs, f.DurationMs)
		fs.staleFrame = nil
		if fs.lastFrame != nil {
			frozen := *fs.lastFrame
			fs.staleFrame = &frozen
		}
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidFault, f.Kind)
	}
	return nil
}

// linkCut reports whether a link_loss fault holds at nowMs.
func (fs *faultState) linkCut(nowMs int64) bool {
	return nowMs >= fs.linkCutFrom && nowMs < fs.linkCutUntil
}

// drift returns the gps_drift offset at nowMs, in metres east and north.
func (fs *faultState) drift(nowMs int64) (east, north float64) {
	if fs.driftMps == 0 || nowMs < fs.driftFrom || nowMs >= fs.driftUntil {
		return 0, 0
	}
	d := float64(fs.driftMps * float64(nowMs-fs.driftFrom) / 1000)
	return float64(d * fs.driftE), float64(d * fs.driftN)
}

// stale returns the frame a stale link repeats at nowMs, if one does. A
// stale fault injected before the vehicle ever sent a frame freezes the
// first frame it sends.
func (fs *faultState) stale(nowMs int64) (domain.VectorState, bool) {
	if nowMs < fs.staleFrom || nowMs >= fs.staleUntil || fs.staleUntil == 0 {
		return domain.VectorState{}, false
	}
	if fs.staleFrame == nil {
		return domain.VectorState{}, false
	}
	return *fs.staleFrame, true
}

// sent records the frame a vehicle just sent, fresh or repeated.
func (fs *faultState) sent(st domain.VectorState, nowMs int64) {
	fs.lastFrame = &st
	if fs.staleFrame == nil && nowMs >= fs.staleFrom && nowMs < fs.staleUntil {
		frozen := st
		fs.staleFrame = &frozen
	}
}

// direction draws a unit vector uniformly on the circle, by rejection from
// the square: no trigonometry, and a square root is correctly rounded on
// every platform.
func direction(rng *rand.Rand) (east, north float64) {
	for {
		x, y := rng.Float64()*2-1, rng.Float64()*2-1
		r2 := float64(x*x) + float64(y*y)
		if r2 > 1e-12 && r2 <= 1 {
			r := math.Sqrt(r2)
			return x / r, y / r
		}
	}
}
