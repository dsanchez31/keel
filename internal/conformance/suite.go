// Package conformance runs the adapter conformance suite of spec section 13
// against a vehicle behind its adapter: the nine cases a manufacturer adapter
// has to pass before the orchestrator trusts it (design section 7.1).
//
// The suite owns the transport. It opens the adapter itself through a proxy
// (proxy.go) that withholds the vehicle's telemetry for C6 and cuts the link
// for C7, so every case runs against any vehicle, simulated or real, without
// its cooperation. C3 to C5 command the vehicle to move and run only with
// motion allowed.
package conformance

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"time"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/vector"
)

// Case is one conformance case of spec section 13.
type Case string

// The nine cases.
const (
	C1 Case = "C1"
	C2 Case = "C2"
	C3 Case = "C3"
	C4 Case = "C4"
	C5 Case = "C5"
	C6 Case = "C6"
	C7 Case = "C7"
	C8 Case = "C8"
	C9 Case = "C9"
)

// names are the cases' titles in spec section 13.
var names = map[Case]string{
	C1: "Capability declaration",
	C2: "Telemetry schema",
	C3: "Command acknowledgement",
	C4: "Idempotency",
	C5: "Sequence gaps",
	C6: "Timeout",
	C7: "Reconnect",
	C8: "Capability honesty",
	C9: "Clean shutdown",
}

// Status is how a case ended.
type Status string

const (
	Pass Status = "pass"
	Fail Status = "fail"
	// Skip is a case not run: it is never a pass.
	Skip Status = "skip"
)

// Result is one case's outcome.
type Result struct {
	Case    Case
	Name    string
	Status  Status
	Detail  string
	Elapsed time.Duration
}

// Passed reports whether every case passed. A skipped case is not a pass.
func Passed(rs []Result) bool {
	return len(rs) > 0 && !slices.ContainsFunc(rs, func(r Result) bool { return r.Status != Pass })
}

// Defaults of Options, from spec section 13.
const (
	DefaultFrames          = 100
	DefaultFramesWithin    = time.Minute
	DefaultReflectWithin   = 2 * time.Second
	DefaultApplyWithin     = 30 * time.Second
	DefaultReconnectWithin = 30 * time.Second
	DefaultTolerance       = time.Second
	DefaultShutdownWithin  = 5 * time.Second
	// probeOffsetM is how far north of the vehicle C3 sends it, and
	// probeClimbM how much higher an aerial vehicle.
	probeOffsetM = 50.0
	probeClimbM  = 10.0
	pollPeriod   = 50 * time.Millisecond
)

// Options tunes a run. The zero value is the conformance run of spec section
// 13 with motion refused.
type Options struct {
	// AllowMotion lets C3, C4 and C5 command the vehicle to move. Without it
	// they are skipped: a validation must not fly a vehicle nobody agreed to
	// fly.
	AllowMotion bool
	// DegradedAfter and LostAfter are the Health thresholds C6 holds the
	// adapter to, and Tolerance how far off either may be observed. Zero
	// means spec section 7.1's 5 s and 10 s, and 1 s.
	DegradedAfter time.Duration
	LostAfter     time.Duration
	Tolerance     time.Duration
	// Frames is how many consecutive frames C2 checks, within FramesWithin.
	Frames       int
	FramesWithin time.Duration
	// ReflectWithin bounds C3, ApplyWithin the wait for an acknowledgement
	// in C4 and C5, ReconnectWithin C7 and ShutdownWithin C9.
	ReflectWithin   time.Duration
	ApplyWithin     time.Duration
	ReconnectWithin time.Duration
	ShutdownWithin  time.Duration
	// OnResult, if set, is called as each case ends.
	OnResult func(Result)
}

func (o Options) normalised() Options {
	o.DegradedAfter = cmp.Or(o.DegradedAfter, vector.DefaultDegradedAfter)
	o.LostAfter = cmp.Or(o.LostAfter, vector.DefaultLostAfter)
	o.Tolerance = cmp.Or(o.Tolerance, DefaultTolerance)
	o.Frames = cmp.Or(o.Frames, DefaultFrames)
	o.FramesWithin = cmp.Or(o.FramesWithin, DefaultFramesWithin)
	o.ReflectWithin = cmp.Or(o.ReflectWithin, DefaultReflectWithin)
	o.ApplyWithin = cmp.Or(o.ApplyWithin, DefaultApplyWithin)
	o.ReconnectWithin = cmp.Or(o.ReconnectWithin, DefaultReconnectWithin)
	o.ShutdownWithin = cmp.Or(o.ShutdownWithin, DefaultShutdownWithin)
	return o
}

// failure is a case's verdict, as opposed to an error ending the run.
type failure string

func (f failure) Error() string { return string(f) }

// fail is a case's failure, its detail the reason.
func fail(format string, args ...any) error { return failure(fmt.Sprintf(format, args...)) }

// Run opens the subject's adapter and runs the nine cases in the order C1,
// C2, C3, C4, C5, C8, C6, C7, C9: the cases that need a healthy link first,
// the ones that break it after, Close last. It returns an error only when the
// suite cannot run at all: the adapter does not open, or the context ends.
func Run(ctx context.Context, s *Subject, opts Options) ([]Result, error) {
	o := opts.normalised()
	baseline := runtime.NumGoroutine()
	v, err := s.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("conformance: opening %s: %w", s.Name, err)
	}
	r := &run{ctx: ctx, s: s, o: o, v: v, caps: v.Describe(), baseline: baseline}
	steps := []struct {
		c      Case
		motion bool
		fn     func() (string, error)
	}{
		{C1, false, r.c1}, {C2, false, r.c2}, {C3, true, r.c3}, {C4, true, r.c4}, {C5, true, r.c5},
		{C8, false, r.c8}, {C6, false, r.c6}, {C7, false, r.c7}, {C9, false, r.c9},
	}
	var out []Result
	for _, st := range steps {
		res := Result{Case: st.c, Name: names[st.c]}
		start := time.Now()
		switch {
		case st.motion && !o.AllowMotion:
			res.Status, res.Detail = Skip, "commands the vehicle to move: motion not allowed"
		default:
			detail, err := st.fn()
			var f failure
			switch {
			case err == nil:
				res.Status, res.Detail = Pass, detail
			case errors.As(err, &f):
				res.Status, res.Detail = Fail, string(f)
			default:
				_ = v.Close()
				return out, err
			}
		}
		res.Elapsed = time.Since(start).Round(time.Millisecond)
		out = append(out, res)
		if o.OnResult != nil {
			o.OnResult(res)
		}
	}
	return out, nil
}

// run is one run's state across the cases.
type run struct {
	ctx      context.Context
	s        *Subject
	o        Options
	v        vector.Vector
	caps     vector.Capabilities
	baseline int
	// last is the latest frame received, have whether there is one.
	last vector.VectorState
	have bool
	// applied is the Seq C3 sent, once acknowledged in C4.
	applied uint64
	probe   vector.Command
}

// next waits up to d for a frame. ok is false on timeout; a closed channel
// is a failure of the case.
func (r *run) next(d time.Duration) (vector.VectorState, bool, error) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.ctx.Done():
		return vector.VectorState{}, false, r.ctx.Err()
	case s, open := <-r.v.Telemetry():
		if !open {
			return vector.VectorState{}, false, fail("the telemetry channel closed before Close")
		}
		r.last, r.have = s, true
		return s, true, nil
	case <-t.C:
		return vector.VectorState{}, false, nil
	}
}

// until reads frames until one satisfies cond, up to d.
func (r *run) until(d time.Duration, cond func(vector.VectorState) bool) (vector.VectorState, bool, error) {
	deadline := time.Now().Add(d)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return vector.VectorState{}, false, nil
		}
		s, ok, err := r.next(left)
		if err != nil || !ok {
			return s, false, err
		}
		if cond(s) {
			return s, true, nil
		}
	}
}

// fresh is the latest frame, waiting for one if none has come yet.
func (r *run) fresh() (vector.VectorState, error) {
	r.drain()
	if r.have {
		return r.last, nil
	}
	s, ok, err := r.next(r.o.FramesWithin)
	if err != nil {
		return s, err
	}
	if !ok {
		return s, fail("no frame within %v", r.o.FramesWithin)
	}
	return s, nil
}

// nextSeq is the first Seq above what the vehicle acknowledged: a vehicle
// that has run under another orchestrator filters anything lower.
func (r *run) nextSeq() (uint64, error) {
	s, err := r.fresh()
	if err != nil {
		return 0, err
	}
	return max(s.AckSeq, r.applied) + 1, nil
}

// c1: sorted tags, non-zero speed and range, valid domain, stable.
func (r *run) c1() (string, error) {
	c := r.caps
	switch {
	case c.ID == "":
		return "", fail("no vector id")
	case !domain.ValidDomain(c.Domain):
		return "", fail("domain %q, want aerial or ground", c.Domain)
	case !slices.IsSorted(c.Tags):
		return "", fail("tags %v are not sorted", c.Tags)
	case len(slices.Compact(slices.Clone(c.Tags))) != len(c.Tags):
		return "", fail("tags %v repeat a member", c.Tags)
	case !(c.CruiseSpeed > 0) || math.IsInf(c.CruiseSpeed, 0):
		return "", fail("cruise speed %v, want positive and finite", c.CruiseSpeed)
	case !(c.MaxRangeM > 0) || math.IsInf(c.MaxRangeM, 0):
		return "", fail("max range %v, want positive and finite", c.MaxRangeM)
	}
	again := r.v.Describe()
	if again.ID != c.ID || again.Domain != c.Domain || !slices.Equal(again.Tags, c.Tags) ||
		again.CruiseSpeed != c.CruiseSpeed || again.MaxRangeM != c.MaxRangeM || again.SensorRadiusM != c.SensorRadiusM {
		return "", fail("Describe changed between two calls: %+v, then %+v", c, again)
	}
	return fmt.Sprintf("%s, %s, tags %v", c.ID, c.Domain, c.Tags), nil
}

// c2: consecutive frames validate, LastSeenMs never decreases, modes
// physical.
func (r *run) c2() (string, error) {
	deadline := time.Now().Add(r.o.FramesWithin)
	var prev int64
	for i := range r.o.Frames {
		s, ok, err := r.next(time.Until(deadline))
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fail("%d of %d frames within %v", i, r.o.Frames, r.o.FramesWithin)
		}
		if err := checkFrame(r.caps.ID, s, prev); err != nil {
			return "", fail("frame %d: %v", i+1, err)
		}
		prev = s.LastSeenMs
	}
	return fmt.Sprintf("%d frames", r.o.Frames), nil
}

func checkFrame(id vector.VectorID, s vector.VectorState, prevMs int64) error {
	finite := func(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
	switch {
	case s.ID != id:
		return fmt.Errorf("id %q, the vector is %q", s.ID, id)
	case !s.Position.Finite():
		return errors.New("position is not finite")
	case !finite(s.Heading):
		return fmt.Errorf("heading %v is not finite", s.Heading)
	case !finite(s.Speed) || s.Speed < 0:
		return fmt.Errorf("speed %v", s.Speed)
	case s.BatteryPct < 0 || s.BatteryPct > 100:
		return fmt.Errorf("battery %d%%", s.BatteryPct)
	case !vector.PhysicalMode(s.Mode):
		return fmt.Errorf("mode %q is not physical", s.Mode)
	case s.LastSeenMs < prevMs:
		return fmt.Errorf("mission time %d ms, before the previous frame's %d ms", s.LastSeenMs, prevMs)
	}
	return nil
}

// c3: a goto is reflected in telemetry, mode transit, within ReflectWithin.
// The goto is a point probeOffsetM north of the vehicle, probeClimbM higher
// for an aerial one.
func (r *run) c3() (string, error) {
	seq, err := r.nextSeq()
	if err != nil {
		return "", err
	}
	at := r.last.Position
	wp := vector.Position{Lat: at.Lat + probeOffsetM/domain.MetresPerDegreeLat(), Lon: at.Lon, AltM: at.AltM}
	if r.caps.Domain == vector.DomainAerial {
		wp.AltM += probeClimbM
	}
	r.probe = vector.Command{Vector: r.caps.ID, Seq: seq, Type: vector.CommandGoto, Waypoint: &wp}
	start := time.Now()
	if err := r.v.Execute(r.probe); err != nil {
		return "", fail("Execute goto seq %d: %v", seq, err)
	}
	_, ok, err := r.until(r.o.ReflectWithin, func(s vector.VectorState) bool { return s.Mode == vector.ModeTransit })
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fail("goto seq %d not reflected as transit within %v", seq, r.o.ReflectWithin)
	}
	return fmt.Sprintf("goto seq %d in transit after %v", seq, time.Since(start).Round(time.Millisecond)), nil
}

// c4: re-sending the applied Seq returns no error and changes nothing.
func (r *run) c4() (string, error) {
	if r.probe.Seq == 0 {
		return "", fail("no goto of C3 to re-send")
	}
	seq := r.probe.Seq
	if _, ok, err := r.until(r.o.ApplyWithin, func(s vector.VectorState) bool { return s.AckSeq >= seq }); err != nil {
		return "", err
	} else if !ok {
		return "", fail("goto seq %d not acknowledged within %v", seq, r.o.ApplyWithin)
	}
	r.applied = seq
	before := r.last
	if err := r.v.Execute(r.probe); err != nil {
		return "", fail("re-sending seq %d: %v", seq, err)
	}
	// A re-send that the vehicle applied again, or took for another command,
	// shows in the next second of frames.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s, ok, err := r.next(time.Until(deadline))
		if err != nil {
			return "", err
		}
		if !ok {
			break
		}
		if s.AckSeq != before.AckSeq || s.Mode != before.Mode {
			return "", fail("after the re-send of seq %d: ack %d, mode %q; before it ack %d, mode %q", seq, s.AckSeq, s.Mode, before.AckSeq, before.Mode)
		}
	}
	return fmt.Sprintf("seq %d re-sent, ack %d and mode %q unchanged", seq, before.AckSeq, before.Mode), nil
}

// c5: skipping a Seq is tolerated, the later command still applies. The
// later command is a hold, which leaves the vehicle stopped.
func (r *run) c5() (string, error) {
	base, err := r.nextSeq()
	if err != nil {
		return "", err
	}
	seq := base + 1
	if err := r.v.Execute(vector.Command{Vector: r.caps.ID, Seq: seq, Type: vector.CommandHold}); err != nil {
		return "", fail("Execute hold seq %d after skipping %d: %v", seq, base, err)
	}
	if _, ok, err := r.until(r.o.ApplyWithin, func(s vector.VectorState) bool { return s.AckSeq >= seq }); err != nil {
		return "", err
	} else if !ok {
		return "", fail("hold seq %d, sent after skipping %d, not acknowledged within %v", seq, base, r.o.ApplyWithin)
	}
	r.applied = seq
	return fmt.Sprintf("seq %d skipped, hold seq %d applied", base, seq), nil
}

// c8: a command type the adapter does not declare, or that does not exist,
// is refused with ErrUnsupported.
func (r *run) c8() (string, error) {
	seq, err := r.nextSeq()
	if err != nil {
		return "", err
	}
	refused := []vector.CommandType{"teleport"}
	if d, ok := r.v.(interface{ Supports() []vector.CommandType }); ok {
		for _, t := range vector.CommandTypes() {
			if !slices.Contains(d.Supports(), t) {
				refused = append(refused, t)
			}
		}
	}
	for _, t := range refused {
		c := vector.Command{Vector: r.caps.ID, Seq: seq, Type: t}
		if t == vector.CommandGoto {
			wp := r.last.Position
			c.Waypoint = &wp
		}
		if err := r.v.Execute(c); !errors.Is(err, vector.ErrUnsupported) {
			return "", fail("%q accepted or refused otherwise: %v", t, err)
		}
	}
	return fmt.Sprintf("refused %v", refused), nil
}

// c6: with telemetry withheld, Health reports degraded at DegradedAfter,
// then lost at LostAfter, each within Tolerance. The link comes back before
// the case ends.
func (r *run) c6() (string, error) {
	if _, err := r.fresh(); err != nil {
		return "", err
	}
	r.s.Faults.Withhold(true)
	start := time.Now()
	var degradedAt, lostAt time.Duration
	limit := r.o.LostAfter + 2*r.o.Tolerance
	for lostAt == 0 && time.Since(start) < limit {
		if err := r.sleep(pollPeriod); err != nil {
			r.s.Faults.Withhold(false)
			return "", err
		}
		switch r.v.Health().Kind {
		case vector.HealthDegraded:
			if degradedAt == 0 {
				degradedAt = time.Since(start)
			}
		case vector.HealthLost:
			lostAt = time.Since(start)
		}
		r.drain()
	}
	r.s.Faults.Withhold(false)
	if err := r.resumed(r.o.ReconnectWithin); err != nil {
		return "", err
	}
	near := func(got, want time.Duration) bool { return got >= want-r.o.Tolerance && got <= want+r.o.Tolerance }
	switch {
	case degradedAt == 0:
		return "", fail("never reported degraded before lost (lost at %v)", lostAt.Round(pollPeriod))
	case !near(degradedAt, r.o.DegradedAfter):
		return "", fail("degraded at %v, want %v ± %v", degradedAt.Round(pollPeriod), r.o.DegradedAfter, r.o.Tolerance)
	case lostAt == 0:
		return "", fail("not lost within %v", limit)
	case !near(lostAt, r.o.LostAfter):
		return "", fail("lost at %v, want %v ± %v", lostAt.Round(pollPeriod), r.o.LostAfter, r.o.Tolerance)
	}
	return fmt.Sprintf("degraded at %v, lost at %v", degradedAt.Round(pollPeriod), lostAt.Round(pollPeriod)), nil
}

// c7: after a forced disconnect the adapter reports it, re-establishes the
// link and resumes telemetry on the same channel within ReconnectWithin.
func (r *run) c7() (string, error) {
	if _, err := r.fresh(); err != nil {
		return "", err
	}
	r.s.Faults.Cut()
	start := time.Now()
	lost := false
	for !lost && time.Since(start) < r.o.LostAfter+r.o.Tolerance {
		if err := r.sleep(pollPeriod); err != nil {
			r.s.Faults.Restore()
			return "", err
		}
		lost = r.v.Health().Kind == vector.HealthLost
		r.drain()
	}
	r.s.Faults.Restore()
	if !lost {
		return "", fail("Health still %q %v after the link was cut", r.v.Health().Kind, r.o.LostAfter+r.o.Tolerance)
	}
	left := r.o.ReconnectWithin - time.Since(start)
	if err := r.resumed(left); err != nil {
		return "", err
	}
	return fmt.Sprintf("telemetry resumed %v after the cut", time.Since(start).Round(time.Millisecond)), nil
}

// resumed waits for a frame after a fault, within d.
func (r *run) resumed(d time.Duration) error {
	_, ok, err := r.next(max(d, 0))
	if err != nil {
		return err
	}
	if !ok {
		return fail("telemetry did not resume within %v", d.Round(time.Millisecond))
	}
	return nil
}

// drain takes the frames already delivered, without waiting.
func (r *run) drain() {
	for {
		select {
		case s, open := <-r.v.Telemetry():
			if !open {
				return
			}
			r.last, r.have = s, true
		default:
			return
		}
	}
}

func (r *run) sleep(d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.ctx.Done():
		return r.ctx.Err()
	case <-t.C:
		return nil
	}
}

// c9: Close closes the telemetry channel, a second Close returns nil, and
// the goroutines the adapter started are gone.
func (r *run) c9() (string, error) {
	if err := r.v.Close(); err != nil {
		return "", fail("Close: %v", err)
	}
	deadline := time.After(r.o.ShutdownWithin)
	for open := true; open; {
		select {
		case _, open = <-r.v.Telemetry():
		case <-deadline:
			return "", fail("the telemetry channel is still open %v after Close", r.o.ShutdownWithin)
		}
	}
	if err := r.v.Close(); err != nil {
		return "", fail("a second Close: %v", err)
	}
	stop := time.Now().Add(r.o.ShutdownWithin)
	n := runtime.NumGoroutine()
	for n > r.baseline && time.Now().Before(stop) {
		if err := r.sleep(pollPeriod); err != nil {
			return "", err
		}
		n = runtime.NumGoroutine()
	}
	if n > r.baseline {
		return "", fail("%d goroutines left beyond the %d before the adapter opened", n-r.baseline, r.baseline)
	}
	return "channel closed, second Close nil, no goroutine left", nil
}
