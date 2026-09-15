package engine

import "github.com/dsanchez31/keel/internal/domain"

// The logical clock.
//
// Mission time is an int64 count of milliseconds that starts at 0 on the first
// tick and advances by exactly one tick interval per tick. Nothing in the
// decision path reads a wall clock, and .golangci.yml enforces that with a
// forbidigo rule scoped to this package.
//
// The wall clock still exists, but it lives in the daemon: it decides *when*
// to call Step, never *what* Step sees. That separation is what lets the same
// mission run in real time against SITL and headless-fast in the DST harness
// and produce identical decisions.

// TickIntervalMs is the fixed tick interval in milliseconds of mission time.
// Fixed at 100 ms by spec.md section 2. It is a constant rather than
// configuration on purpose: a mission recorded at one interval and replayed at
// another would not be the same mission, and there would be nothing in the log
// to say so. The definition lives in internal/domain, where doctrine can
// reach it without an import cycle.
const TickIntervalMs = domain.TickIntervalMs

// TicksPerSecond is the derived tick rate.
const TicksPerSecond = 1000 / TickIntervalMs

// This package does not import "time", and that is a rule rather than an
// accident. The wall-clock pacing constant a real-time run needs is
// time.Duration(engine.TickIntervalMs) * time.Millisecond, computed in the
// daemon. Importing "time" here would put a clock read one keystroke away
// from every function in the decision path.

// Clock is mission time. It carries a tick count rather than only a
// millisecond value so that "how many ticks since" is an integer question with
// no rounding in it.
type Clock struct {
	Tick   int64 `json:"tick"`
	TickMs int64 `json:"tick_ms"`
}

// NewClock returns a clock at mission time zero, before the first tick.
func NewClock() Clock { return Clock{} }

// Advance returns the clock one tick later. It is a value method returning a
// new value: there is no in-place mutation to accidentally share between a
// state and its copy.
func (c Clock) Advance() Clock {
	return Clock{Tick: c.Tick + 1, TickMs: c.TickMs + TickIntervalMs}
}

// AdvancedBy reports whether next is exactly one tick after c. This is
// invariant I5 as a predicate, asserted at every tick boundary by the DST
// harness.
func (c Clock) AdvancedBy(next Clock) bool {
	return next.Tick == c.Tick+1 && next.TickMs == c.TickMs+TickIntervalMs
}

// MsToTicks converts a duration in milliseconds of mission time to a whole
// number of ticks, rounding up.
//
// Rounding up rather than to nearest is deliberate: a doctrine window of
// "5s" must not fire at 4.95 s. A rule that fires early is a rule that fires
// on a condition that had not yet held for as long as it claims.
func MsToTicks(ms int64) int64 {
	if ms <= 0 {
		return 0
	}
	return (ms + TickIntervalMs - 1) / TickIntervalMs
}

// TicksToMs converts a tick count to milliseconds of mission time.
func TicksToMs(ticks int64) int64 { return ticks * TickIntervalMs }

// Seconds renders mission time in seconds as a float, for display only.
// Nothing in the decision path compares it.
func (c Clock) Seconds() float64 { return float64(c.TickMs) / 1000 }
