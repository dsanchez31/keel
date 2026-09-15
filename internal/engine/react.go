package engine

import (
	"errors"
	"fmt"

	"github.com/dsanchez31/keel/internal/assign"
	"github.com/dsanchez31/keel/internal/coverage"
	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
)

// react is the doctrine and allocation stage of a tick.
//
// Doctrine is evaluated first: constraints, then rules, for every vector not
// written off, and the actions of every rule that fired are carried out in
// the order written. Then every lane still unassigned is offered to the fleet
// in one allocation round under the plan's policy. That round is the launch
// allocation on the tick the plan is approved, and afterwards the guarantee
// behind I3: a lane released by a vector leaving, a kill, or a rule that did
// not redecompose is reassigned on the tick an eligible vector can take it,
// without waiting for a rule to notice.
//
// Between the two, a lane walked to its last waypoint with cells still
// uncovered is released and its residue redecomposed: see sweepResidue.
//
// Nothing here runs outside a running mission: before approval there is no
// pack and no lane, and after completion there is nothing left to decide.
func react(s State, cmds []Command, decs []Decision) (State, []Command, []Decision) {
	if s.Mission.State != MissionRunning || s.Pack == nil {
		return s, cmds, decs
	}
	s, cmds, decs = evaluateDoctrine(s, cmds, decs)
	s, decs = sweepResidue(s, decs)
	s, decs = allocatePending(s, decs)
	return s, cmds, decs
}

// sweepResidue hands on the cells a walked lane left uncovered.
//
// Expansion puts every cell within half a swath of its lane's path, which is
// the footprint's edge itself: a position reported a metre off, or a detour
// around a concavity, can leave a cell just outside every footprint. A lane
// walked to its end with such a residue would
// otherwise stay assigned forever and the mission would never complete (I8).
// The lane is released, and once a vector can take work the residue is
// redecomposed under the plan's policy. Every lane a redecomposition cuts
// visits the centre of a pooled cell, so the residue strictly shrinks and the
// sweep converges rather than repeating.
func sweepResidue(s State, decs []Decision) (State, []Decision) {
	g := s.Grid()
	var walked []string
	residue := false
	for i := range s.Lanes {
		l := s.Lanes[i]
		if s.Cursor[l.ID] < len(l.Waypoints) {
			continue
		}
		explored, total := coverage.LaneCoverage(g, l)
		if explored == total {
			continue
		}
		residue = true
		if !l.Assigned() {
			continue
		}
		owner := l.AssignedTo
		s = releaseLane(s, i)
		note := fmt.Sprintf("%s walked by %s with %d uncovered cell(s)", l.ID, owner, total-explored)
		walked = append(walked, note)
		decs = append(decs, refusalDecision(s.Clock.TickMs, DecisionOperatorNotice, string(l.ID), note+", released for redecomposition"))
	}
	if !residue || len(s.EligibleVectors()) == 0 {
		// Without a vector to hand it to, the residue waits unassigned; the
		// stall deadline bounds the wait.
		return s, decs
	}
	reason := "residue of walked lanes"
	if len(walked) > 0 {
		reason = listOrNone(walked)
	}
	s, _, ds := redecompose(s, s.Plan.Policy, "", reason)
	return s, append(decs, ds...)
}

// evaluateDoctrine runs one tick of the active pack and carries out what it
// fired.
//
// Every vector the engine knows is evaluated except those in mode down, the
// scope gate 4 checks and I4 asserts. The window state goes in and comes back
// out of the evaluator, so a condition held across ticks is measured in
// mission time and nothing else.
func evaluateDoctrine(s State, cmds []Command, decs []Decision) (State, []Command, []Decision) {
	res, err := doctrine.Evaluate(s.Pack, s.DoctrineEnv(), s.Windows)
	if err != nil {
		// Agent ids come from map keys, so they are unique and non-empty
		// and this cannot happen. If it does, the log says so instead of
		// the tick silently skipping its doctrine.
		return s, cmds, append(decs, refusalDecision(s.Clock.TickMs, DecisionOperatorNotice, s.Pack.Ref.String(),
			fmt.Sprintf("doctrine evaluation skipped: %v", err)))
	}
	s.Windows = res.Windows
	for _, f := range res.Firings {
		s, cmds, decs = fire(s, f, cmds, decs)
	}
	return s, cmds, decs
}

// DoctrineEnv binds the state for doctrine: the mission, and every vector the
// engine knows except those in mode down, the scope of gate 4 and I4. It is
// exported so the DST harness checks I4 on the bindings the engine evaluates.
func (s State) DoctrineEnv() doctrine.Env {
	env := doctrine.Env{TickMs: s.Clock.TickMs, Mission: doctrine.BindMission(s.Mission)}
	for _, id := range s.VectorIDs() {
		v := s.Vectors[id]
		caps, ok := s.Caps[id]
		if !ok || v.Mode == ModeDown {
			continue
		}
		env.Agents = append(env.Agents, doctrine.BindAgent(caps, v, s.Stations, s.Lanes, s.Cursor))
	}
	return env
}

// fire carries out one firing's actions, in the order the rule wrote them,
// and records the rule followed by what its actions decided.
//
// The rule's decision comes first so the log reads cause before effect: the
// rule, then the redecomposition it asked for, then who took each new lane.
func fire(s State, f doctrine.Firing, cmds []Command, decs []Decision) (State, []Command, []Decision) {
	effects := make([]string, len(f.Actions))
	var follow []Decision
	for i, a := range f.Actions {
		switch a.Kind {
		case doctrine.ActionReleaseLanes:
			var released []LaneID
			s, released = releaseLanesOf(s, f.Agent)
			effects[i] = "released " + laneList(released)
		case doctrine.ActionRedecompose:
			var ds []Decision
			s, effects[i], ds = redecompose(s, a.Policy, f.RuleID, "")
			follow = append(follow, ds...)
		case doctrine.ActionReturnToBase:
			s, cmds, effects[i] = returnToBase(s, cmds, f.Agent)
		case doctrine.ActionNotifyOperator:
			follow = append(follow, noticeDecision(s.Clock.TickMs, f.Agent, f.RuleID, a.Message))
		}
	}
	decs = append(decs, ruleDecision(s.Clock.TickMs, f, effects))
	return s, cmds, append(decs, follow...)
}

// returnToBase sends a vector to the nearest ground station and puts it in
// mode rtb, which takes it out of every later allocation (spec section 5.7).
// With no station named by the plan, the command carries no waypoint and the
// vehicle returns to its own home.
func returnToBase(s State, cmds []Command, id VectorID) (State, []Command, string) {
	v := s.Vectors[id]
	v.Mode = ModeRTB
	s.Vectors[id] = v

	cmd := Command{Vector: id, Type: CommandRTB}
	effect := "to its home, the plan names no station"
	if st, ok := s.NearestStation(v.Position); ok {
		wp := st.Position
		cmd.Waypoint = &wp
		effect = "to " + st.Name
	}
	s, cmds = issue(s, cmds, id, cmd, "rtb")
	return s, cmds, effect
}

// redecompose pools the uncovered cells of every unassigned lane, cuts them
// into one new lane per eligible vector and allocates the new lanes under
// the action's policy (spec section 6.5.1).
//
// The geometry is the plan's: the swath and altitude fixed at gate 3. A pool
// with no eligible vector to take it is left where it is, unassigned, and the
// per-tick allocation round offers those lanes again as soon as a vector can
// fly them. cause is the rule that asked, reason the engine's own motive
// when no rule did; both end up in the trace.
func redecompose(s State, policy AssignPolicy, cause, reason string) (State, string, []Decision) {
	eligible := s.EligibleVectors()
	res, err := coverage.Redecompose(s.Grid(), s.Lanes, len(eligible), s.NextLaneIndex,
		coverage.Params{SwathM: s.Plan.SwathM, AltM: s.Plan.ScanAltM})
	if err != nil {
		why := err.Error()
		if errors.Is(err, coverage.ErrNoEligibleVector) {
			why = "no eligible vector to take the pool"
		}
		d := refusalDecision(s.Clock.TickMs, DecisionRedecompose, string(policy), "redecomposition deferred: "+why)
		d.RuleFired = cause
		return s, "deferred, " + why, []Decision{d}
	}

	for _, id := range res.Retired {
		delete(s.Cursor, id)
		delete(s.Unassigned, id)
		delete(s.Engagement, id)
		delete(s.Reported, id)
	}
	// Kept lanes are in their original order and every new lane has a
	// higher index, so the concatenation keeps Lanes ordered by Index.
	s.Lanes = append(domain.CloneLanes(res.Kept), domain.CloneLanes(res.New)...)
	for _, l := range res.New {
		s.Cursor[l.ID] = 0
		s.Unassigned[l.ID] = s.Clock.TickMs
		s.NextLaneIndex = max(s.NextLaneIndex, l.Index+1)
	}

	decs := []Decision{redecomposeDecision(s.Clock.TickMs, cause, reason, policy, len(eligible), res)}
	var ds []Decision
	s, ds = allocate(s, res.New, policy, DecisionReassignment, cause)
	decs = append(decs, ds...)

	effect := fmt.Sprintf("cut %d uncovered cell(s) into %s", len(res.Pool), laneList(laneIDs(res.New)))
	if len(res.Pool) == 0 {
		effect = "found no uncovered cell to pool"
	}
	return s, effect, decs
}

// allocatePending offers every unassigned lane with a cell left to explore to
// the fleet, under the plan's policy. On the tick the plan is approved this
// is the launch round and records assignment decisions; afterwards it
// records reassignments.
//
// A lane whose cells are all explored is not offered: flying it would explore
// nothing. Neither is a lane walked to its end: its residue is sweepResidue's
// to redecompose, and handing the lane itself on would have the new owner
// arrive at a cursor already past its last waypoint. Both stay unassigned
// until a redecomposition retires them.
func allocatePending(s State, decs []Decision) (State, []Decision) {
	pending := s.PendingLanes()
	if len(pending) == 0 {
		return s, decs
	}
	kind := DecisionReassignment
	if s.Clock.TickMs == s.Mission.StartedMs {
		kind = DecisionAssignment
	}
	s, ds := allocate(s, pending, s.Plan.Policy, kind, "")
	return s, append(decs, ds...)
}

// allocate runs one allocation round over lanes and applies its winners.
//
// Every lane of the round yields a decision carrying every vector of the
// fleet as a candidate, the losers with their reasons. A lane no vector can
// take is recorded once per stretch of being unassigned: the next rounds
// retry it silently, and the one that succeeds records the assignment.
func allocate(s State, lanes []Lane, policy AssignPolicy, kind DecisionKind, cause string) (State, []Decision) {
	if len(lanes) == 0 {
		return s, nil
	}
	out, err := assign.Allocate(s.AllocationRequest(lanes, policy))
	if err != nil {
		d := refusalDecision(s.Clock.TickMs, kind, string(policy), fmt.Sprintf("allocation refused: %v", err))
		d.RuleFired = cause
		return s, []Decision{d}
	}

	var decs []Decision
	for _, a := range out {
		i := s.laneIndex(a.Lane)
		if i < 0 {
			continue
		}
		if a.Winner == "" {
			if s.Reported[a.Lane] {
				continue
			}
			s.Reported[a.Lane] = true
			decs = append(decs, assignmentDecision(s.Clock.TickMs, kind, cause, a))
			continue
		}
		s.Lanes[i].AssignedTo = a.Winner
		s.Engagement[a.Lane] = Engagement{FromCursor: s.Cursor[a.Lane]}
		delete(s.Unassigned, a.Lane)
		delete(s.Reported, a.Lane)
		decs = append(decs, assignmentDecision(s.Clock.TickMs, kind, cause, a))
	}
	return s, decs
}

// AllocationRequest is the question an allocation round asks the allocator
// about lanes under policy: the plan's requirements, the fleet as the
// allocator sees it, the plan's stations and the active pack's reserve. It is
// exported so the DST harness asks the allocator the engine's own question
// when it checks I3. It needs an active pack.
func (s State) AllocationRequest(lanes []Lane, policy AssignPolicy) assign.Request {
	return assign.Request{
		Policy:     policy,
		Requires:   s.Plan.Requires,
		Lanes:      lanes,
		Fleet:      s.allocationFleet(),
		Stations:   s.Stations,
		ReservePct: s.Pack.Params.BatteryReservePct,
	}
}

// allocationFleet describes every vector that has joined, eligible or not, as
// the allocator sees it. A busy vector becomes free where its last unfinished
// lane ends and still owes the distance to get there: its position to its
// active waypoint, then the remaining waypoints of every lane it holds, in
// lane order. The segments are summed in path order, which is fixed, so the
// total is reproducible without a sort.
func (s State) allocationFleet() []assign.Vector {
	var out []assign.Vector
	for _, id := range s.VectorIDs() {
		caps, ok := s.Caps[id]
		if !ok {
			continue
		}
		v := s.Vectors[id]
		av := assign.Vector{Caps: caps, State: v, FreeAt: v.Position}
		at := v.Position
		for _, l := range s.Lanes {
			if l.AssignedTo != id {
				continue
			}
			av.Holds = append(av.Holds, l.ID)
			for _, wp := range l.Waypoints[min(s.Cursor[l.ID], len(l.Waypoints)):] {
				av.CommittedM += domain.HaversineM(at, wp)
				at = wp
			}
		}
		if v.Position.Finite() {
			av.FreeAt = at
		} else {
			av.CommittedM = 0
		}
		out = append(out, av)
	}
	return out
}

// resolvePinned resolves a doctrine reference in the registry and checks the
// pack against the hash the event expects (spec sections 4.5 and 6.6). The
// pin is required: an event without one is refused like an event whose pack
// differs, so there is no unpinned path for a replay to fall back on.
//
// A pack edited under the same version is how a replay diverges without the
// log saying why. Pinned, the divergence becomes a refusal naming the pack.
func resolvePinned(s State, ref DoctrineRef, hash string) (*doctrine.Pack, error) {
	pack, err := s.Doctrines.Resolve(ref)
	if err != nil {
		return nil, err
	}
	switch hash {
	case "":
		return nil, fmt.Errorf("%s carries no expected pack hash", ref)
	case pack.Hash:
		return pack, nil
	default:
		return nil, fmt.Errorf("registry pack %s has hash %s, %s expected", ref, shortHex(pack.Hash), shortHex(hash))
	}
}

// applyDoctrineSwap performs a hot swap at the tick boundary (spec section
// 6.6).
//
// Events are applied before doctrine is evaluated, so the new pack governs
// this whole tick and the old one never sees it. Duration windows are carried
// across by doctrine.HotSwap, keyed by (rule id, agent). The mission, its
// plan, its lane assignments and its coverage are not doctrine state and are
// not touched, which is invariant I9.
func applyDoctrineSwap(s State, ev Event, decs []Decision) (State, []Decision) {
	if ev.Doctrine == nil {
		return s, decs
	}
	ref := *ev.Doctrine
	// A refusal is a notice, so every doctrine_swap decision in a log is a
	// swap that happened and carries its record.
	refuse := func(reason string) (State, []Decision) {
		return s, append(decs, refusalDecision(s.Clock.TickMs, DecisionOperatorNotice, ref.String(), "hot swap to "+ref.String()+" refused: "+reason))
	}
	if s.Pack == nil {
		return refuse("no doctrine is active, a pack becomes active with an approved plan")
	}
	if s.Doctrines == nil {
		return refuse("no doctrine registry")
	}
	to, err := resolvePinned(s, ref, ev.DoctrineHash)
	if err != nil {
		return refuse(err.Error())
	}
	windows, sw, err := doctrine.HotSwap(s.Pack, to, s.Windows)
	if err != nil {
		return refuse(err.Error())
	}
	s.Pack = to
	s.Windows = windows
	s.Mission.Doctrine = to.Ref
	return s, append(decs, swapDecision(s.Clock.TickMs, sw))
}

func laneIDs(ls []Lane) []LaneID {
	out := make([]LaneID, len(ls))
	for i, l := range ls {
		out[i] = l.ID
	}
	return out
}
