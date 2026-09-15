package engine

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dsanchez31/keel/internal/assign"
	"github.com/dsanchez31/keel/internal/coverage"
	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
)

// Decision traces.
//
// Every orchestration choice the engine makes is recorded with what it chose
// and what it did not: the rule that fired and every rule it shadowed, the
// vector that won a lane and every vector that lost it, each loser with its
// reason. A trace reporting only the winner answers "what did it do" and not
// "why didn't it do the other thing", which is the question an operator asks.
//
// The builders below are the only place a Decision is assembled from the
// doctrine, allocation and coverage results, so the shape of a trace cannot
// differ between the launch round, a redecomposition and a retry. They copy
// every slice they are given: a decision is appended to the log and must not
// alias state the next tick mutates.

// ruleDecision records one doctrine firing for one agent.
//
// effects holds, per action of the firing and in the order they ran, what the
// action did. Shadowed rules are listed with the id of the rule that shadowed
// them, highest priority first.
func ruleDecision(tickMs int64, f doctrine.Firing, effects []string) Decision {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (priority %d) fired for %s", f.RuleID, f.Priority, f.Agent)
	for i, a := range f.Actions {
		sep := ", "
		if i == 0 {
			sep = ": "
		}
		b.WriteString(sep + a.String())
		if i < len(effects) && effects[i] != "" {
			b.WriteString(" " + effects[i])
		}
	}
	shadowed := sortShadowed(f.Shadowed)
	for i, s := range shadowed {
		sep := ", "
		if i == 0 {
			sep = "; shadowed "
		}
		fmt.Fprintf(&b, "%s%s (priority %d)", sep, s.RuleID, s.Priority)
	}
	return Decision{
		TickMs:    tickMs,
		Kind:      DecisionDoctrineRule,
		Subject:   string(f.Agent),
		RuleFired: f.RuleID,
		Shadowed:  shadowed,
		Rationale: b.String(),
	}
}

// assignmentDecision records the outcome of one lane in an allocation round,
// with every vector of the fleet as a candidate. cause is the rule whose
// redecomposition started the round, empty for the launch round and for the
// per-tick round over lanes left unassigned.
func assignmentDecision(tickMs int64, kind DecisionKind, cause string, a assign.Assignment) Decision {
	return Decision{
		TickMs:     tickMs,
		Kind:       kind,
		Subject:    string(a.Lane),
		RuleFired:  cause,
		Candidates: slices.Clone(a.Candidates),
		Winner:     a.Winner,
		Rationale:  a.Rationale,
	}
}

// redecomposeDecision records a redecomposition: the pool, the lanes it
// retired and the lanes it cut. The allocation of the new lanes follows as
// one assignment decision per lane, each with its own candidate list. cause
// is the rule that asked for it; reason, when no rule did, is why the engine
// did.
func redecomposeDecision(tickMs int64, cause, reason string, policy AssignPolicy, eligible int, res coverage.Result) Decision {
	retired := make([]string, len(res.Retired))
	for i, id := range res.Retired {
		retired[i] = string(id)
	}
	cut := make([]string, len(res.New))
	for i, l := range res.New {
		cut[i] = string(l.ID)
	}
	rationale := fmt.Sprintf("pooled %d uncovered cell(s) of %s into %s across %d eligible vector(s), allocated by %s",
		len(res.Pool), listOrNone(retired), listOrNone(cut), eligible, policy)
	if len(res.Pool) == 0 {
		rationale = fmt.Sprintf("no uncovered cell to pool, retired %s", listOrNone(retired))
	}
	if reason != "" {
		rationale = reason + ": " + rationale
	}
	return Decision{
		TickMs:    tickMs,
		Kind:      DecisionRedecompose,
		Subject:   string(policy),
		RuleFired: cause,
		Rationale: rationale,
	}
}

// swapDecision records a hot swap with the full swap record, both packs by
// reference and hash and every window retained or discarded (spec.md section
// 6.6), so a replay reproduces it exactly.
func swapDecision(tickMs int64, sw doctrine.Swap) Decision {
	rec := sw
	rec.Retained = slices.Clone(sw.Retained)
	rec.Discarded = slices.Clone(sw.Discarded)
	return Decision{
		TickMs:  tickMs,
		Kind:    DecisionDoctrineSwap,
		Subject: sw.To.String(),
		Rationale: fmt.Sprintf("doctrine swapped from %s to %s at a tick boundary, %d window(s) retained, %d discarded",
			PackLabel(sw.From, sw.FromHash), PackLabel(sw.To, sw.ToHash), len(sw.Retained), len(sw.Discarded)),
		Swap: &rec,
	}
}

// noticeDecision records an operator-facing message from notify_operator.
func noticeDecision(tickMs int64, agent VectorID, rule, message string) Decision {
	return Decision{
		TickMs:    tickMs,
		Kind:      DecisionOperatorNotice,
		Subject:   string(agent),
		RuleFired: rule,
		Rationale: fmt.Sprintf("%s: %s", agent, message),
	}
}

// refusalDecision records an event the engine could not act on, so the log
// says why nothing happened rather than saying nothing.
func refusalDecision(tickMs int64, kind DecisionKind, subject, rationale string) Decision {
	return Decision{TickMs: tickMs, Kind: kind, Subject: subject, Rationale: rationale}
}

// sortShadowed orders shadowed rules by descending priority then ascending
// id, nil when there are none so the field is omitted from the encoding.
func sortShadowed(in []ShadowedRule) []ShadowedRule {
	if len(in) == 0 {
		return nil
	}
	return domain.SortShadowed(in)
}

func listOrNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}

// PackLabel is how a decision names a pack: its reference, then the first 12
// hex digits of its hash in parentheses. A doctrine diff reads decisions
// modulo the pack they name through it.
func PackLabel(ref DoctrineRef, hash string) string {
	return ref.String() + " (" + shortHex(hash) + ")"
}

func shortHex(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
