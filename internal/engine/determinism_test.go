package engine

import (
	"testing"

	"github.com/dsanchez31/keel/internal/eventlog"
)

// The phase 1 definition of done: a seeded run of N ticks produces an
// identical head hash across 100 repetitions.
//
// The head hash is the whole point. Comparing two runs is comparing two
// digests, not diffing two logs, and a single differing bit anywhere in any
// event, decision or command changes it. That is what makes this test worth
// more than a handful of field-by-field assertions.

const determinismReps = 100

func TestDeterminism(t *testing.T) {
	const (
		seed  = uint64(0x5EED)
		ticks = int64(400)
	)

	want, wantState := runScenario(t, seed, ticks)
	if want.IsZero() {
		t.Fatal("run produced an empty chain")
	}

	for i := 1; i < determinismReps; i++ {
		got, gotState := runScenario(t, seed, ticks)
		if got != want {
			t.Fatalf("repetition %d diverged\n  head %s\n  want %s\n  reproduce: go test ./internal/engine -run TestDeterminism -count=1",
				i, got, want)
		}
		if gotState.Clock != wantState.Clock {
			t.Fatalf("repetition %d: clock %+v, want %+v", i, gotState.Clock, wantState.Clock)
		}
		gotExplored, gotTotal := gotState.Grid().Coverage()
		wantExplored, wantTotal := wantState.Grid().Coverage()
		if gotExplored != wantExplored || gotTotal != wantTotal {
			t.Fatalf("repetition %d: coverage %d/%d, want %d/%d", i, gotExplored, gotTotal, wantExplored, wantTotal)
		}
	}
}

// A different seed must be able to produce a different chain, otherwise the
// test above would pass just as happily against a run that ignores its seed.
//
// The assertion is on the seed reaching the log, not on the decisions
// differing: nothing in the phase 1 pipeline consumes randomness yet, and
// asserting that it does would be asserting a behaviour that does not exist.
func TestSeedIsRecorded(t *testing.T) {
	a, _ := runScenario(t, 1, 20)
	b, _ := runScenario(t, 2, 20)
	if a == b {
		t.Fatal("two seeds produced the same head hash, the seed is not reaching the log")
	}
}

// I5: mission time advances by exactly one tick interval per tick, never
// varying, whatever the tick did.
func TestClockAdvancesByExactlyOneInterval(t *testing.T) {
	s := NewState(7, DefaultConfig(), nil)
	for i := range 50 {
		before := s.Clock
		next, _, _ := Step(s, nil)
		if !before.AdvancedBy(next.Clock) {
			t.Fatalf("tick %d: clock went from %+v to %+v", i, before, next.Clock)
		}
		s = next
	}
	if s.Clock.TickMs != 50*TickIntervalMs {
		t.Fatalf("after 50 ticks mission time is %d ms, want %d", s.Clock.TickMs, 50*TickIntervalMs)
	}
}

// Step returns a new state and leaves the one it was given untouched.
//
// Without this, an invariant asserted against tick N would be asserted
// against a value that tick N+1 has already rewritten, and I2 in particular
// would be unfalsifiable.
func TestStepDoesNotMutateItsInput(t *testing.T) {
	area := testAO()
	plan := testPlan(t, area)

	s := NewState(3, DefaultConfig(), loadRegistry(t))
	s, _, _ = Step(s, []Event{{Kind: EventPlanApproved, Plan: &plan}})

	before := eventlog.MustHashOf(snapshot(s))
	v := VectorState{ID: "DRONE-01", Position: plan.Lanes[0].Waypoints[0], Link: LinkOK, Mode: ModeScanning, LastSeenMs: s.Clock.TickMs}
	caps := testCaps("DRONE-01")
	_, _, _ = Step(s, []Event{
		{Kind: EventVectorJoined, Vector: "DRONE-01", Caps: &caps, Telemetry: &v},
		{Kind: EventTelemetry, Vector: "DRONE-01", Telemetry: &v},
	})
	after := eventlog.MustHashOf(snapshot(s))

	if before != after {
		t.Fatal("Step mutated the state it was given")
	}
}

// I2: coverage is monotonic, an explored cell never becomes unexplored.
func TestCoverageIsMonotonic(t *testing.T) {
	area := testAO()
	plan := testPlan(t, area)
	world := newSim(plan)

	s := NewState(11, DefaultConfig(), loadRegistry(t))
	batch := joinEvents(plan, world)

	prev := make([]bool, len(area.Grid.Explored))
	for range 300 {
		next, cmds, _ := Step(s, batch)
		s = next
		world.apply(cmds)

		cur := s.Grid().Explored
		for i := range prev {
			if prev[i] && !cur[i] {
				t.Fatalf("cell %d went from explored to unexplored at tick %d", i, s.Clock.Tick)
			}
		}
		copy(prev, cur)
		batch = world.advance(s.Clock.TickMs + TickIntervalMs)
	}
}

// snapshot reduces a state to the parts a hash comparison should notice. The
// Rand is excluded because it is a stream position, not an observable.
func snapshot(s State) map[string]any {
	explored, total := s.Grid().Coverage()
	return map[string]any{
		"clock":    s.Clock,
		"mission":  string(s.Mission.State),
		"lanes":    s.Lanes,
		"vectors":  SortedValues(s.Vectors),
		"cursor":   SortedValues(s.Cursor),
		"cmd_seq":  SortedValues(s.CmdSeq),
		"issued":   SortedValues(s.Issued),
		"explored": explored,
		"total":    total,
	}
}
