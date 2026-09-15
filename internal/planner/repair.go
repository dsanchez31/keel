package planner

import (
	"context"
	"fmt"

	"github.com/dsanchez31/keel/internal/assign"
	"github.com/dsanchez31/keel/internal/coverage"
	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/planir"
)

// MaxAttempts is the repair budget of spec section 5.4: the first proposal
// and at most two repairs.
const MaxAttempts = 3

// Input is one compilation: the operator's intent and what it is compiled
// against.
type Input struct {
	Intent    string
	World     *planir.World
	Doctrines *doctrine.Registry
	// Arrival is the engine's arrival budget gate 3 checks the AO against.
	Arrival coverage.Arrival
}

// Attempt is one proposal and what the validator said about it.
type Attempt struct {
	N           int                 `json:"n"`
	Reply       string              `json:"reply"`
	Diagnostics []planir.Diagnostic `json:"diagnostics,omitempty"`
}

// Outcome is what a compilation produced. Plan is set when an attempt passed
// every gate; otherwise Attempts holds every refused reply with its
// diagnostics, which is what the operator sees, or Triage says why the intent
// was declined before any attempt.
type Outcome struct {
	Backend     string               `json:"backend"`
	Intent      string               `json:"intent"`
	Triage      *Triage              `json:"triage,omitempty"`
	IR          *planir.Plan         `json:"ir,omitempty"`
	Plan        *domain.ApprovedPlan `json:"plan,omitempty"`
	Assignments []assign.Assignment  `json:"assignments,omitempty"`
	Attempts    []Attempt            `json:"attempts"`
}

// OK reports whether the compilation produced a plan.
func (o Outcome) OK() bool { return o.Plan != nil }

// Declined reports whether the triage found no mission in the intent.
func (o Outcome) Declined() bool { return o.Triage != nil && !o.Triage.Mission }

// Compile triages the intent, then asks the planner for a Plan IR and
// validates it, feeding every refusal back as structured diagnostics, for at
// most MaxAttempts attempts.
//
// An intent the triage finds no mission in ends the compilation there, with
// its reason and no attempt. The triage is not an attempt: it proposes no
// plan, and an unreadable triage reply is compiled like a mission.
//
// Nothing is relaxed between attempts. The validator input is the same every
// time, so the only thing a repair can change is the reply. After the last
// refusal the outcome carries no plan and every attempt's diagnostics: that is
// a valid result, not an error.
//
// A backend failure is an error. It is not a plan defect, so it does not
// spend an attempt on a retry the model cannot fix; the attempts made so far
// are returned with it.
func Compile(ctx context.Context, p Planner, in Input) (Outcome, error) {
	out := Outcome{Backend: p.Name(), Intent: in.Intent, Attempts: []Attempt{}}
	t, err := triage(ctx, p, in.Intent)
	if err != nil {
		return out, fmt.Errorf("triage: %w", err)
	}
	out.Triage = &t
	if !t.Mission {
		return out, nil
	}
	conv := Opening(in.Intent, in.World, in.Doctrines)
	for n := 1; n <= MaxAttempts; n++ {
		reply, err := p.Propose(ctx, conv)
		if err != nil {
			return out, fmt.Errorf("attempt %d of %d: %w", n, MaxAttempts, err)
		}
		res := planir.Validate(planir.Input{Intent: in.Intent, Raw: []byte(reply), World: in.World, Doctrines: in.Doctrines, Arrival: in.Arrival})
		out.Attempts = append(out.Attempts, Attempt{N: n, Reply: reply, Diagnostics: res.Diagnostics})
		if res.OK() {
			out.IR, out.Plan, out.Assignments = res.IR, res.Plan, res.Assignments
			return out, nil
		}
		if n == MaxAttempts {
			break
		}
		if conv, err = Repair(conv, reply, res.Diagnostics); err != nil {
			return out, err
		}
	}
	return out, nil
}
