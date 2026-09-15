package doctrine

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/dsanchez31/keel/internal/domain"
)

// ErrInvalidEnv reports an evaluation input the evaluator cannot reason about.
var ErrInvalidEnv = errors.New("doctrine: invalid evaluation input")

// Agent is what the identifiers agent and lane read for one vector.
//
// A value that cannot be known is absent rather than zero: every comparison
// that reads an absent value is false, != included, and so is within. An agent
// holding no lane therefore never satisfies a condition on lane.*, and a
// vector with no finite position never satisfies one on its position.
type Agent struct {
	ID         domain.VectorID
	Domain     domain.Domain
	BatteryPct int
	// RTBCostPct is the battery percentage needed to reach the nearest
	// station, rounded up. Absent when RTBCostKnown is false.
	RTBCostPct   int
	RTBCostKnown bool
	Link         domain.LinkState
	Mode         domain.VectorMode
	Position     domain.Position // absent when not finite
	Speed        float64
	Lane         *LaneBinding // nil when the agent holds no lane
}

// LaneBinding is what lane reads: the first lane the agent holds, by Index.
type LaneBinding struct {
	ID          domain.LaneID
	Index       int
	ProgressPct float64
}

// MissionBinding is what mission reads.
type MissionBinding struct {
	Area        domain.Area
	CoveragePct float64
	State       domain.MissionState
}

// Env is one evaluation's input: the mission time of the tick, the mission
// and every agent to evaluate.
type Env struct {
	TickMs  int64
	Mission MissionBinding
	Agents  []Agent // any order, ids unique
}

// maxRTBCostPct bounds the rounded cost before it becomes an int. Any value
// above 100 already means "unreachable on a full battery"; the bound only
// keeps the conversion defined on every platform.
const maxRTBCostPct = 1_000_000

// BindAgent derives an agent's binding from the domain types. Plan
// validation (gate 4) and the engine both call it, so a derived quantity like
// rtb_cost_pct has one definition.
//
// lanes are the current lanes; the agent's lane is the lowest-Index one
// assigned to it. cursor holds each lane's next waypoint index and may be nil
// before execution starts. rtb_cost_pct is 0 when there is no station, and
// absent when the position is not finite or the vector declares no range.
func BindAgent(caps domain.Capabilities, st domain.VectorState, stations []domain.Station, lanes []domain.Lane, cursor map[domain.LaneID]int) Agent {
	a := Agent{
		ID:         st.ID,
		Domain:     caps.Domain,
		BatteryPct: st.BatteryPct,
		Link:       st.Link,
		Mode:       st.Mode,
		Position:   st.Position,
		Speed:      st.Speed,
	}
	if a.ID == "" {
		a.ID = caps.ID
	}

	switch {
	case !st.Position.Finite() || !(caps.MaxRangeM > 0) || math.IsInf(caps.MaxRangeM, 0):
	case len(stations) == 0:
		a.RTBCostPct, a.RTBCostKnown = 0, true
	default:
		best := domain.MinByCost(stations,
			func(s domain.Station) float64 { return domain.HaversineM(st.Position, s.Position) },
			func(s domain.Station) string { return s.Name },
		)
		// Multiply before dividing: an exact distance then yields an exact
		// percentage, where d / r * 100 can land a hair above an integer and
		// round up to the next one.
		pct := math.Ceil(domain.HaversineM(st.Position, stations[best].Position) * 100 / caps.MaxRangeM)
		a.RTBCostPct, a.RTBCostKnown = int(math.Min(pct, maxRTBCostPct)), true
	}

	for _, l := range lanes {
		if l.AssignedTo != a.ID || (a.Lane != nil && l.Index >= a.Lane.Index) {
			continue
		}
		progress := 0.0
		if n := len(l.Waypoints); n > 0 {
			c := min(max(cursor[l.ID], 0), n)
			progress = float64(c) * 100 / float64(n)
		}
		a.Lane = &LaneBinding{ID: l.ID, Index: l.Index, ProgressPct: progress}
	}
	return a
}

// BindMission derives the mission binding. coverage_pct is in [0, 100].
func BindMission(m domain.Mission) MissionBinding {
	return MissionBinding{
		Area:        m.Area,
		CoveragePct: m.Area.Grid.CoveragePct() * 100,
		State:       m.State,
	}
}

// Window is the duration-window state of one (rule, agent) pair. The
// definition lives in internal/domain, where engine state and the hot swap
// decision can carry it.
type Window = domain.Window

// compareWindows orders windows by rule id, then agent: the order a window
// slice is kept in, so the state encodes canonically and lookup is a binary
// search.
func compareWindows(a, b Window) int {
	if c := strings.Compare(a.RuleID, b.RuleID); c != 0 {
		return c
	}
	return strings.Compare(string(a.Agent), string(b.Agent))
}

// Violation is one constraint an agent does not satisfy.
type Violation struct {
	Constraint string          `json:"constraint"`
	Agent      domain.VectorID `json:"agent"`
}

// Firing is the rule an agent's tick resolved to, with every rule that would
// also have fired and lost to it.
type Firing struct {
	Agent    domain.VectorID       `json:"agent"`
	RuleID   string                `json:"rule_id"`
	Priority int                   `json:"priority"`
	Actions  []Action              `json:"actions"`
	Shadowed []domain.ShadowedRule `json:"shadowed,omitempty"`
}

// Result is one evaluation's output.
type Result struct {
	// Windows is the window state to carry into the next tick, sorted by
	// rule id then agent.
	Windows []Window `json:"windows,omitempty"`
	// Violations are sorted by agent, then constraint id.
	Violations []Violation `json:"violations,omitempty"`
	// Firings holds at most one entry per agent, sorted by agent.
	Firings []Firing `json:"firings,omitempty"`
}

// CheckConstraints evaluates every constraint of the pack for every agent.
// It is gate 4 of plan validation, run against the projected initial state,
// and the first half of Evaluate.
func CheckConstraints(p *Pack, env Env) ([]Violation, error) {
	agents, err := sortedAgents(env.Agents)
	if err != nil {
		return nil, err
	}
	var out []Violation
	for i := range agents {
		e := evaluator{pack: p, env: &env, agent: &agents[i]}
		for _, c := range p.Constraints {
			if !e.bool(c.Rule.root) {
				out = append(out, Violation{Constraint: c.ID, Agent: agents[i].ID})
			}
		}
	}
	return out, nil
}

// Evaluate runs one tick of doctrine: constraints, then rules, for every
// agent in id order.
//
// A rule fires on an edge. Its condition must hold for the rule's window of
// mission time, measured from the tick it was first observed true; the rule
// then fires once, and fires again only after its condition has been false
// for at least one tick. A rule with no window fires on the tick its
// condition becomes true.
//
// Priority is resolved per agent (spec.md section 6.4): of the rules ready to
// fire for one agent, the one with the highest priority wins, ties broken by
// ascending id. Every other ready rule is reported as shadowed by the winner,
// and consumes its edge: it does not fire on the next tick in the winner's
// place.
//
// windows is the state returned by the previous tick. Entries for rules the
// pack does not declare, and for agents absent from env, are dropped.
func Evaluate(p *Pack, env Env, windows []Window) (Result, error) {
	agents, err := sortedAgents(env.Agents)
	if err != nil {
		return Result{}, err
	}
	prev := slices.Clone(windows)
	slices.SortFunc(prev, compareWindows)

	var res Result
	for i := range agents {
		a := &agents[i]
		e := evaluator{pack: p, env: &env, agent: a, violated: map[string]bool{}}
		for _, c := range p.Constraints {
			if !e.bool(c.Rule.root) {
				e.violated[c.ID] = true
				res.Violations = append(res.Violations, Violation{Constraint: c.ID, Agent: a.ID})
			}
		}

		var ready []Rule
		for _, r := range p.Rules { // priority descending, then id
			if !e.bool(r.When.Expr.root) {
				continue
			}
			w := Window{RuleID: r.ID, Agent: a.ID, SinceMs: env.TickMs}
			if j, ok := slices.BinarySearchFunc(prev, w, compareWindows); ok {
				w = prev[j]
			}
			if !w.Fired && env.TickMs-w.SinceMs >= r.When.WindowMs {
				w.Fired = true
				ready = append(ready, r)
			}
			res.Windows = append(res.Windows, w)
		}
		if len(ready) == 0 {
			continue
		}
		winner := ready[0]
		f := Firing{Agent: a.ID, RuleID: winner.ID, Priority: winner.Priority, Actions: slices.Clone(winner.Then)}
		for _, r := range ready[1:] {
			f.Shadowed = append(f.Shadowed, domain.ShadowedRule{RuleID: r.ID, ShadowedBy: winner.ID, Priority: r.Priority})
		}
		res.Firings = append(res.Firings, f)
	}
	slices.SortFunc(res.Windows, compareWindows)
	return res, nil
}

// sortedAgents copies the agents into id order and rejects duplicate ids.
func sortedAgents(in []Agent) ([]Agent, error) {
	out := slices.Clone(in)
	slices.SortStableFunc(out, func(a, b Agent) int { return strings.Compare(string(a.ID), string(b.ID)) })
	for i := range out {
		if out[i].ID == "" {
			return nil, fmt.Errorf("%w: agent with an empty id", ErrInvalidEnv)
		}
		if i > 0 && out[i].ID == out[i-1].ID {
			return nil, fmt.Errorf("%w: agent %s appears twice", ErrInvalidEnv, out[i].ID)
		}
	}
	return out, nil
}

// evaluator evaluates compiled expressions for one agent.
type evaluator struct {
	pack     *Pack
	env      *Env
	agent    *Agent
	violated map[string]bool // constraint ids this agent violates; read by lookup only
}

func (e *evaluator) bool(n boolNode) bool {
	switch n.op {
	case boolOr:
		for _, t := range n.terms {
			if e.bool(t) {
				return true
			}
		}
		return false
	case boolAnd:
		for _, t := range n.terms {
			if !e.bool(t) {
				return false
			}
		}
		return true
	case boolCmp:
		l, lok := e.value(n.left)
		r, rok := e.value(n.right)
		if !lok || !rok {
			return false
		}
		return compare(n.cmp, n.left.typ.kind, l, r)
	case boolWithin:
		pos, pok := e.value(n.left)
		area, aok := e.value(n.right)
		return pok && aok && withinAO(pos.pos, area.area)
	case boolViolates:
		return e.violated[n.constraint]
	default:
		return false
	}
}

// value is the runtime form of a valueNode.
type value struct {
	num  float64
	str  string
	pos  domain.Position
	area *domain.Area
}

// value reads an operand. The boolean is false when the value is absent.
func (e *evaluator) value(v valueNode) (value, bool) {
	switch v.op {
	case valNumber:
		return value{num: v.num}, true
	case valString, valEnum:
		return value{str: v.str}, true
	case valSum:
		// Terms are added left to right in source order, which is fixed, so
		// the sum is reproducible without a sort.
		var acc float64
		for i, t := range v.terms {
			x, ok := e.value(t)
			if !ok {
				return value{}, false
			}
			if v.neg[i] {
				acc -= x.num
			} else {
				acc += x.num
			}
		}
		return value{num: acc}, true
	case valPath:
		return e.path(v.field)
	default:
		return value{}, false
	}
}

func (e *evaluator) path(f fieldID) (value, bool) {
	a := e.agent
	switch f {
	case fieldAgentID:
		return value{str: string(a.ID)}, true
	case fieldAgentDomain:
		return value{str: string(a.Domain)}, true
	case fieldAgentBatteryPct:
		return value{num: float64(a.BatteryPct)}, true
	case fieldAgentRTBCostPct:
		return value{num: float64(a.RTBCostPct)}, a.RTBCostKnown
	case fieldAgentLinkState:
		return value{str: string(a.Link)}, true
	case fieldAgentMode:
		return value{str: string(a.Mode)}, true
	case fieldAgentPosition:
		return value{pos: a.Position}, a.Position.Finite()
	case fieldAgentSpeed:
		return value{num: a.Speed}, true
	case fieldMissionArea:
		return value{area: &e.env.Mission.Area}, true
	case fieldMissionCoveragePct:
		return value{num: e.env.Mission.CoveragePct}, true
	case fieldMissionState:
		return value{str: string(e.env.Mission.State)}, true
	case fieldLaneID:
		if a.Lane == nil {
			return value{}, false
		}
		return value{str: string(a.Lane.ID)}, true
	case fieldLaneIndex:
		if a.Lane == nil {
			return value{}, false
		}
		return value{num: float64(a.Lane.Index)}, true
	case fieldLaneProgressPct:
		if a.Lane == nil {
			return value{}, false
		}
		return value{num: a.Lane.ProgressPct}, true
	case fieldDoctrineBatteryReservePct:
		return value{num: float64(e.pack.Params.BatteryReservePct)}, true
	default:
		return value{}, false
	}
}

// compare applies a comparison the type checker has already validated.
// Numbers compare exactly: their values come from integers, sums of
// integers and fixed derivations, so there is no accumulated error for an
// epsilon to absorb, and an epsilon would make == disagree with <=.
func compare(op string, k kind, l, r value) bool {
	if k == kindNumber {
		switch op {
		case "==":
			return l.num == r.num
		case "!=":
			return l.num != r.num
		case "<":
			return l.num < r.num
		case "<=":
			return l.num <= r.num
		case ">":
			return l.num > r.num
		case ">=":
			return l.num >= r.num
		}
		return false
	}
	switch op {
	case "==":
		return l.str == r.str
	case "!=":
		return l.str != r.str
	}
	return false
}

// withinAO is the geofence at grid resolution: the position lies in a cell
// whose centre is inside the AO polygon. It is the resolution coverage routes
// are guaranteed at (spec.md section 5.5 step 7), so the system's own routes
// never break its own geofence at a concave corner.
func withinAO(p domain.Position, a *domain.Area) bool {
	if a == nil || a.Grid.Count() == 0 || !(a.Grid.CellM > 0) {
		return false
	}
	id, ok := a.Grid.CellOf(p)
	return ok && a.Grid.IsInAO(id)
}
