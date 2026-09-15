package planir

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dsanchez31/keel/internal/assign"
	"github.com/dsanchez31/keel/internal/coverage"
	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/eventlog"
)

// Input is one validation: the operator's intent, the planner's raw reply and
// what the reply is resolved against.
type Input struct {
	Intent    string
	Raw       []byte
	World     *World
	Doctrines *doctrine.Registry
	// Arrival is the budget of the engine the plan would run under
	// (engine.Config.Arrival): gate 3 refuses an AO whose route clearance
	// does not exceed it, as the engine would at approval. Required.
	Arrival coverage.Arrival
}

// Result is the outcome of a validation. Plan is set exactly when Diagnostics
// is empty.
type Result struct {
	// IR is the decoded Plan IR, set once gate 1 passes.
	IR *Plan `json:"ir,omitempty"`
	// Plan is the expanded, validated, content-addressed plan, ready for the
	// human gate. Its lanes are unassigned: the engine allocates at launch
	// against the fleet as it is then.
	Plan *domain.ApprovedPlan `json:"plan,omitempty"`
	// Assignments is the allocation gate 3 projected over the snapshot, for
	// display beside the plan. It is not part of the plan or of its hash.
	Assignments []assign.Assignment `json:"assignments,omitempty"`
	// Diagnostics are the violations of the first gate that failed, sorted.
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// OK reports whether the reply produced a plan.
func (r Result) OK() bool { return r.Plan != nil }

// Validate runs the four gates of spec section 5.3 in order: schema,
// resolution, feasibility, doctrine constraints. The first gate that fails
// stops the pipeline and reports every violation it found.
//
// It never relaxes anything to let a plan through: the same input always
// produces the same result, and the repair loop can only change the reply.
func Validate(in Input) Result {
	ir, diags := checkSchema(in.Raw)
	if len(diags) > 0 {
		return Result{Diagnostics: diags}
	}
	res := Result{IR: ir}

	r, diags := resolve(in, ir)
	if len(diags) > 0 {
		res.Diagnostics = sortDiagnostics(diags)
		return res
	}

	f, diags := feasibility(in.World, in.Arrival, ir, r)
	if len(diags) > 0 {
		res.Diagnostics = sortDiagnostics(diags)
		return res
	}
	res.Assignments = f.assignments

	if diags := constraints(in.World, r, f); len(diags) > 0 {
		res.Diagnostics = sortDiagnostics(diags)
		return res
	}

	plan, err := approvedPlan(ir, r, f)
	if err != nil {
		res.Diagnostics = []Diagnostic{{Gate: GateFeasibility, Code: "unencodable", Message: err.Error()}}
		return res
	}
	res.Plan = plan
	return res
}

// resolved is what gate 2 looked up.
type resolved struct {
	pack     *doctrine.Pack
	area     AreaSpec
	stations []domain.Station // the named GCS, or none
}

// resolve is gate 2: every name in the plan must exist server side. An unknown
// name is a failure that lists the names that would have resolved, never an
// invitation to guess.
func resolve(in Input, ir *Plan) (resolved, []Diagnostic) {
	var r resolved
	var diags []Diagnostic

	if strings.TrimSpace(ir.Intent) != strings.TrimSpace(in.Intent) {
		diags = append(diags, Diagnostic{
			Gate:    GateResolution,
			Code:    "intent_mismatch",
			Pointer: "/intent",
			Message: fmt.Sprintf("intent must echo the operator's text verbatim: %q", strings.TrimSpace(in.Intent)),
		})
	}

	refs := in.Doctrines.Refs()
	valid := make([]string, len(refs))
	for i, ref := range refs {
		valid[i] = ref.String()
	}
	ref, err := doctrine.ParseRef(ir.Doctrine)
	if err == nil {
		r.pack, err = in.Doctrines.Resolve(ref)
	}
	if err != nil {
		diags = append(diags, Diagnostic{
			Gate:         GateResolution,
			Code:         "unknown_doctrine",
			Pointer:      "/doctrine",
			Message:      fmt.Sprintf("doctrine %q does not name a registered pack", ir.Doctrine),
			Alternatives: valid,
		})
	}

	var ok bool
	if r.area, ok = in.World.Area(ir.Mission.Area); !ok {
		diags = append(diags, Diagnostic{
			Gate:         GateResolution,
			Code:         "unknown_area",
			Pointer:      "/mission/area",
			Message:      fmt.Sprintf("area %q is not known to the server", ir.Mission.Area),
			Alternatives: in.World.AreaNames(),
		})
	}

	if name := ir.Assignment.GCS; name != "" {
		st, ok := in.World.Station(name)
		if ok {
			r.stations = []domain.Station{st}
		} else {
			diags = append(diags, Diagnostic{
				Gate:         GateResolution,
				Code:         "unknown_gcs",
				Pointer:      "/assignment/gcs",
				Message:      fmt.Sprintf("ground station %q is not known to the server", name),
				Alternatives: in.World.StationNames(),
			})
		}
	}
	return r, diags
}

// feasible is what gate 3 computed.
type feasible struct {
	lanes       []domain.Lane
	assignments []assign.Assignment
	params      coverage.Params
}

// feasibility is gate 3. The tactic is expanded with the swath and altitude
// the server derives, then the real allocator runs over the fleet snapshot
// with the plan's policy: a plan passes only when launch would assign every
// lane, which is the range check of spec section 5.7 and nothing weaker.
// First, the AO's route clearance must exceed the engine's arrival budget,
// the check the engine repeats at approval.
func feasibility(w *World, arrival coverage.Arrival, ir *Plan, r resolved) (feasible, []Diagnostic) {
	fail := func(code, ptr, subject, format string, args ...any) (feasible, []Diagnostic) {
		return feasible{}, []Diagnostic{{Gate: GateFeasibility, Code: code, Pointer: ptr, Subject: subject, Message: fmt.Sprintf(format, args...)}}
	}

	if err := arrival.Check(r.area.Area.Grid); err != nil {
		if errors.Is(err, coverage.ErrArrival) {
			return fail("invalid_arrival_budget", "", "", "%v", err)
		}
		return fail("clearance_too_small", "/mission/area", r.area.Area.Name,
			"%s cannot be flown by this engine: %v", r.area.Area.Name, err)
	}

	requires := ir.Assignment.Requires
	var capable []FleetVector
	for _, v := range w.Fleet {
		if v.Caps.Satisfies(requires) && v.State.Available() {
			capable = append(capable, v)
		}
	}
	if len(capable) == 0 {
		return fail("no_capable_vector", "/assignment/requires", "",
			"no available vector declares every tag of %v", requires)
	}

	// The narrowest footprint sets the spacing between passes, so whichever
	// capable vector flies a lane leaves no gap between its passes.
	swath := 2 * capable[0].Caps.SensorRadiusM
	narrowest := capable[0].Caps.ID
	for _, v := range capable[1:] {
		if s := 2 * v.Caps.SensorRadiusM; s < swath {
			swath, narrowest = s, v.Caps.ID
		}
	}
	if !(swath > 0) {
		return fail("no_sensor_footprint", "/assignment/requires", string(narrowest),
			"%s satisfies %v but declares no sensor footprint, so its lanes cannot be spaced", narrowest, requires)
	}

	params := coverage.Params{SwathM: swath, AltM: r.area.ScanAltM}
	lanes, err := coverage.Decompose(r.area.Area, ir.Tactic.Coverage(), params)
	if err != nil {
		var empty *coverage.EmptyLaneError
		switch {
		case errors.As(err, &empty):
			return fail("empty_lane", "/tactic/lanes", fmt.Sprintf("lane-%02d", empty.Index),
				"lane %d of %d holds no cell of %s: use fewer lanes", empty.Index, ir.Tactic.Lanes, r.area.Area.Name)
		case errors.Is(err, coverage.ErrUnsupportedPattern):
			return fail("unsupported_pattern", "/tactic/pattern", "",
				"pattern %s has no expansion yet, only parallel_lanes does", ir.Tactic.Pattern)
		case errors.Is(err, coverage.ErrInvalidTactic):
			return fail("invalid_tactic", "/tactic", "", "%v", err)
		default:
			return fail("expansion_failed", "", r.area.Area.Name, "%v", err)
		}
	}

	var diags []Diagnostic
	for _, l := range lanes {
		for i, wp := range l.Waypoints {
			if c, ok := r.area.Area.Grid.CellOf(wp); !ok || !r.area.Area.Grid.IsInAO(c) {
				diags = append(diags, Diagnostic{
					Gate: GateFeasibility, Code: "waypoint_outside_ao", Subject: string(l.ID),
					Message: fmt.Sprintf("waypoint %d of %s lies outside %s", i, l.ID, r.area.Area.Name),
				})
			}
		}
	}
	if len(capable) < len(lanes) {
		diags = append(diags, Diagnostic{
			Gate: GateFeasibility, Code: "insufficient_vectors", Pointer: "/tactic/lanes",
			Message: fmt.Sprintf("%d lanes need %d vectors declaring %v, %d are available", len(lanes), len(lanes), requires, len(capable)),
		})
	}
	if len(diags) > 0 {
		return feasible{}, diags
	}

	fleet := make([]assign.Vector, len(w.Fleet))
	for i, v := range w.Fleet {
		fleet[i] = assign.Vector{Caps: v.Caps, State: v.State, FreeAt: v.State.Position}
	}
	as, err := assign.Allocate(assign.Request{
		Policy:     ir.Assignment.Policy,
		Requires:   requires,
		Lanes:      lanes,
		Fleet:      fleet,
		Stations:   r.stations,
		ReservePct: r.pack.Params.BatteryReservePct,
	})
	if err != nil {
		return fail("allocation_failed", "/assignment", "", "%v", err)
	}
	for _, a := range as {
		if a.Winner == "" {
			diags = append(diags, Diagnostic{
				Gate: GateFeasibility, Code: "lane_unassigned", Subject: string(a.Lane),
				Message: a.Rationale, Candidates: a.Candidates,
			})
		}
	}
	if len(diags) > 0 {
		return feasible{}, diags
	}
	return feasible{lanes: lanes, assignments: as, params: params}, nil
}

// constraints is gate 4: every constraint of the pack, for every vector not
// written off, against the state the plan projects at launch. That state is
// the snapshot with the lanes assigned as gate 3 allocated them, the mission
// awaiting approval and nothing explored yet. A vector in mode down is out of
// the envelope by definition (I4) and nothing a plan can repair.
func constraints(w *World, r resolved, f feasible) []Diagnostic {
	projected := domain.CloneLanes(f.lanes)
	for i := range projected {
		for _, a := range f.assignments {
			if a.Lane == projected[i].ID {
				projected[i].AssignedTo = a.Winner
			}
		}
	}

	env := doctrine.Env{
		Mission: doctrine.BindMission(domain.Mission{Area: r.area.Area, State: domain.MissionAwaitingApproval}),
	}
	for _, v := range w.Fleet {
		if v.State.Mode == domain.ModeDown {
			continue
		}
		env.Agents = append(env.Agents, doctrine.BindAgent(v.Caps, v.State, r.stations, projected, nil))
	}

	violations, err := doctrine.CheckConstraints(r.pack, env)
	if err != nil {
		return []Diagnostic{{Gate: GateDoctrine, Code: "invalid_state", Message: err.Error()}}
	}
	diags := make([]Diagnostic, len(violations))
	for i, v := range violations {
		c, _ := r.pack.Constraint(v.Constraint)
		diags[i] = Diagnostic{
			Gate:    GateDoctrine,
			Code:    "constraint_violated",
			Pointer: "/doctrine",
			Subject: v.Constraint,
			Message: fmt.Sprintf("%s violates constraint %s of %s: %s", v.Agent, v.Constraint, r.pack.Ref, c.Rule),
		}
	}
	return diags
}

// approvedPlan builds the plan that crosses the fence and content-addresses
// it (spec section 5.6). The hash covers everything but itself and the
// mission id, which is assigned when the plan is approved. The pack gate 2
// resolved is pinned by its hash, so the plan names the rules it was
// validated against and not only their version.
func approvedPlan(ir *Plan, r resolved, f feasible) (*domain.ApprovedPlan, error) {
	p := domain.ApprovedPlan{
		Intent:       ir.Intent,
		Area:         r.area.Area.Clone(),
		Doctrine:     r.pack.Ref,
		DoctrineHash: r.pack.Hash,
		Lanes:        domain.CloneLanes(f.lanes),
		Requires:     ir.Assignment.Requires,
		Policy:       ir.Assignment.Policy,
		GCS:          r.stations,
		Rationale:    ir.Rationale,
		SwathM:       f.params.SwathM,
		ScanAltM:     f.params.AltM,
	}
	h, err := eventlog.HexOf(p)
	if err != nil {
		return nil, err
	}
	p.Hash = domain.PlanHash(h)
	return &p, nil
}
