package daemon

import (
	"context"
	"fmt"
	"math"

	"github.com/dsanchez31/keel/internal/pacer"
	"github.com/dsanchez31/keel/internal/transport"
)

// The pace (spec section 16.5).
//
// keeld's pacer decides when a tick runs, never what it holds, so the pace
// changes no decision the log records and a replay ignores it. What it must
// not do is outrun the world: a simulator left slower than the engine flies
// its vehicles slower than their cruise speed in mission time, and their
// frames read as silence; one left faster flies them faster. So the whole
// closed loop changes pace together, and only a fleet every vector of which
// is simulated changes it at all.

// Clock is keeld's pace.
func (d *Daemon) Clock(context.Context) (transport.ClockView, error) {
	return d.clockView(), nil
}

// SetClock sets the pace of keeld and of every simulator of its fleet, from
// the next tick on, speed already checked against pacer.CheckLiveSpeed.
// Raising it, the simulators go first; lowering it, the pacer does: the engine
// never runs ahead of the world. A simulator failing leaves the pace as it
// was.
func (d *Daemon) SetClock(ctx context.Context, speed int) (transport.ClockView, error) {
	d.clockMu.Lock()
	defer d.clockMu.Unlock()
	p, fixed := d.scalable()
	if speed != 1 && fixed != "" {
		return transport.ClockView{}, fmt.Errorf("%w: %s", transport.ErrConflict, fixed)
	}
	if p == nil {
		// Not paced in real time, and asked for real time: nothing to do.
		return d.clockView(), nil
	}
	from := int(math.Round(p.Speed()))
	if speed == from {
		return d.clockView(), nil
	}
	if speed > from {
		if err := d.fleet.SetSimSpeed(ctx, speed); err != nil {
			return transport.ClockView{}, fmt.Errorf("%w: %v", transport.ErrUpstream, err)
		}
		if err := p.SetSpeed(float64(speed)); err != nil {
			return transport.ClockView{}, err
		}
	} else {
		if err := p.SetSpeed(float64(speed)); err != nil {
			return transport.ClockView{}, err
		}
		if err := d.fleet.SetSimSpeed(ctx, speed); err != nil {
			_ = p.SetSpeed(float64(from))
			return transport.ClockView{}, fmt.Errorf("%w: %v", transport.ErrUpstream, err)
		}
	}
	d.log.Info("pace set", "speed", speed, "was", from)
	return d.clockView(), nil
}

// scalable is the pacer when it can change pace, and why the pace stays real
// time otherwise: a loop not paced in real time, or a vector no simulator
// runs.
func (d *Daemon) scalable() (pacer.Scalable, string) {
	p, ok := d.pacer.(pacer.Scalable)
	if !ok {
		return nil, "keeld is not paced in real time"
	}
	for _, b := range d.cfg.Vectors {
		if !b.TimeScalable() {
			return p, fmt.Sprintf("%s is not simulated: keeld keeps real time while a real vehicle is bound", b.ID)
		}
	}
	return p, ""
}

// speed is the pace in force, 1 for a loop not paced in real time.
func (d *Daemon) speed() int {
	if p, ok := d.pacer.(pacer.Scalable); ok {
		return max(1, int(math.Round(p.Speed())))
	}
	return 1
}

func (d *Daemon) clockView() transport.ClockView {
	_, fixed := d.scalable()
	return transport.ClockView{Speed: d.speed(), MaxSpeed: pacer.MaxLiveSpeed, Fixed: fixed}
}
