package engine

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/assign"
	"github.com/dsanchez31/keel/internal/doctrine"
)

// loadRegistry loads the shipped doctrine packs, the registry a real daemon
// resolves plans and hot swaps against.
func loadRegistry(t testing.TB) *doctrine.Registry {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "doctrine-packs", "*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no doctrine pack found: %v", err)
	}
	var packs []*doctrine.Pack
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		pack, err := doctrine.Parse(src)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		packs = append(packs, pack)
	}
	reg, err := doctrine.NewRegistry(packs...)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return reg
}

func mustResolve(t testing.TB, reg *doctrine.Registry, name, version string) *doctrine.Pack {
	t.Helper()
	p, err := reg.Resolve(DoctrineRef{Name: name, Version: version})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return p
}

// checkTrace asserts the phase 5 definition of done on one decision. An
// allocation decision lists every vector of the fleet, accepts the winner
// alone and gives every loser a reason. A doctrine decision names the rule
// that fired and every rule it shadowed, each with the id that shadowed it.
func checkTrace(t testing.TB, d Decision, fleet []VectorID) {
	t.Helper()
	if strings.TrimSpace(d.Rationale) == "" {
		t.Fatalf("%s decision on %s has no rationale", d.Kind, d.Subject)
	}
	switch d.Kind {
	case DecisionAssignment, DecisionReassignment:
		var seen []VectorID
		accepted := 0
		for _, c := range d.Candidates {
			seen = append(seen, c.Vector)
			if !c.Rejected {
				accepted++
				if c.Vector != d.Winner {
					t.Fatalf("%s: accepted candidate %s is not the winner %q", d.Subject, c.Vector, d.Winner)
				}
				continue
			}
			if c.Reason == "" {
				t.Fatalf("%s: %s rejected without a reason", d.Subject, c.Vector)
			}
		}
		slices.Sort(seen)
		if !slices.Equal(seen, fleet) {
			t.Fatalf("%s: candidates %v, want the whole fleet %v", d.Subject, seen, fleet)
		}
		if (d.Winner == "" && accepted != 0) || (d.Winner != "" && accepted != 1) {
			t.Fatalf("%s: winner %q with %d accepted candidate(s)", d.Subject, d.Winner, accepted)
		}
	case DecisionDoctrineRule:
		if d.RuleFired == "" || d.Subject == "" {
			t.Fatalf("doctrine decision without a rule or an agent: %+v", d)
		}
		for _, s := range d.Shadowed {
			if s.ShadowedBy != d.RuleFired {
				t.Fatalf("%s is reported shadowed by %s, the rule that fired is %s", s.RuleID, s.ShadowedBy, d.RuleFired)
			}
		}
	case DecisionDoctrineSwap:
		if d.Swap == nil || d.Swap.FromHash == "" || d.Swap.ToHash == "" {
			t.Fatalf("swap decision without its record: %+v", d)
		}
	}
}

func TestRuleDecisionReportsShadowing(t *testing.T) {
	reg := loadRegistry(t)
	pack := mustResolve(t, reg, "recon-standard", "2.1.0")
	rule, _ := pack.Rule("rtb-on-low-battery")
	f := doctrine.Firing{
		Agent:    "DRONE-02",
		RuleID:   rule.ID,
		Priority: rule.Priority,
		Actions:  rule.Then,
		// Given out of order: the trace sorts them.
		Shadowed: []ShadowedRule{
			{RuleID: "notify-on-degraded-link", ShadowedBy: rule.ID, Priority: 10},
			{RuleID: "reassign-on-link-loss", ShadowedBy: rule.ID, Priority: 100},
		},
	}
	d := ruleDecision(1200, f, []string{"to gcs-west", "released lane-01", "cut 3 lane(s)"})
	checkTrace(t, d, nil)

	if d.Kind != DecisionDoctrineRule || d.RuleFired != "rtb-on-low-battery" || d.Subject != "DRONE-02" || d.TickMs != 1200 {
		t.Fatalf("decision %+v", d)
	}
	if len(d.Shadowed) != 2 || d.Shadowed[0].RuleID != "reassign-on-link-loss" {
		t.Fatalf("shadowed %+v, want both rules, highest priority first", d.Shadowed)
	}
	for _, want := range []string{
		"return_to_base(agent) to gcs-west",
		"release_lanes(agent) released lane-01",
		"redecompose(policy=nearest_capable) cut 3 lane(s)",
		"shadowed reassign-on-link-loss (priority 100), notify-on-degraded-link (priority 10)",
	} {
		if !strings.Contains(d.Rationale, want) {
			t.Fatalf("rationale %q does not say %q", d.Rationale, want)
		}
	}
}

// The full candidate list survives into the decision, rejected candidates
// with their reasons, and the decision does not alias the allocator's slice.
func TestAssignmentDecisionKeepsEveryCandidate(t *testing.T) {
	area := testAO()
	plan := testPlan(t, area)
	lane := plan.Lanes[0]
	lane.AssignedTo = ""

	near, far, lost := testCaps("DRONE-01"), testCaps("DRONE-02"), testCaps("DRONE-03")
	at := lane.Waypoints[0]
	fleet := []assign.Vector{
		{Caps: near, State: VectorState{ID: near.ID, Position: at, BatteryPct: 100, Link: LinkOK, Mode: ModeIdle}, FreeAt: at},
		{Caps: far, State: VectorState{ID: far.ID, Position: plan.Lanes[3].Waypoints[0], BatteryPct: 100, Link: LinkOK, Mode: ModeIdle}, FreeAt: plan.Lanes[3].Waypoints[0]},
		{Caps: lost, State: VectorState{ID: lost.ID, Position: at, BatteryPct: 100, Link: LinkLost, Mode: ModeIdle}, FreeAt: at},
	}
	out, err := assign.Allocate(assign.Request{
		Policy: PolicyNearestCapable, Requires: plan.Requires, Lanes: []Lane{lane}, Fleet: fleet, ReservePct: 15,
	})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	d := assignmentDecision(500, DecisionAssignment, "", out[0])
	checkTrace(t, d, []VectorID{"DRONE-01", "DRONE-02", "DRONE-03"})

	if d.Winner != "DRONE-01" {
		t.Fatalf("winner %q, want DRONE-01", d.Winner)
	}
	reasons := map[VectorID]string{}
	for _, c := range d.Candidates {
		reasons[c.Vector] = c.Reason
	}
	if reasons["DRONE-03"] != "link lost" || !strings.HasPrefix(reasons["DRONE-02"], "farther than DRONE-01") {
		t.Fatalf("reasons %v", reasons)
	}

	out[0].Candidates[0].Vector = "MUTATED"
	if d.Candidates[0].Vector == "MUTATED" {
		t.Fatal("the decision aliases the allocator's candidate slice")
	}
}

func TestSwapDecisionCarriesTheRecord(t *testing.T) {
	reg := loadRegistry(t)
	from := mustResolve(t, reg, "recon-standard", "2.0.0")
	to := mustResolve(t, reg, "recon-standard", "2.1.0")
	windows := []Window{
		{RuleID: "reassign-on-link-loss", Agent: "DRONE-02", SinceMs: 3000},
		{RuleID: "gone-in-2-1", Agent: "DRONE-01", SinceMs: 100},
	}
	_, sw, err := doctrine.HotSwap(from, to, windows)
	if err != nil {
		t.Fatalf("hot swap: %v", err)
	}
	d := swapDecision(6000, sw)
	checkTrace(t, d, nil)

	if d.Swap.From != from.Ref || d.Swap.To != to.Ref || d.Swap.FromHash != from.Hash || d.Swap.ToHash != to.Hash {
		t.Fatalf("swap record %+v", d.Swap)
	}
	if len(d.Swap.Retained) != 1 || len(d.Swap.Discarded) != 1 {
		t.Fatalf("retained %v, discarded %v", d.Swap.Retained, d.Swap.Discarded)
	}
	if !strings.Contains(d.Rationale, "1 window(s) retained, 1 discarded") {
		t.Fatalf("rationale %q", d.Rationale)
	}
	sw.Retained[0].SinceMs = -1
	if d.Swap.Retained[0].SinceMs == -1 {
		t.Fatal("the decision aliases the swap's window slice")
	}
}
