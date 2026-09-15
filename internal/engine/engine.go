package engine

import (
	"fmt"

	"github.com/dsanchez31/keel/internal/coverage"
	"github.com/dsanchez31/keel/internal/domain"
)

// Step is the whole decision path.
//
//	func Step(s State, in []Event) (State, []Command, []Decision)
//
// It does not know that networks exist. It receives the events that arrived,
// returns the commands to send and the decisions it made, and has no way to
// observe anything else. The daemon owns the loop and holds every impure
// step: drain the inbox, append the decisions, dispatch the commands.
//
// Replay reuses this function unchanged, substituting a recorded event stream
// for the live inbox. There is no second implementation to drift from this
// one, which is the only way a replay guarantee survives maintenance.
//
// The tick pipeline, in order:
//
//  1. advance the logical clock
//  2. apply the tick's events, sorted so wire order cannot leak into a decision
//  3. age links whose telemetry has gone stale
//  4. derive the orchestration modes (scanning, transit, rtb) from lane state
//  5. paint sensor footprints onto the coverage grid
//  6. evaluate doctrine and carry out its actions, then allocate the lanes
//     still unassigned
//  7. advance lane cursors and emit commands
//  8. re-send the commands that have stood unanswered long enough
//  9. settle the mission state
//
// The order is load bearing. Modes are derived before doctrine reads them,
// coverage is painted before assignment so an assignment sees the coverage its
// own vectors just produced, and the mission settles last so completion is
// judged on the tick's final state.
func Step(s State, in []Event) (State, []Command, []Decision) {
	s = s.Clone()
	s.Clock = s.Clock.Advance()

	var decs []Decision
	var cmds []Command

	s, cmds, decs = applyEvents(s, domain.SortEvents(in), cmds, decs)
	s = ageLinks(s)
	s = deriveModes(s)
	s = paintCoverage(s)
	s, cmds, decs = react(s, cmds, decs)
	s, cmds = advanceLanes(s, cmds)
	s, cmds = resendCommands(s, cmds)
	s, decs = settleMission(s, decs)

	return s, domain.SortCommands(cmds), decs
}

// TickResult is one tick's output, kept together so a driver can hand a whole
// tick to a log, a UI or an assertion without re-associating three slices.
type TickResult struct {
	State     State
	Commands  []Command
	Decisions []Decision
}

// Run drives Step over a sequence of per-tick event batches.
//
// This is the pure half of the tick loop. The daemon's loop is the impure
// half: it decides when to call Step and what to do with the output. Replay
// and the DST harness use Run, feeding recorded or generated batches, so
// neither of them contains a second copy of the pipeline.
//
// visit, if non-nil, is called after every tick with that tick's result. It
// is where an invariant assertion or a log append belongs.
func Run(s State, batches [][]Event, visit func(TickResult)) State {
	for _, batch := range batches {
		next, cmds, decs := Step(s, batch)
		s = next
		if visit != nil {
			visit(TickResult{State: s, Commands: cmds, Decisions: decs})
		}
	}
	return s
}

// RunUntil drives Step until stop reports true or maxTicks is reached.
//
// maxTicks is not optional. A loop that runs until a predicate holds is a
// loop that can run forever when the predicate never holds, and "the mission
// never deadlocks" (I8) is a property to be proved, not assumed by the driver
// that is supposed to detect its absence.
func RunUntil(s State, next func(tick int64) []Event, stop func(State) bool, maxTicks int64, visit func(TickResult)) State {
	for i := int64(0); i < maxTicks; i++ {
		var batch []Event
		if next != nil {
			batch = next(s.Clock.Tick + 1)
		}
		st, cmds, decs := Step(s, batch)
		s = st
		if visit != nil {
			visit(TickResult{State: s, Commands: cmds, Decisions: decs})
		}
		if stop != nil && stop(s) {
			break
		}
	}
	return s
}

// applyEvents folds the tick's events into state.
//
// Events are the only way anything from outside reaches a decision. They
// arrive already sorted by kind then vector id, so the order the inbox
// happened to drain them in cannot change the outcome. Reordering the wire is
// one of the faults the DST harness injects, and this is why it is survivable.
// Only an operator abort issues commands here: the stop it orders cannot wait
// for the lane stage, which a mission no longer running skips.
func applyEvents(s State, events []Event, cmds []Command, decs []Decision) (State, []Command, []Decision) {
	for _, ev := range events {
		switch ev.Kind {
		case EventVectorJoined:
			s, decs = applyVectorJoined(s, ev, decs)
		case EventVectorLeft:
			s, decs = applyVectorLeft(s, ev, decs)
		case EventTelemetry:
			s = applyTelemetry(s, ev)
		case EventLinkChanged:
			s = applyLinkChanged(s, ev)
		case EventPlanApproved:
			s, decs = applyPlanApproved(s, ev, decs)
		case EventDoctrineSwap:
			s, decs = applyDoctrineSwap(s, ev, decs)
		case EventFaultInjected:
			s, decs = applyFault(s, ev, decs)
		case EventOperatorAbort:
			s, cmds, decs = applyAbort(s, ev, cmds, decs)
		}
	}
	return s, cmds, decs
}

// applyVectorJoined admits a vector with its capabilities and its initial
// state, both required. A join without a state, or with a position that is
// not finite, is refused with a decision: admitting it would put a vector in
// state that doctrine evaluates and allocation weighs with nothing true to
// say about it.
//
// A vector already known that joins again is re-admitted with the state it
// carries. It is how a vector written off comes back: an explicit act, never
// a telemetry frame that happened to arrive.
func applyVectorJoined(s State, ev Event, decs []Decision) (State, []Decision) {
	if ev.Caps == nil || ev.Caps.ID == "" {
		return s, decs
	}
	id := ev.Caps.ID
	if ev.Telemetry == nil || !ev.Telemetry.Position.Finite() {
		return s, append(decs, refusalDecision(s.Clock.TickMs, DecisionOperatorNotice, string(id),
			fmt.Sprintf("join of %s refused: it carries no initial state with a finite position", id)))
	}
	caps := ev.Caps.Clone()
	caps.SortTags()
	s.Caps[id] = caps

	st := *ev.Telemetry
	st.ID = id
	st.Heading = domain.NormaliseDeg(st.Heading)
	if st.LastSeenMs == 0 {
		st.LastSeenMs = s.Clock.TickMs
	}
	s.Vectors[id] = st
	return s, decs
}

func applyVectorLeft(s State, ev Event, decs []Decision) (State, []Decision) {
	v, ok := s.Vectors[ev.Vector]
	if !ok {
		return s, decs
	}
	v.Link = LinkLost
	v.Mode = ModeDown
	s.Vectors[ev.Vector] = v
	s, released := releaseLanesOf(s, ev.Vector)
	if len(released) > 0 {
		// A notice, not a reassignment: the choice of who takes the lanes
		// is the allocation round that follows, with its own candidates.
		decs = append(decs, refusalDecision(s.Clock.TickMs, DecisionOperatorNotice, string(ev.Vector),
			fmt.Sprintf("%s left the fleet, %s released for redistribution", ev.Vector, laneList(released))))
	}
	return s, decs
}

// applyTelemetry accepts one frame.
//
// A frame older than the one already held is dropped rather than applied.
// Duplication and reordering are faults the harness injects, and accepting a
// stale frame would walk a vector backwards, which the coverage grid would
// then record as real.
func applyTelemetry(s State, ev Event) State {
	if ev.Telemetry == nil {
		return s
	}
	in := *ev.Telemetry
	if in.ID == "" {
		in.ID = ev.Vector
	}
	if in.ID == "" || !in.Position.Finite() {
		return s
	}
	// A vehicle that knows no mission time (an autopilot, keelsim --serve)
	// sends 0, and the frame is stamped with the tick that accepts it. The
	// stamp comes before the ordering check: 0 is not a time older than the
	// join, and comparing it as one would drop every such frame after the
	// first.
	if in.LastSeenMs == 0 {
		in.LastSeenMs = s.Clock.TickMs
	}
	if prev, ok := s.Vectors[in.ID]; ok && in.LastSeenMs < prev.LastSeenMs {
		return s
	}
	prev, known := s.Vectors[in.ID]
	if known && prev.Mode == ModeDown {
		// A vector written off does not come back on its own. It rejoins
		// through EventVectorJoined, which is an explicit operator-visible
		// act rather than a frame that happened to arrive.
		return s
	}
	in.Heading = domain.NormaliseDeg(in.Heading)
	s.Vectors[in.ID] = in
	return s
}

func applyLinkChanged(s State, ev Event) State {
	v, ok := s.Vectors[ev.Vector]
	if !ok || ev.Link == "" {
		return s
	}
	v.Link = ev.Link
	s.Vectors[ev.Vector] = v
	return s
}

// applyPlanApproved is the human gate opening. Everything below the fence
// starts here, and the plan it starts from is content addressed, so what runs
// is provably what was approved.
//
// The plan's doctrine must resolve in the registry to the pack gate 2 pinned.
// A registry that no longer knows it, or knows another pack under the same
// reference, means the mission would run under rules nobody validated it
// against, so the approval is refused and the mission does not start. So is
// a plan whose grid's route clearance does not exceed the arrival radius plus
// the position error: its vectors would trip the geofence on their own
// routes.
func applyPlanApproved(s State, ev Event, decs []Decision) (State, []Decision) {
	if ev.Plan == nil {
		return s, decs
	}
	plan := ev.Plan.Clone()
	if s.Doctrines == nil {
		return s, append(decs, refusalDecision(s.Clock.TickMs, DecisionMissionState, string(plan.Hash),
			fmt.Sprintf("plan %s refused: no doctrine registry to resolve %s against", shortHash(plan.Hash), plan.Doctrine)))
	}
	pack, err := resolvePinned(s, plan.Doctrine, plan.DoctrineHash)
	if err == nil {
		// Gate 3 checked the same rule against the budget it was given; this
		// is the budget the mission would run under.
		err = s.Config.Arrival().Check(plan.Area.Grid)
	}
	if err != nil {
		return s, append(decs, refusalDecision(s.Clock.TickMs, DecisionMissionState, string(plan.Hash),
			fmt.Sprintf("plan %s refused: %v", shortHash(plan.Hash), err)))
	}
	s.Pack = pack
	s.Windows = nil
	s.Plan = plan
	s.Mission = Mission{
		ID:        plan.Mission,
		Intent:    plan.Intent,
		Area:      plan.Area.Clone(),
		Doctrine:  plan.Doctrine,
		Plan:      plan.Hash,
		State:     MissionRunning,
		StartedMs: s.Clock.TickMs,
	}
	s.Lanes = domain.CloneLanes(plan.Lanes)
	s.Stations = append([]Station(nil), plan.GCS...)
	s.Cursor = map[LaneID]int{}
	s.Unassigned = map[LaneID]int64{}
	s.Engagement = map[LaneID]Engagement{}
	s.Reported = map[LaneID]bool{}
	s.NextLaneIndex = 0
	for _, l := range s.Lanes {
		s.NextLaneIndex = max(s.NextLaneIndex, l.Index+1)
		s.Cursor[l.ID] = 0
		if l.Assigned() {
			s.Engagement[l.ID] = Engagement{}
		} else {
			s.Unassigned[l.ID] = s.Clock.TickMs
		}
	}
	decs = append(decs, Decision{
		TickMs:  s.Clock.TickMs,
		Kind:    DecisionMissionState,
		Subject: string(plan.Hash),
		Rationale: fmt.Sprintf("plan %s approved, mission running over %s with %d lane(s) under %s",
			shortHash(plan.Hash), plan.Area.Name, len(s.Lanes), PackLabel(pack.Ref, pack.Hash)),
	})
	return s, decs
}

func applyFault(s State, ev Event, decs []Decision) (State, []Decision) {
	if ev.Fault == nil {
		return s, decs
	}
	f := *ev.Fault
	v, ok := s.Vectors[f.Vector]
	if !ok {
		return s, decs
	}
	note := ""
	switch f.Kind {
	case FaultKill:
		v.Mode = ModeDown
		v.Link = LinkLost
		v.Speed = 0
		// A vector written off is outside doctrine evaluation (I4 scope), so
		// no rule will ever release its lanes: the kill does, as a vector
		// leaving the fleet does, and the allocation round redistributes them.
		var released []LaneID
		s, released = releaseLanesOf(s, f.Vector)
		if len(released) > 0 {
			note = ", " + laneList(released) + " released for redistribution"
		}
	case FaultLinkLoss:
		v.Link = LinkLost
	case FaultBatteryDrain:
		v.BatteryPct = clampPct(v.BatteryPct - int(f.Magnitude))
	case FaultGPSDrift, FaultStaleTelemetry:
		// The simulator applies these to the frames it produces. The engine
		// records the injection so replay reproduces the fault itself, not
		// merely the telemetry it happened to cause.
	}
	s.Vectors[f.Vector] = v
	decs = append(decs, Decision{
		TickMs:    s.Clock.TickMs,
		Kind:      DecisionOperatorNotice,
		Subject:   string(f.Vector),
		Rationale: fmt.Sprintf("fault %s injected on %s%s", f.Kind, f.Vector, note),
	})
	return s, decs
}

// applyAbort ends the mission from the operator's side and stops every vector
// still flying to a point the mission gave it.
//
// A failed mission advances no lane and re-sends nothing, so without a stop a
// vector would fly on to its last waypoint with nobody commanding it, and an
// orchestrator shutting down would leave its fleet in flight. A vector whose
// standing command is a goto gets an abort (stop where it is); one holding
// is stopped already; one returning to base keeps returning, since stopping
// it would strand it on the battery it went home to save. A vector written
// off is not reachable and is left alone.
func applyAbort(s State, ev Event, cmds []Command, decs []Decision) (State, []Command, []Decision) {
	if s.Mission.State.Terminal() {
		return s, cmds, decs
	}
	s.Mission.State = MissionFailed
	reason := ev.Reason
	if reason == "" {
		reason = "operator abort"
	}
	var stopped []string
	for _, id := range SortedKeys(s.Issued) {
		if s.Issued[id].Command.Type != CommandGoto || s.Vectors[id].Mode == ModeDown {
			continue
		}
		s, cmds = issue(s, cmds, id, Command{Vector: id, Type: CommandAbort}, "abort")
		stopped = append(stopped, string(id))
	}
	decs = append(decs, Decision{
		TickMs:    s.Clock.TickMs,
		Kind:      DecisionMissionState,
		Subject:   string(s.Mission.ID),
		Rationale: fmt.Sprintf("mission failed: %s, %s stopped", reason, listOrNone(stopped)),
	})
	return s, cmds, decs
}

// ageLinks downgrades a link that has stopped producing frames.
//
// The threshold is in ticks of mission time, not seconds of wall clock, so a
// headless-fast run and a real-time run age links identically.
func ageLinks(s State) State {
	if s.Config.StaleTelemetryTicks <= 0 {
		return s
	}
	staleMs := TicksToMs(s.Config.StaleTelemetryTicks)
	for _, id := range s.VectorIDs() {
		v := s.Vectors[id]
		if v.Mode == ModeDown {
			continue
		}
		age := s.Clock.TickMs - v.LastSeenMs
		switch {
		case age >= 2*staleMs:
			v.Link = LinkLost
		case age >= staleMs:
			if v.Link == LinkOK {
				v.Link = LinkDegraded
			}
		}
		s.Vectors[id] = v
	}
	return s
}

// paintCoverage marks every AO cell inside a sensor footprint as explored.
//
// The painting itself is coverage.Paint, the one implementation of the fog of
// war. Vectors are visited in ascending id order, and Step has already cloned
// the state, so painting in place never reaches a caller's snapshot.
func paintCoverage(s State) State {
	g := s.Mission.Area.Grid
	if g.Count() == 0 {
		return s
	}
	for _, id := range s.VectorIDs() {
		v := s.Vectors[id]
		if v.Link == LinkLost || v.Mode == ModeDown || v.Mode == ModeIdle {
			continue
		}
		caps, ok := s.Caps[id]
		if !ok || caps.SensorRadiusM <= 0 {
			continue
		}
		coverage.Paint(g, v.Position, caps.SensorRadiusM)
	}
	return s
}

// advanceLanes moves each vector along its active lane and emits the commands
// whose target changed.
//
// A vector holding several lanes works them in ascending Index order (spec
// section 4.4): only its active lane, the first with a waypoint left, is
// driven, and the lanes queued behind it wait. A vector whose lanes are all
// walked holds.
//
// A goto is sent when the target waypoint changes, not on every tick. Sending
// the same waypoint ten times a second would be correct (commands are
// idempotent by Seq) and would also bury the interesting traffic, so the
// engine records what it last asked each vector for, and resendCommands
// repeats it only at the configured interval.
func advanceLanes(s State, cmds []Command) (State, []Command) {
	if s.Mission.State != MissionRunning {
		return s, cmds
	}
	for _, id := range s.VectorIDs() {
		v := s.Vectors[id]
		if !v.Available() {
			continue
		}
		last := -1
		active := -1
		for i := range s.Lanes {
			lane := s.Lanes[i]
			if lane.AssignedTo != id {
				continue
			}
			last = i
			cursor := s.Cursor[lane.ID]
			// Consume every waypoint already reached this tick. A fast
			// vector over a fine lane can pass more than one between
			// frames, and stopping at the first would leave the cursor
			// permanently behind the vehicle. Finishing one lane moves on to
			// the next, whose first waypoint may be reached already.
			for cursor < len(lane.Waypoints) &&
				domain.HaversineM(v.Position, lane.Waypoints[cursor]) <= s.Config.ArrivalRadiusM {
				cursor++
			}
			s.Cursor[lane.ID] = cursor
			if cursor < len(lane.Waypoints) {
				active = i
				break
			}
		}
		if last < 0 {
			continue
		}

		if active < 0 {
			// Every lane it holds is walked. The vector holds rather than
			// returning to base: redecomposition may hand it uncovered
			// cells, and a vector already in the AO is the cheapest one to
			// hand them to.
			lane := s.Lanes[last]
			s, cmds = issue(s, cmds, id, Command{Vector: id, Type: CommandHold, Lane: lane.ID}, string(lane.ID)+"#done")
			continue
		}

		lane := s.Lanes[active]
		cursor := s.Cursor[lane.ID]
		e := s.Engagement[lane.ID]
		if !e.Engaged && cursor > e.FromCursor && s.InAO(v.Position) {
			// It has reached a waypoint of this lane, and is over the AO:
			// from here on it is doing lane work, and the geofence applies.
			e.Engaged = true
			s.Engagement[lane.ID] = e
		}
		wp := lane.Waypoints[cursor]
		s, cmds = issue(s, cmds, id, Command{
			Vector:   id,
			Type:     CommandGoto,
			Waypoint: &wp,
			Lane:     lane.ID,
		}, fmt.Sprintf("%s#%d", lane.ID, cursor))
	}
	return s, cmds
}

// issue emits a command only when its target differs from the one the vector
// was last given, and stamps it with the next per-vector sequence number.
func issue(s State, cmds []Command, id VectorID, cmd Command, target string) (State, []Command) {
	if prev, ok := s.Issued[id]; ok && prev.Target == target {
		return s, cmds
	}
	s.CmdSeq[id]++
	cmd.Seq = s.CmdSeq[id]
	s.Issued[id] = IssuedCommand{Target: target, Command: cloneCommand(cmd), SentTick: s.Clock.Tick}
	return s, append(cmds, cmd)
}

// resendCommands sends again, with its original Seq, every command that has
// stood for ResendTicks without being superseded or acknowledged.
//
// A lost goto leaves the vehicle holding at its previous waypoint, where the
// cursor never advances, and the mission stalls without a single fault to
// show for it. Telemetry carries the last Seq the vehicle applied, so a
// command it acknowledged is not repeated; one it has not is, and
// re-delivering an applied Seq is a no-op anyway (spec section 7.2). The
// acknowledgement is read from the latest frame each time, not remembered: a
// vehicle that restarts reports 0 again and gets its standing command back. A
// vector whose link is lost is skipped: the command could not reach it, and
// it is re-sent on the first tick the link is back.
func resendCommands(s State, cmds []Command) (State, []Command) {
	if s.Config.ResendTicks <= 0 || s.Mission.State != MissionRunning {
		return s, cmds
	}
	for _, id := range SortedKeys(s.Issued) {
		is := s.Issued[id]
		v, ok := s.Vectors[id]
		if !ok || v.Mode == ModeDown || v.Link == LinkLost || s.Clock.Tick-is.SentTick < s.Config.ResendTicks {
			continue
		}
		if v.AckSeq >= is.Command.Seq {
			continue
		}
		is.SentTick = s.Clock.Tick
		s.Issued[id] = is
		cmds = append(cmds, cloneCommand(is.Command))
	}
	return s, cmds
}

// cloneCommand copies the waypoint a command points to, so a command kept in
// state and one handed to the dispatcher never share it.
func cloneCommand(c Command) Command {
	if c.Waypoint != nil {
		wp := *c.Waypoint
		c.Waypoint = &wp
	}
	return c
}

// deriveModes sets the orchestration modes telemetry cannot know.
//
// An adapter reports what the vehicle is physically doing: idle, or moving to
// a point. Whether that point is lane work is the engine's knowledge, so a
// moving vector is scanning when its active lane is engaged and in transit
// otherwise. A vector the engine sent home is in mode rtb whatever its frames
// say, because a frame sent before the rtb command arrived would otherwise
// make it available for work again. Idle and down are left as they are.
func deriveModes(s State) State {
	for _, id := range s.VectorIDs() {
		v := s.Vectors[id]
		switch {
		case v.Mode == ModeDown:
			continue
		case s.Issued[id].Command.Type == CommandRTB:
			v.Mode = ModeRTB
		case v.Mode == ModeTransit || v.Mode == ModeScanning:
			v.Mode = ModeTransit
			if i := s.ActiveLane(id); i >= 0 && s.Engagement[s.Lanes[i].ID].Engaged {
				v.Mode = ModeScanning
			}
		}
		s.Vectors[id] = v
	}
	return s
}

// releaseLanesOf marks every lane held by a vector as unassigned, retaining
// the cells it already explored, and returns the lanes released in Index
// order.
//
// This is the release_lanes doctrine action and the reaction to a vector
// leaving or being killed, sharing one implementation. Coverage survives: the
// point of releasing a lane is to redistribute what is left of it, not to
// redo it. The cursor survives too, so whoever takes the lane next starts
// where its previous owner stopped.
func releaseLanesOf(s State, id VectorID) (State, []LaneID) {
	var released []LaneID
	for i := range s.Lanes {
		if s.Lanes[i].AssignedTo != id {
			continue
		}
		released = append(released, s.Lanes[i].ID)
		s = releaseLane(s, i)
	}
	return s, released
}

// releaseLane unassigns the lane at index i of Lanes.
func releaseLane(s State, i int) State {
	lid, owner := s.Lanes[i].ID, s.Lanes[i].AssignedTo
	s.Lanes[i].AssignedTo = ""
	s.Unassigned[lid] = s.Clock.TickMs
	delete(s.Engagement, lid)
	delete(s.Reported, lid)
	// A command toward a lane the vector no longer holds must not be re-sent:
	// a vector whose link comes back would fly off to work that is someone
	// else's. An rtb command names no lane and stands.
	if is, ok := s.Issued[owner]; ok && is.Command.Lane == lid {
		delete(s.Issued, owner)
	}
	return s
}

// laneList renders lane ids for a rationale.
func laneList(ids []LaneID) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return listOrNone(out)
}

// settleMission decides whether the mission has finished, failed or should
// keep running, and records the transition.
//
// I8 lives here. A mission reaches complete or failed in bounded time because
// every path out is bounded: coverage completes, no eligible vector works a
// lane for the stall deadline, or the tick ceiling is hit. The stall covers a
// fleet gone dark and work none of the fleet can reach alike, and waits while
// any lane is being worked: a mission that can still explore is not failed
// for the lanes it cannot.
func settleMission(s State, decs []Decision) (State, []Decision) {
	if s.Mission.State != MissionRunning {
		return s, decs
	}

	explored, total := s.Grid().Coverage()
	if total > 0 && explored == total {
		s.Mission.State = MissionComplete
		return s, append(decs, Decision{
			TickMs:    s.Clock.TickMs,
			Kind:      DecisionMissionState,
			Subject:   string(s.Mission.ID),
			Rationale: fmt.Sprintf("coverage complete: %d of %d cells explored", explored, total),
		})
	}

	if s.Working() {
		s.StalledTicks = 0
	} else {
		s.StalledTicks++
	}

	switch {
	case s.Config.StallDeadlineTicks > 0 && s.StalledTicks >= s.Config.StallDeadlineTicks:
		s.Mission.State = MissionFailed
		decs = append(decs, Decision{
			TickMs:  s.Clock.TickMs,
			Kind:    DecisionMissionState,
			Subject: string(s.Mission.ID),
			Rationale: fmt.Sprintf("mission failed: no eligible vector has worked a lane for %d ticks, %d eligible, %s pending, %d of %d cells explored",
				s.StalledTicks, len(s.EligibleVectors()), laneList(laneIDs(s.PendingLanes())), explored, total),
		})
	case s.Config.MaxTicks > 0 && s.Clock.Tick >= s.Config.MaxTicks:
		s.Mission.State = MissionFailed
		decs = append(decs, Decision{
			TickMs:    s.Clock.TickMs,
			Kind:      DecisionMissionState,
			Subject:   string(s.Mission.ID),
			Rationale: fmt.Sprintf("mission failed: tick ceiling %d reached with %d of %d cells explored", s.Config.MaxTicks, explored, total),
		})
	}
	return s, decs
}

// react is the doctrine and allocation stage of the tick.
//
// It is defined in react.go so that the tick pipeline above stays readable as
// a pipeline. Everything it does is a pure function of the state it is given.

func clampPct(v int) int {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}

func shortHash(h PlanHash) string {
	if len(h) <= 12 {
		return string(h)
	}
	return string(h[:12])
}
