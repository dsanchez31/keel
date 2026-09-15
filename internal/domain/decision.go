package domain

import "slices"

// DecisionKind classifies an orchestration choice.
type DecisionKind string

const (
	DecisionAssignment     DecisionKind = "assignment"
	DecisionReassignment   DecisionKind = "reassignment"
	DecisionRedecompose    DecisionKind = "redecompose"
	DecisionDoctrineRule   DecisionKind = "doctrine_rule"
	DecisionDoctrineSwap   DecisionKind = "doctrine_swap"
	DecisionMissionState   DecisionKind = "mission_state"
	DecisionOperatorNotice DecisionKind = "operator_notice"
)

// ShadowedRule records a rule that would have fired but lost to a higher
// priority one. Shadowing is reported, never silent: the question an operator
// actually asks is "why didn't it do the other thing".
type ShadowedRule struct {
	RuleID     string `json:"rule_id"`
	ShadowedBy string `json:"shadowed_by"`
	Priority   int    `json:"priority"`
}

// Candidate is one vector considered for a decision, kept whether it won or
// not. A decision that reports only its winner is incomplete.
type Candidate struct {
	Vector   VectorID `json:"vector"`
	Cost     float64  `json:"cost"`
	Rejected bool     `json:"rejected,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

// Decision is one recorded orchestration choice with its full rationale.
type Decision struct {
	TickMs     int64          `json:"tick_ms"`
	Kind       DecisionKind   `json:"kind"`
	Subject    string         `json:"subject,omitempty"`
	RuleFired  string         `json:"rule_fired,omitempty"`
	Shadowed   []ShadowedRule `json:"shadowed,omitempty"`
	Candidates []Candidate    `json:"candidates,omitempty"`
	Winner     VectorID       `json:"winner,omitempty"`
	Rationale  string         `json:"rationale"`
	// Swap is set on a doctrine_swap decision only.
	Swap *DoctrineSwap `json:"swap,omitempty"`
}

// SortCandidates orders a candidate list: accepted before rejected, then by
// ascending cost, then by vector id.
//
// The id tiebreak is what makes two vectors with an identical cost resolve the
// same way on every run and every machine.
func SortCandidates(in []Candidate) []Candidate {
	out := slices.Clone(in)
	sortSlice(out, func(a, b Candidate) int {
		if a.Rejected != b.Rejected {
			if a.Rejected {
				return 1
			}
			return -1
		}
		if c := compareFloat(a.Cost, b.Cost); c != 0 {
			return c
		}
		return compareString(string(a.Vector), string(b.Vector))
	})
	return out
}

// SortShadowed orders shadowed rules by descending priority then ascending id,
// the same order the evaluator collected them in.
func SortShadowed(in []ShadowedRule) []ShadowedRule {
	out := slices.Clone(in)
	sortSlice(out, func(a, b ShadowedRule) int {
		if a.Priority != b.Priority {
			return b.Priority - a.Priority
		}
		return compareString(a.RuleID, b.RuleID)
	})
	return out
}

// CostEpsilon is the tolerance below which two costs are the same cost.
//
// Float addition is not associative, so a cost accumulated over a different
// number of terms can differ in the last bits. Comparing with == would let
// that difference decide an assignment; comparing with an epsilon and then
// breaking the tie on VectorID makes the outcome a function of the fleet
// rather than of the arithmetic.
const CostEpsilon = 1e-9

// compareFloat orders two costs, treating a difference below CostEpsilon as
// equality so the caller's id tiebreak takes over.
func compareFloat(a, b float64) int {
	d := a - b
	switch {
	case d < -CostEpsilon:
		return -1
	case d > CostEpsilon:
		return 1
	default:
		return 0
	}
}

// CompareCost is the exported form of the epsilon comparison, for callers
// outside this package that rank on cost.
func CompareCost(a, b float64) int { return compareFloat(a, b) }

// MinByCost returns the index of the lowest-cost element, comparing with
// CostEpsilon and breaking ties on the key returned by keyOf. It returns -1
// for an empty slice.
//
// This is the shape every allocation decision in the system takes, factored
// out so that the epsilon and the tiebreak cannot be forgotten at one call
// site and remembered at another. It lives here rather than in engine because
// internal/assign needs it and engine imports assign.
func MinByCost[T any](xs []T, costOf func(T) float64, keyOf func(T) string) int {
	best := -1
	for i := range xs {
		if best < 0 {
			best = i
			continue
		}
		switch compareFloat(costOf(xs[i]), costOf(xs[best])) {
		case -1:
			best = i
		case 0:
			if keyOf(xs[i]) < keyOf(xs[best]) {
				best = i
			}
		}
	}
	return best
}
