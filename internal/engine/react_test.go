package engine

import (
	"slices"
	"strings"
	"testing"
)

// loop drives Step against the test sim, asserting at every tick boundary
// what phase 5 guarantees: every decision carries a complete trace, no cell
// is held by two lanes (I1), coverage never shrinks (I2) and mission time
// advances by exactly one tick (I5).
type loop struct {
	t     *testing.T
	s     State
	w     *sim
	next  []Event
	fleet []VectorID
	decs  []Decision
	cmds  []Command
}

func newLoop(t *testing.T, plan ApprovedPlan) *loop {
	t.Helper()
	w := newSim(plan)
	return &loop{
		t:     t,
		s:     NewState(1, DefaultConfig(), loadRegistry(t)),
		w:     w,
		next:  joinEvents(plan, w),
		fleet: testFleet(plan),
	}
}

// tick runs one Step with the sim's telemetry plus extra events, and returns
// that tick's decisions.
func (l *loop) tick(extra ...Event) []Decision {
	l.t.Helper()
	prev := l.s
	next, cmds, decs := Step(prev, append(l.next, extra...))
	l.s = next
	l.w.apply(cmds)
	l.cmds = append(l.cmds, cmds...)
	l.decs = append(l.decs, decs...)
	for _, d := range decs {
		checkTrace(l.t, d, l.fleet)
	}
	checkInvariants(l.t, prev, next)
	l.next = l.w.advance(next.Clock.TickMs + TickIntervalMs)
	return decs
}

// run ticks until stop holds, failing after max ticks.
func (l *loop) run(max int, stop func(State) bool) {
	l.t.Helper()
	for range max {
		l.tick()
		if stop(l.s) {
			return
		}
	}
	explored, total := l.s.Grid().Coverage()
	l.t.Fatalf("condition not reached in %d ticks: mission %s, %d of %d cells explored", max, l.s.Mission.State, explored, total)
}

func checkInvariants(t *testing.T, prev, cur State) {
	t.Helper()
	if !prev.Clock.AdvancedBy(cur.Clock) {
		t.Fatalf("I5: clock went from %+v to %+v", prev.Clock, cur.Clock)
	}
	owner := map[CellID]LaneID{}
	for _, l := range cur.Lanes {
		if !l.Assigned() {
			continue
		}
		for _, c := range l.Cells {
			if o, dup := owner[c]; dup {
				t.Fatalf("I1: cell %d held by %s and %s at tick %d", c, o, l.ID, cur.Clock.Tick)
			}
			owner[c] = l.ID
		}
	}
	before, after := prev.Grid().Explored, cur.Grid().Explored
	for i := range before {
		if before[i] && (i >= len(after) || !after[i]) {
			t.Fatalf("I2: cell %d unexplored again at tick %d", i, cur.Clock.Tick)
		}
	}
}

func decisionsOf(decs []Decision, kind DecisionKind) []Decision {
	var out []Decision
	for _, d := range decs {
		if d.Kind == kind {
			out = append(out, d)
		}
	}
	return out
}

func ownerOf(s State, lane LaneID) VectorID {
	l, _ := s.LaneByID(lane)
	return l.AssignedTo
}

func running(s State) bool { return s.Mission.State == MissionRunning }

// The launch round: every lane of the approved plan goes to the drone that
// sits by it, each decision listing the whole fleet.
func TestLaunchAllocatesEveryLane(t *testing.T) {
	plan := testPlan(t, testAO())
	l := newLoop(t, plan)
	decs := l.tick()

	got := decisionsOf(decs, DecisionAssignment)
	if len(got) != 4 {
		t.Fatalf("%d assignment decisions on the launch tick, want 4: %+v", len(got), decs)
	}
	for i, d := range got {
		want := l.fleet[i]
		if d.Subject != string(plan.Lanes[i].ID) || d.Winner != want || len(d.Candidates) != 4 {
			t.Fatalf("decision %d: %s won by %s with %d candidates, want %s by %s", i, d.Subject, d.Winner, len(d.Candidates), plan.Lanes[i].ID, want)
		}
		if ownerOf(l.s, plan.Lanes[i].ID) != want {
			t.Fatalf("%s owned by %q, want %s", plan.Lanes[i].ID, ownerOf(l.s, plan.Lanes[i].ID), want)
		}
	}
	if len(l.s.Unassigned) != 0 {
		t.Fatalf("lanes still unassigned after launch: %v", l.s.Unassigned)
	}
}

// The reference reaction of spec section 14 in miniature: DRONE-02 loses its
// link, reassign-on-link-loss fires after 5 s of mission time, its lane is
// released and redecomposed across the three survivors, and coverage still
// completes with every invariant held at every tick.
func TestLinkLossRedistributesAndCompletes(t *testing.T) {
	plan := testPlan(t, testAO())
	l := newLoop(t, plan)
	l.run(100, func(s State) bool { return s.Clock.Tick >= 100 })

	l.w.silent["DRONE-02"] = true
	lossMs := l.s.Clock.TickMs + TickIntervalMs
	l.tick(Event{Kind: EventFaultInjected, Fault: &Fault{Kind: FaultLinkLoss, Vector: "DRONE-02"}})

	var fired Decision
	l.run(80, func(s State) bool {
		for _, d := range decisionsOf(l.decs, DecisionDoctrineRule) {
			if d.RuleFired == "reassign-on-link-loss" && d.Subject == "DRONE-02" {
				fired = d
				return true
			}
		}
		return false
	})
	if fired.TickMs != lossMs+5000 {
		t.Fatalf("rule fired at %d ms, want the first tick 5 s after the loss at %d ms", fired.TickMs, lossMs)
	}
	if !strings.Contains(fired.Rationale, "released lane-01") {
		t.Fatalf("rationale %q does not name the released lane", fired.Rationale)
	}
	if _, ok := l.s.LaneByID("lane-01"); ok {
		t.Fatal("lane-01 was redecomposed and should be retired")
	}

	redec := decisionsOf(l.decs, DecisionRedecompose)
	if len(redec) != 1 || redec[0].RuleFired != "reassign-on-link-loss" {
		t.Fatalf("redecompose decisions %+v", redec)
	}
	var cut []Lane
	for _, ln := range l.s.Lanes {
		if ln.Index >= 4 {
			cut = append(cut, ln)
		}
	}
	if len(cut) != 3 {
		t.Fatalf("%d new lanes, want one per survivor", len(cut))
	}
	owners := map[VectorID]bool{}
	for _, ln := range cut {
		if ln.AssignedTo == "" || ln.AssignedTo == "DRONE-02" {
			t.Fatalf("%s owned by %q after redecomposition", ln.ID, ln.AssignedTo)
		}
		owners[ln.AssignedTo] = true
	}
	if len(owners) != 3 {
		t.Fatalf("new lanes went to %v, want the three survivors", owners)
	}
	re := decisionsOf(l.decs, DecisionReassignment)
	if len(re) != 3 || re[0].RuleFired != "reassign-on-link-loss" {
		t.Fatalf("reassignment decisions %+v", re)
	}
	for _, d := range re {
		for _, c := range d.Candidates {
			if c.Vector == "DRONE-02" && c.Reason != "link lost" {
				t.Fatalf("%s: DRONE-02 rejected for %q, want link lost", d.Subject, c.Reason)
			}
		}
	}

	l.run(3000, func(s State) bool { return !running(s) })
	if l.s.Mission.State != MissionComplete {
		t.Fatalf("mission %s, want complete", l.s.Mission.State)
	}
	if next := l.s.NextLaneIndex; next != 7 {
		t.Fatalf("next lane index %d, want 7", next)
	}
}

// Two rules ready for one agent on one tick: the higher priority fires, the
// other is reported shadowed by it. The vector sent home leaves the pool of
// eligible vectors, so its lane is not handed straight back to it.
func TestShadowedRuleIsReportedAndRTBIsExcluded(t *testing.T) {
	plan := testPlan(t, testAO())
	plan.GCS = []Station{{Name: "gcs-test", Position: Position{Lat: 44.995, Lon: 5.006}}}
	l := newLoop(t, plan)
	l.run(50, func(s State) bool { return s.Clock.Tick >= 50 })

	// The link is lost at t: reassign-on-link-loss becomes ready at t+5s.
	// On that very tick the battery collapses below the reserve, so
	// rtb-on-low-battery is ready too, and outranks it.
	l.w.silent["DRONE-02"] = true
	lossMs := l.s.Clock.TickMs + TickIntervalMs
	l.tick(Event{Kind: EventFaultInjected, Fault: &Fault{Kind: FaultLinkLoss, Vector: "DRONE-02"}})
	l.run(49, func(s State) bool { return s.Clock.TickMs+TickIntervalMs >= lossMs+5000 })
	decs := l.tick(Event{Kind: EventFaultInjected, Fault: &Fault{Kind: FaultBatteryDrain, Vector: "DRONE-02", Magnitude: 90}})

	rules := decisionsOf(decs, DecisionDoctrineRule)
	if len(rules) != 1 || rules[0].RuleFired != "rtb-on-low-battery" || rules[0].Subject != "DRONE-02" {
		t.Fatalf("doctrine decisions %+v, want rtb-on-low-battery for DRONE-02", rules)
	}
	if sh := rules[0].Shadowed; len(sh) != 1 || sh[0].RuleID != "reassign-on-link-loss" || sh[0].ShadowedBy != "rtb-on-low-battery" {
		t.Fatalf("shadowed %+v, want reassign-on-link-loss shadowed by rtb-on-low-battery", sh)
	}
	if l.s.Vectors["DRONE-02"].Mode != ModeRTB {
		t.Fatalf("DRONE-02 mode %s, want rtb", l.s.Vectors["DRONE-02"].Mode)
	}
	rtb := slices.IndexFunc(l.cmds, func(c Command) bool { return c.Vector == "DRONE-02" && c.Type == CommandRTB })
	if rtb < 0 || l.cmds[rtb].Waypoint == nil || *l.cmds[rtb].Waypoint != plan.GCS[0].Position {
		t.Fatalf("no rtb command to gcs-test for DRONE-02 in %+v", l.cmds)
	}
	for _, ln := range l.s.Lanes {
		if ln.AssignedTo == "DRONE-02" {
			t.Fatalf("%s is still or again held by DRONE-02, which was sent home", ln.ID)
		}
	}
	for _, d := range decisionsOf(decs, DecisionReassignment) {
		for _, c := range d.Candidates {
			if c.Vector == "DRONE-02" && !c.Rejected {
				t.Fatalf("%s: DRONE-02 accepted while returning to base", d.Subject)
			}
		}
	}

	// The shadowed rule consumed its edge: it does not fire in the winner's
	// place on the next tick.
	next := l.tick()
	for _, d := range decisionsOf(next, DecisionDoctrineRule) {
		if d.RuleFired == "reassign-on-link-loss" {
			t.Fatalf("shadowed rule fired on the next tick: %+v", d)
		}
	}
}

// A hot swap keeps the clock of a window whose rule survives: under 2.0.0 the
// link-loss rule waits 10 s; three seconds into the loss the pack becomes
// 2.1.0, whose rule waits 5 s, and it fires 5 s after the loss, not 5 s after
// the swap. The mission, its lanes and its coverage go through untouched.
func TestHotSwapRetainsTheWindow(t *testing.T) {
	plan := testPlan(t, testAO())
	plan.Doctrine = DoctrineRef{Name: "recon-standard", Version: "2.0.0"}
	plan = pin(t, plan)
	l := newLoop(t, plan)
	l.run(50, func(s State) bool { return s.Clock.Tick >= 50 })

	l.w.silent["DRONE-02"] = true
	lossMs := l.s.Clock.TickMs + TickIntervalMs
	l.tick(Event{Kind: EventFaultInjected, Fault: &Fault{Kind: FaultLinkLoss, Vector: "DRONE-02"}})
	l.run(29, func(s State) bool { return s.Clock.TickMs >= lossMs+2900 })

	before := l.s
	decs := l.tick(swapEvent(t, "2.1.0"))
	swaps := decisionsOf(decs, DecisionDoctrineSwap)
	if len(swaps) != 1 || swaps[0].Swap == nil {
		t.Fatalf("swap decisions %+v", swaps)
	}
	sw := swaps[0].Swap
	if !slices.ContainsFunc(sw.Retained, func(w Window) bool {
		return w.RuleID == "reassign-on-link-loss" && w.Agent == "DRONE-02" && w.SinceMs == lossMs
	}) {
		t.Fatalf("retained %+v, want the DRONE-02 link-loss window started at %d", sw.Retained, lossMs)
	}
	if l.s.Mission.State != before.Mission.State || l.s.Mission.Doctrine.Version != "2.1.0" || l.s.Pack.Ref.Version != "2.1.0" {
		t.Fatalf("after swap: mission %s doctrine %s pack %s", l.s.Mission.State, l.s.Mission.Doctrine, l.s.Pack.Ref)
	}
	for _, ln := range before.Lanes {
		if ownerOf(l.s, ln.ID) != ln.AssignedTo {
			t.Fatalf("%s changed hands across the swap", ln.ID)
		}
	}

	l.run(30, func(s State) bool {
		return slices.ContainsFunc(l.decs, func(d Decision) bool { return d.RuleFired == "reassign-on-link-loss" && d.Kind == DecisionDoctrineRule })
	})
	i := slices.IndexFunc(l.decs, func(d Decision) bool { return d.RuleFired == "reassign-on-link-loss" && d.Kind == DecisionDoctrineRule })
	if got := l.decs[i].TickMs; got != lossMs+5000 {
		t.Fatalf("rule fired at %d ms, want %d: the window restarted at the swap", got, lossMs+5000)
	}
}

func TestSwapToAnUnknownPackIsRefused(t *testing.T) {
	plan := testPlan(t, testAO())
	l := newLoop(t, plan)
	l.tick()
	decs := l.tick(Event{Kind: EventDoctrineSwap, Doctrine: &DoctrineRef{Name: "recon-standard", Version: "9.9.9"}})
	if swaps := decisionsOf(decs, DecisionDoctrineSwap); len(swaps) != 0 {
		t.Fatalf("swap decisions %+v for a refused swap", swaps)
	}
	notes := decisionsOf(decs, DecisionOperatorNotice)
	if len(notes) != 1 || !strings.Contains(notes[0].Rationale, "refused") {
		t.Fatalf("notices %+v, want one refusal", notes)
	}
	if l.s.Pack.Ref.Version != "2.1.0" {
		t.Fatalf("active pack %s after a refused swap", l.s.Pack.Ref)
	}
}

// swapEvent requests a hot swap to recon-standard at version, pinned to the
// shipped pack as keeld pins it.
func swapEvent(t *testing.T, version string) Event {
	t.Helper()
	p := mustResolve(t, loadRegistry(t), "recon-standard", version)
	ref := p.Ref
	return Event{Kind: EventDoctrineSwap, Doctrine: &ref, DoctrineHash: p.Hash}
}

// A swap whose pin the registry pack does not match, or that carries none, is
// refused with a notice naming the pack, and the active pack stays.
func TestSwapToAnUnpinnedPackIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, hash, want string
	}{
		{"no hash", "", "no expected pack hash"},
		{"another hash", strings.Repeat("0", 64), "000000000000 expected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLoop(t, testPlan(t, testAO()))
			l.tick()
			ev := swapEvent(t, "2.2.0")
			ev.DoctrineHash = tc.hash
			decs := l.tick(ev)
			if swaps := decisionsOf(decs, DecisionDoctrineSwap); len(swaps) != 0 {
				t.Fatalf("swap decisions %+v for a refused swap", swaps)
			}
			notes := decisionsOf(decs, DecisionOperatorNotice)
			if len(notes) != 1 || !strings.Contains(notes[0].Rationale, "recon-standard@2.2.0") || !strings.Contains(notes[0].Rationale, tc.want) {
				t.Fatalf("notices %+v, want one refusal naming recon-standard@2.2.0 and %q", notes, tc.want)
			}
			if l.s.Pack.Ref.Version != "2.1.0" {
				t.Fatalf("active pack %s after a refused swap", l.s.Pack.Ref)
			}
		})
	}
}

// A plan whose pin the registry pack does not match, or that carries none,
// does not start: the mission stays planning and the decision names the pack.
func TestApprovalWithAnUnpinnedPackIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, hash, want string
	}{
		{"no hash", "", "no expected pack hash"},
		{"another hash", strings.Repeat("0", 64), "000000000000 expected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := testPlan(t, testAO())
			plan.DoctrineHash = tc.hash
			l := newLoop(t, plan)
			decs := l.tick()
			if l.s.Mission.State != MissionPlanning || l.s.Pack != nil {
				t.Fatalf("mission %s with pack %v, want the approval refused", l.s.Mission.State, l.s.Pack)
			}
			ms := decisionsOf(decs, DecisionMissionState)
			if len(ms) != 1 || !strings.Contains(ms[0].Rationale, "recon-standard@2.1.0") || !strings.Contains(ms[0].Rationale, tc.want) {
				t.Fatalf("mission decisions %+v, want one refusal naming recon-standard@2.1.0 and %q", ms, tc.want)
			}
		})
	}
}

func TestJoinWithoutStateIsRefused(t *testing.T) {
	s := NewState(1, DefaultConfig(), nil)
	caps := testCaps("DRONE-01")
	s, _, decs := Step(s, []Event{{Kind: EventVectorJoined, Vector: "DRONE-01", Caps: &caps}})
	if _, ok := s.Caps["DRONE-01"]; ok {
		t.Fatal("a join without an initial state was admitted")
	}
	if len(decs) != 1 || decs[0].Kind != DecisionOperatorNotice || !strings.Contains(decs[0].Rationale, "refused") {
		t.Fatalf("decisions %+v, want one refusal", decs)
	}
}

// A vehicle that knows no mission time sends LastSeenMs 0, and the engine
// stamps the tick that accepts the frame. Such frames keep being accepted
// after the join, so the link does not age, while a frame carrying an
// explicit time older than the one held is still dropped.
func TestFrameWithoutMissionTimeIsStamped(t *testing.T) {
	caps := testCaps("DRONE-01")
	start := VectorState{ID: "DRONE-01", Position: Position{Lat: 45, Lon: 5}, BatteryPct: 100, Link: LinkOK, Mode: ModeIdle}
	s := NewState(1, DefaultConfig(), nil)
	s, _, _ = Step(s, []Event{{Kind: EventVectorJoined, Vector: "DRONE-01", Caps: &caps, Telemetry: &start}})

	frame := start
	for i := range DefaultConfig().StaleTelemetryTicks * 2 {
		frame.Position.Lat = 45 + float64(float64(i+1)*1e-5)
		f := frame
		s, _, _ = Step(s, []Event{{Kind: EventTelemetry, Vector: "DRONE-01", Telemetry: &f}})
		got := s.Vectors["DRONE-01"]
		if got.Position != frame.Position || got.LastSeenMs != s.Clock.TickMs {
			t.Fatalf("tick %d: held %+v at %d ms, want the frame stamped %d ms", s.Clock.Tick, got.Position, got.LastSeenMs, s.Clock.TickMs)
		}
	}
	if got := s.Vectors["DRONE-01"].Link; got != LinkOK {
		t.Fatalf("link %s after %d ticks of frames, want ok", got, DefaultConfig().StaleTelemetryTicks*2)
	}

	held := s.Vectors["DRONE-01"]
	stale := held
	stale.Position = Position{Lat: 44, Lon: 4}
	stale.LastSeenMs = held.LastSeenMs - TickIntervalMs
	s, _, _ = Step(s, []Event{{Kind: EventTelemetry, Vector: "DRONE-01", Telemetry: &stale}})
	if got := s.Vectors["DRONE-01"].Position; got != held.Position {
		t.Fatalf("a frame older than the one held moved the vector to %+v", got)
	}
}

func TestApprovalWithAnUnknownDoctrineIsRefused(t *testing.T) {
	plan := testPlan(t, testAO())
	plan.Doctrine = DoctrineRef{Name: "recon-standard", Version: "9.9.9"}
	l := newLoop(t, plan)
	decs := l.tick()
	if l.s.Mission.State != MissionPlanning || l.s.Pack != nil {
		t.Fatalf("mission %s with pack %v, want the approval refused", l.s.Mission.State, l.s.Pack)
	}
	ms := decisionsOf(decs, DecisionMissionState)
	if len(ms) != 1 || !strings.Contains(ms[0].Rationale, "refused") {
		t.Fatalf("mission decisions %+v", ms)
	}
}

// An engine whose arrival radius plus position error does not fit under the
// plan's route clearance refuses the approval: its vectors would trip the
// geofence on their own routes. 7.5 m of error spends the 12.5 m clearance of
// the test AO's 50 m cells exactly.
func TestApprovalBeyondTheRouteClearanceIsRefused(t *testing.T) {
	l := newLoop(t, testPlan(t, testAO()))
	l.s.Config.PositionErrorM = 7.5
	decs := l.tick()
	if l.s.Mission.State != MissionPlanning || l.s.Pack != nil {
		t.Fatalf("mission %s with pack %v, want the approval refused", l.s.Mission.State, l.s.Pack)
	}
	ms := decisionsOf(decs, DecisionMissionState)
	if len(ms) != 1 || !strings.Contains(ms[0].Rationale, "refused") || !strings.Contains(ms[0].Rationale, "does not exceed") {
		t.Fatalf("mission decisions %+v, want one refusal naming the clearance", ms)
	}
}

// Work none of the fleet can reach fails the mission on the stall deadline,
// not on the tick ceiling, and only once the lane that can be worked is
// walked. Three drones are killed at launch and the fourth is held at 16
// percent: above the 15 percent reserve, so it keeps its lane, and with 200 m
// of range, so it takes none of the three released ones.
func TestStarvedMissionFailsOnTheStall(t *testing.T) {
	plan := testPlan(t, testAO())
	l := newLoop(t, plan)
	l.tick()

	var faults []Event
	for _, id := range []VectorID{"DRONE-01", "DRONE-02", "DRONE-03"} {
		l.w.silent[id] = true
		faults = append(faults, Event{Kind: EventFaultInjected, Fault: &Fault{Kind: FaultKill, Vector: id}})
	}
	l.w.battery["DRONE-04"] = 16
	faults = append(faults, Event{Kind: EventFaultInjected, Fault: &Fault{Kind: FaultBatteryDrain, Vector: "DRONE-04", Magnitude: 84}})
	l.tick(faults...)
	if !l.s.Working() || len(l.s.PendingLanes()) != 3 {
		t.Fatalf("working %v with %d pending lane(s), want DRONE-04 on its lane and three lanes pending", l.s.Working(), len(l.s.PendingLanes()))
	}

	var walkedMs int64
	l.run(3000, func(s State) bool {
		if walkedMs == 0 && !s.Working() {
			walkedMs = s.Clock.TickMs
		}
		return !running(s)
	})
	if l.s.Mission.State != MissionFailed || walkedMs == 0 {
		t.Fatalf("mission %s, last lane walked at %d ms", l.s.Mission.State, walkedMs)
	}
	if lane, ok := l.s.LaneByID("lane-03"); ok && l.s.Cursor["lane-03"] < len(lane.Waypoints) {
		t.Fatalf("lane-03 at waypoint %d of %d: the mission stalled before its worked lane was walked", l.s.Cursor["lane-03"], len(lane.Waypoints))
	}
	if want := walkedMs + TicksToMs(DefaultConfig().StallDeadlineTicks-1); l.s.Clock.TickMs != want {
		t.Fatalf("failed at %d ms, want the stall deadline after the last worked lane at %d ms", l.s.Clock.TickMs, walkedMs)
	}
	ms := decisionsOf(l.decs, DecisionMissionState)
	last := ms[len(ms)-1]
	if !strings.Contains(last.Rationale, "no eligible vector has worked a lane") || !strings.Contains(last.Rationale, "pending") {
		t.Fatalf("final decision %q, want the stall naming the pending work", last.Rationale)
	}
}

// A killed vector is outside doctrine evaluation, so the kill itself releases
// its lanes, and the allocation round of the same tick hands them on.
func TestKillReleasesAndReassigns(t *testing.T) {
	plan := testPlan(t, testAO())
	l := newLoop(t, plan)
	l.run(30, func(s State) bool { return s.Clock.Tick >= 30 })

	l.w.silent["DRONE-03"] = true
	decs := l.tick(Event{Kind: EventFaultInjected, Fault: &Fault{Kind: FaultKill, Vector: "DRONE-03"}})
	re := decisionsOf(decs, DecisionReassignment)
	if len(re) != 1 || re[0].Subject != "lane-02" || re[0].Winner == "" || re[0].Winner == "DRONE-03" {
		t.Fatalf("reassignment decisions %+v, want lane-02 handed to a survivor", re)
	}
	if got := ownerOf(l.s, "lane-02"); got != re[0].Winner {
		t.Fatalf("lane-02 owned by %q, decision says %s", got, re[0].Winner)
	}
}

// An operator abort fails the mission and stops every vector flying to a
// point, once; a vector returning to base keeps returning.
func TestAbortStopsTheVectorsInFlight(t *testing.T) {
	plan := testPlan(t, testAO())
	l := newLoop(t, plan)
	l.run(30, func(s State) bool { return s.Clock.Tick >= 30 })
	for _, id := range l.fleet {
		if l.s.Issued[id].Command.Type != CommandGoto {
			t.Fatalf("%s stands on %+v, want every drone on a goto before the abort", id, l.s.Issued[id].Command)
		}
	}
	home := l.s.Issued["DRONE-04"]
	home.Command.Type = CommandRTB
	l.s.Issued["DRONE-04"] = home

	before := len(l.cmds)
	decs := l.tick(Event{Kind: EventOperatorAbort, Reason: "daemon shutting down"})
	if l.s.Mission.State != MissionFailed {
		t.Fatalf("mission %s after the abort", l.s.Mission.State)
	}
	ms := decisionsOf(decs, DecisionMissionState)
	if len(ms) != 1 || !strings.Contains(ms[0].Rationale, "daemon shutting down, DRONE-01, DRONE-02, DRONE-03 stopped") {
		t.Fatalf("mission decisions %+v, want the failure naming the stopped drones", ms)
	}
	var stopped []VectorID
	for _, c := range l.cmds[before:] {
		if c.Type != CommandAbort {
			t.Fatalf("the abort tick issued %+v", c)
		}
		if c.Seq != l.s.CmdSeq[c.Vector] {
			t.Fatalf("abort to %s carries Seq %d, want the next one %d", c.Vector, c.Seq, l.s.CmdSeq[c.Vector])
		}
		stopped = append(stopped, c.Vector)
	}
	if want := []VectorID{"DRONE-01", "DRONE-02", "DRONE-03"}; !slices.Equal(stopped, want) {
		t.Fatalf("aborted %v, want %v and DRONE-04 left returning", stopped, want)
	}

	after := len(l.cmds)
	for range int(DefaultConfig().ResendTicks) + 1 {
		l.tick()
	}
	if extra := l.cmds[after:]; len(extra) != 0 {
		t.Fatalf("a failed mission kept commanding: %+v", extra)
	}
}

// A lost goto leaves the vehicle where it was. The engine re-sends the
// standing command with its original Seq until the vehicle behaves as told.
func TestStandingCommandIsResent(t *testing.T) {
	plan := testPlan(t, testAO())
	l := newLoop(t, plan)
	l.w.deaf["DRONE-01"] = true
	l.run(int(DefaultConfig().ResendTicks)+1, func(s State) bool { return s.Clock.Tick > DefaultConfig().ResendTicks })

	var sent []Command
	for _, c := range l.cmds {
		if c.Vector == "DRONE-01" {
			sent = append(sent, c)
		}
	}
	if len(sent) != 2 || sent[0].Seq != sent[1].Seq || sent[1].Type != CommandGoto || *sent[0].Waypoint != *sent[1].Waypoint {
		t.Fatalf("commands to DRONE-01 %+v, want the first goto twice with one Seq", sent)
	}

	l.w.deaf["DRONE-01"] = false
	l.run(200, func(s State) bool { return s.Cursor["lane-00"] > 0 })
}

// A command the vehicle acknowledged is not re-sent, however long it stands.
// The acknowledgement is the vehicle's word, read afresh from every frame: a
// vehicle that restarts reports nothing applied and gets its command again.
func TestAcknowledgedCommandIsNotResent(t *testing.T) {
	plan := testPlan(t, testAO())
	l := newLoop(t, plan)
	resend := DefaultConfig().ResendTicks
	l.run(int(2*resend)+1, func(s State) bool { return s.Clock.Tick > 2*resend })

	var sent []Command
	for _, c := range l.cmds {
		if c.Vector == "DRONE-01" {
			sent = append(sent, c)
		}
	}
	if len(sent) != 1 || sent[0].Type != CommandGoto {
		t.Fatalf("commands to DRONE-01 %+v, want its first goto once", sent)
	}
	standing := l.s.Issued["DRONE-01"].Command
	if standing.Seq != sent[0].Seq || l.s.Vectors["DRONE-01"].AckSeq != standing.Seq {
		t.Fatalf("standing %+v, acknowledged %d: the first goto should stand acknowledged past two re-send intervals",
			standing, l.s.Vectors["DRONE-01"].AckSeq)
	}

	l.w.acked["DRONE-01"] = 0
	before := len(l.cmds)
	var re Command
	l.run(3, func(State) bool {
		for _, c := range l.cmds[before:] {
			if c.Vector == "DRONE-01" {
				re = c
				return true
			}
		}
		return false
	})
	if re.Seq != standing.Seq || re.Type != CommandGoto {
		t.Fatalf("after a restart sent %+v, want the standing goto again with Seq %d", re, standing.Seq)
	}
}

// A lane walked to its end with cells still uncovered is released and its
// residue redecomposed, so the mission completes instead of waiting on a lane
// nobody will ever fly again. Here lane-03 stops halfway up the AO.
func TestWalkedLaneResidueIsRedecomposed(t *testing.T) {
	plan := testPlan(t, testAO())
	short := &plan.Lanes[3]
	short.Waypoints = short.Waypoints[:len(short.Waypoints)/2]
	l := newLoop(t, plan)

	l.run(3000, func(s State) bool { return !running(s) })
	if l.s.Mission.State != MissionComplete {
		t.Fatalf("mission %s, want complete", l.s.Mission.State)
	}
	var residue Decision
	for _, d := range decisionsOf(l.decs, DecisionRedecompose) {
		if d.RuleFired == "" && strings.Contains(d.Rationale, "lane-03 walked by DRONE-04 with") {
			residue = d
		}
	}
	if residue.Rationale == "" {
		t.Fatalf("no redecomposition of lane-03's residue in %+v", decisionsOf(l.decs, DecisionRedecompose))
	}
	if _, ok := l.s.LaneByID("lane-03"); ok {
		t.Fatal("lane-03 was redecomposed and should be retired")
	}
}

// A drone crossing open ground from outside the AO to its lane is in transit,
// so the geofence does not trip; once it has reached a waypoint of its lane
// from inside the AO it is scanning.
func TestScanningIsDerivedFromTheLane(t *testing.T) {
	plan := testPlan(t, testAO())
	l := newLoop(t, plan)
	l.tick()
	l.tick()
	v := l.s.Vectors["DRONE-01"]
	if v.Mode != ModeTransit || l.s.InAO(v.Position) {
		t.Fatalf("DRONE-01 mode %s, in AO %v: want transit outside the AO", v.Mode, l.s.InAO(v.Position))
	}
	l.run(200, func(s State) bool { return s.Vectors["DRONE-01"].Mode == ModeScanning })
	if !l.s.InAO(l.s.Vectors["DRONE-01"].Position) {
		t.Fatal("scanning outside the AO")
	}
	for _, d := range decisionsOf(l.decs, DecisionDoctrineRule) {
		t.Fatalf("a rule fired on a plain approach: %+v", d)
	}
}
