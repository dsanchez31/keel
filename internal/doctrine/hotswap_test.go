package doctrine

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
)

// swappedPack is testPackYAML at 1.3.0: reassign-on-link-loss keeps its
// condition and changes its action, notify-on-degraded-link is gone.
func swappedPack(t *testing.T) *Pack {
	t.Helper()
	src := strings.Replace(testPackYAML, "version: 1.2.0", "version: 1.3.0", 1)
	src = strings.Replace(src, "then: release_lanes(agent), redecompose(policy=nearest_capable)", "then: release_lanes(agent), redecompose(policy=lowest_cost)", 1)
	src = strings.Replace(src, `  - id: notify-on-degraded-link
    when: agent.link_state == DEGRADED for 3s
    then: notify_operator(message="link degraded")
    priority: 10
`, "", 1)
	return mustParse(t, src)
}

func TestHotSwapRetainsByRuleID(t *testing.T) {
	from := mustParse(t, testPackYAML)
	to := swappedPack(t)
	windows := []Window{
		{RuleID: "reassign-on-link-loss", Agent: "DRONE-02", SinceMs: 1000},
		{RuleID: "notify-on-degraded-link", Agent: "DRONE-01", SinceMs: 500},
		{RuleID: "also-100", Agent: "DRONE-03", SinceMs: 200, Fired: true},
		{RuleID: "reassign-on-link-loss", Agent: "DRONE-01", SinceMs: 800},
	}

	kept, sw, err := HotSwap(from, to, windows)
	if err != nil {
		t.Fatal(err)
	}
	wantKept := []Window{
		{RuleID: "also-100", Agent: "DRONE-03", SinceMs: 200, Fired: true},
		{RuleID: "reassign-on-link-loss", Agent: "DRONE-01", SinceMs: 800},
		{RuleID: "reassign-on-link-loss", Agent: "DRONE-02", SinceMs: 1000},
	}
	if !slices.Equal(kept, wantKept) || !slices.Equal(sw.Retained, wantKept) {
		t.Fatalf("kept = %+v, retained = %+v, want %+v", kept, sw.Retained, wantKept)
	}
	if want := []Window{{RuleID: "notify-on-degraded-link", Agent: "DRONE-01", SinceMs: 500}}; !slices.Equal(sw.Discarded, want) {
		t.Fatalf("discarded = %+v, want %+v", sw.Discarded, want)
	}
	if sw.From != from.Ref || sw.FromHash != from.Hash || sw.To != to.Ref || sw.ToHash != to.Hash || sw.FromHash == sw.ToHash {
		t.Fatalf("swap = %+v", sw)
	}

	kept[0].SinceMs = -1
	if sw.Retained[0].SinceMs == -1 {
		t.Fatal("returned windows alias the swap record")
	}
}

func TestHotSwapRejectsMissingPack(t *testing.T) {
	p := mustParse(t, testPackYAML)
	for _, pair := range [][2]*Pack{{nil, p}, {p, nil}} {
		if _, _, err := HotSwap(pair[0], pair[1], nil); !errors.Is(err, ErrInvalidSwap) {
			t.Errorf("HotSwap(%v, %v) = %v", pair[0], pair[1], err)
		}
	}
}

// timedFiring is a firing of reassign-on-link-loss and its mission time.
type timedFiring struct {
	ms int64
	f  Firing
}

// runSwaps evaluates ticks 1..n with DRONE-01's link lost from 1000 ms,
// starting under pack and hot swapping to swaps[ms] at the boundary before
// tick ms, and returns every firing of reassign-on-link-loss.
func runSwaps(t *testing.T, pack *Pack, swaps map[int64]*Pack, n int) []timedFiring {
	t.Helper()
	var windows []Window
	var out []timedFiring
	for k := int64(1); k <= int64(n); k++ {
		ms := k * domain.TickIntervalMs
		if next, ok := swaps[ms]; ok {
			var err error
			if windows, _, err = HotSwap(pack, next, windows); err != nil {
				t.Fatal(err)
			}
			pack = next
		}
		agent := testAgent("DRONE-01")
		if ms >= 1000 {
			agent.Link = domain.LinkLost
		}
		res := mustEvaluate(t, pack, testEnv(t, ms, agent), windows)
		windows = res.Windows
		for _, f := range res.Firings {
			if f.RuleID == "reassign-on-link-loss" {
				out = append(out, timedFiring{ms: ms, f: f})
			}
		}
	}
	return out
}

// The phase 3 definition of done: a swap 3 s into a 5 s window does not
// restart its clock. The rule fires at the tick it would have fired at with no
// swap, with the new pack's actions.
func TestHotSwapKeepsWindowClock(t *testing.T) {
	from := mustParse(t, testPackYAML)
	to := swappedPack(t)

	baseline := runSwaps(t, from, nil, 100)
	swapped := runSwaps(t, from, map[int64]*Pack{4000: to}, 100)
	if len(baseline) != 1 || len(swapped) != 1 {
		t.Fatalf("baseline %+v, swapped %+v, want one firing each", baseline, swapped)
	}
	if baseline[0].ms != 6000 || swapped[0].ms != 6000 {
		t.Fatalf("fired at %d without a swap and %d with one, want 6000 for both", baseline[0].ms, swapped[0].ms)
	}
	if baseline[0].f.Actions[1].Policy != domain.PolicyNearestCapable || swapped[0].f.Actions[1].Policy != domain.PolicyLowestCost {
		t.Fatalf("actions: baseline %+v, swapped %+v", baseline[0].f.Actions, swapped[0].f.Actions)
	}
}

// A window already fired stays fired across a swap: the new pack does not
// fire the same stretch of truth a second time.
func TestHotSwapKeepsFiredFlag(t *testing.T) {
	got := runSwaps(t, mustParse(t, testPackYAML), map[int64]*Pack{7000: swappedPack(t)}, 100)
	if len(got) != 1 || got[0].ms != 6000 {
		t.Fatalf("firings = %+v, want exactly one, at 6000", got)
	}
}

// A window changing its duration under the same id keeps its start time and
// fires at start + new duration.
func TestHotSwapNewDurationAppliesToRetainedStart(t *testing.T) {
	from := mustParse(t, testPackYAML)
	to := mustParse(t, strings.Replace(strings.Replace(testPackYAML, "version: 1.2.0", "version: 1.4.0", 1), "LOST for 5s", "LOST for 8s", 1))
	got := runSwaps(t, from, map[int64]*Pack{3000: to}, 100)
	if len(got) != 1 || got[0].ms != 9000 {
		t.Fatalf("firings = %+v, want one at 9000 (lost since 1000, 8 s window)", got)
	}
}

// A rule dropped by one swap and restored by the next starts a fresh window.
func TestHotSwapDiscardedWindowRestarts(t *testing.T) {
	from := mustParse(t, testPackYAML)
	without := mustParse(t, strings.Replace(strings.Replace(testPackYAML, "version: 1.2.0", "version: 1.5.0", 1),
		"  - id: reassign-on-link-loss\n    when: agent.link_state == LOST for 5s\n    then: release_lanes(agent), redecompose(policy=nearest_capable)\n    priority: 100\n", "", 1))
	if _, ok := without.Rule("reassign-on-link-loss"); ok {
		t.Fatal("edit did not remove the rule")
	}
	got := runSwaps(t, from, map[int64]*Pack{3000: without, 4000: from}, 150)
	if len(got) != 1 || got[0].ms != 9000 {
		t.Fatalf("firings = %+v, want one at 9000 (window restarted at 4000)", got)
	}
}
