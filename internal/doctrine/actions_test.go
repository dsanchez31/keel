package doctrine

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
)

func TestCompileActionsAccepts(t *testing.T) {
	got, err := compileActions(`release_lanes(agent), redecompose(policy=nearest_capable), return_to_base( agent ),notify_operator(message="link \"lost\"")`)
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{
		{Kind: ActionReleaseLanes},
		{Kind: ActionRedecompose, Policy: domain.PolicyNearestCapable},
		{Kind: ActionReturnToBase},
		{Kind: ActionNotifyOperator, Message: `link "lost"`},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("compileActions = %+v, want %+v", got, want)
	}
}

func TestCompileActionsPolicies(t *testing.T) {
	for _, p := range []domain.AssignPolicy{domain.PolicyNearestCapable, domain.PolicyRoundRobin, domain.PolicyLowestCost} {
		got, err := compileActions("redecompose(policy=" + string(p) + ")")
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if got[0].Policy != p {
			t.Fatalf("policy = %s, want %s", got[0].Policy, p)
		}
	}
}

// String renders the syntax the action was written in, so it round trips.
func TestActionStringRoundTrips(t *testing.T) {
	src := `release_lanes(agent), return_to_base(agent), redecompose(policy=lowest_cost), notify_operator(message="a \\ \"b\"")`
	actions, err := compileActions(src)
	if err != nil {
		t.Fatal(err)
	}
	parts := make([]string, len(actions))
	for i, a := range actions {
		parts[i] = a.String()
	}
	again, err := compileActions(strings.Join(parts, ", "))
	if err != nil {
		t.Fatalf("re-parse %q: %v", strings.Join(parts, ", "), err)
	}
	if !slices.Equal(actions, again) {
		t.Fatalf("round trip %+v != %+v", again, actions)
	}
}

func TestCompileActionsRejects(t *testing.T) {
	cases := []struct{ src, want string }{
		{"", "action expected, found end of expression"},
		{"release_lanes", "'(' expected after release_lanes"},
		{"release_lanes()", "release_lanes takes exactly one argument: agent"},
		{"release_lanes(vehicle)", "release_lanes takes exactly one argument: agent"},
		{"release_lanes(agent, agent)", "takes exactly one argument"},
		{"return_to_base(target=agent)", "return_to_base takes exactly one argument: agent"},
		{"redecompose()", "redecompose takes exactly one argument: policy=<name>"},
		{"redecompose(nearest_capable)", "takes exactly one argument: policy=<name>"},
		{"redecompose(policy=fastest)", `unknown policy "fastest", valid: lowest_cost, nearest_capable, round_robin`},
		{`redecompose(policy="nearest_capable")`, "redecompose expects policy=<name>"},
		{`notify_operator("hi")`, `notify_operator takes exactly one argument: message="..."`},
		{"notify_operator(message=hi)", `notify_operator expects message="..."`},
		{`notify_operator(message="  ")`, "message is empty"},
		{"self_destruct(agent)", `unknown action "self_destruct", valid: notify_operator, redecompose, release_lanes, return_to_base`},
		{"release_lanes(agent) redecompose(policy=round_robin)", "',' or end of actions expected"},
		{"release_lanes(agent),", "action expected, found end of expression"},
		{"release_lanes(agent), release_lanes(agent)", "release_lanes appears twice in one rule"},
		{"release_lanes(agent", "',' or ')' expected, found end of expression"},
		{"release_lanes(3)", "argument expected, found '3'"},
		{"redecompose(policy=)", "argument expected, found ')'"},
	}
	for _, c := range cases {
		_, err := compileActions(c.src)
		if err == nil {
			t.Errorf("%q: compiled, want error containing %q", c.src, c.want)
			continue
		}
		var ee *ExprError
		if !errors.As(err, &ee) {
			t.Errorf("%q: error %T is not an *ExprError", c.src, err)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: error %q does not contain %q", c.src, err, c.want)
		}
	}
}
