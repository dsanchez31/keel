package doctrine

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/dsanchez31/keel/internal/domain"
)

// ActionKind names one doctrine action (spec.md section 6.5).
type ActionKind string

const (
	// ActionReleaseLanes marks the agent's lanes unassigned, retaining their
	// explored cells.
	ActionReleaseLanes ActionKind = "release_lanes"
	// ActionRedecompose pools every unassigned uncovered cell and
	// redistributes it across eligible vectors under Policy.
	ActionRedecompose ActionKind = "redecompose"
	// ActionReturnToBase sets the agent's mode to rtb and routes it to the
	// nearest GCS.
	ActionReturnToBase ActionKind = "return_to_base"
	// ActionNotifyOperator emits an operator-facing event carrying Message and
	// changes no state.
	ActionNotifyOperator ActionKind = "notify_operator"
)

// Action is one validated call from a rule's then.
//
// It is a descriptor, not an effect. The doctrine package decides which
// actions a tick calls for; the engine carries them out, because the effects
// touch lanes, cursors and commands that only the engine's state holds. The
// agent an action applies to is the agent the rule fired for, so it is not a
// field here.
type Action struct {
	Kind    ActionKind          `json:"kind"`
	Policy  domain.AssignPolicy `json:"policy,omitempty"`
	Message string              `json:"message,omitempty"`
}

// String renders the action in the syntax it was written in.
func (a Action) String() string {
	switch a.Kind {
	case ActionReleaseLanes, ActionReturnToBase:
		return string(a.Kind) + "(agent)"
	case ActionRedecompose:
		return fmt.Sprintf("redecompose(policy=%s)", a.Policy)
	case ActionNotifyOperator:
		return fmt.Sprintf("notify_operator(message=%s)", quoteMessage(a.Message))
	default:
		return string(a.Kind)
	}
}

// quoteMessage quotes with the two escapes the lexer accepts.
func quoteMessage(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// ActionKinds lists every action, sorted.
var ActionKinds = []ActionKind{ActionNotifyOperator, ActionRedecompose, ActionReleaseLanes, ActionReturnToBase}

var policies = []string{
	string(domain.PolicyLowestCost), string(domain.PolicyNearestCapable), string(domain.PolicyRoundRobin),
}

// callArg is one argument as written: positional (key empty) or key=value.
type callArg struct {
	key   string
	value token
	pos   int
}

// compileActions parses a rule's then: a comma-separated list of calls, run in
// the order written. Every call is checked against its signature, and an
// action may appear once per rule.
//
//	release_lanes(agent)
//	return_to_base(agent)
//	redecompose(policy=nearest_capable|round_robin|lowest_cost)
//	notify_operator(message="...")
func compileActions(src string) ([]Action, error) {
	p, err := newParser(src, false, nil)
	if err != nil {
		return nil, err
	}
	var out []Action
	seen := map[ActionKind]bool{}
	for {
		name := p.next()
		if name.kind != tokIdent {
			return nil, p.errAt(name.pos, "action expected, found %s", describe(name))
		}
		args, err := p.parseArgs(name)
		if err != nil {
			return nil, err
		}
		a, err := p.checkCall(name, args)
		if err != nil {
			return nil, err
		}
		if seen[a.Kind] {
			return nil, p.errAt(name.pos, "%s appears twice in one rule", a.Kind)
		}
		seen[a.Kind] = true
		out = append(out, a)

		t := p.next()
		if t.kind == tokEOF {
			return out, nil
		}
		if !p.isOp(t, ",") {
			return nil, p.errAt(t.pos, "',' or end of actions expected, found %s", describe(t))
		}
	}
}

// parseArgs reads "(" ( arg ( "," arg )* )? ")".
func (p *parser) parseArgs(name token) ([]callArg, error) {
	if t := p.next(); !p.isOp(t, "(") {
		return nil, p.errAt(t.pos, "'(' expected after %s, found %s", name.text, describe(t))
	}
	var args []callArg
	if p.isOp(p.peek(), ")") {
		p.next()
		return args, nil
	}
	for {
		first := p.next()
		arg := callArg{value: first, pos: first.pos}
		if first.kind == tokIdent && p.isOp(p.peek(), "=") {
			p.next()
			arg.key = first.text
			arg.value = p.next()
		}
		if arg.value.kind != tokIdent && arg.value.kind != tokString {
			return nil, p.errAt(arg.value.pos, "argument expected, found %s", describe(arg.value))
		}
		args = append(args, arg)

		t := p.next()
		if p.isOp(t, ")") {
			return args, nil
		}
		if !p.isOp(t, ",") {
			return nil, p.errAt(t.pos, "',' or ')' expected, found %s", describe(t))
		}
	}
}

func (p *parser) checkCall(name token, args []callArg) (Action, error) {
	kind := ActionKind(name.text)
	switch kind {
	case ActionReleaseLanes, ActionReturnToBase:
		if len(args) != 1 || args[0].key != "" || args[0].value.kind != tokIdent || args[0].value.text != "agent" {
			return Action{}, p.errAt(name.pos, "%s takes exactly one argument: agent", kind)
		}
		return Action{Kind: kind}, nil

	case ActionRedecompose:
		v, err := p.keywordArg(name, args, "policy", tokIdent)
		if err != nil {
			return Action{}, err
		}
		if _, ok := slices.BinarySearch(policies, v.text); !ok {
			return Action{}, p.errAt(v.pos, "unknown policy %q, valid: %s", v.text, strings.Join(policies, ", "))
		}
		return Action{Kind: kind, Policy: domain.AssignPolicy(v.text)}, nil

	case ActionNotifyOperator:
		v, err := p.keywordArg(name, args, "message", tokString)
		if err != nil {
			return Action{}, err
		}
		if strings.TrimSpace(v.text) == "" {
			return Action{}, p.errAt(v.pos, "notify_operator message is empty")
		}
		return Action{Kind: kind, Message: v.text}, nil

	default:
		valid := make([]string, len(ActionKinds))
		for i, k := range ActionKinds {
			valid[i] = string(k)
		}
		return Action{}, p.errAt(name.pos, "unknown action %q, valid: %s", name.text, strings.Join(valid, ", "))
	}
}

// keywordArg checks a call takes exactly key=<value of kind want>.
func (p *parser) keywordArg(name token, args []callArg, key string, want tokKind) (token, error) {
	form := key + "=<name>"
	if want == tokString {
		form = key + `="..."`
	}
	if len(args) != 1 || args[0].key != key {
		return token{}, p.errAt(name.pos, "%s takes exactly one argument: %s", name.text, form)
	}
	v := args[0].value
	if v.kind != want {
		return token{}, p.errAt(v.pos, "%s expects %s, found %s", name.text, form, strconv.Quote(v.text))
	}
	return v, nil
}
