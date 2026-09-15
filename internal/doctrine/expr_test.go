package doctrine

import (
	"errors"
	"strings"
	"testing"
)

var testConstraints = []string{"battery-reserve", "geofence"}

func TestCompileConstraintAccepts(t *testing.T) {
	for _, src := range []string{
		"agent.battery_pct >= agent.rtb_cost_pct + doctrine.battery_reserve_pct",
		"agent.position within mission.area",
		"agent.link_state == LOST",
		"agent.mode != DOWN and agent.battery_pct < 30 or mission.coverage_pct >= 99.5",
		`agent.id == "DRONE-02"`,
		`agent.id == "say \"hi\" \\ bye"`,
		"lane.progress_pct - 10 > 0",
		"agent.domain == AERIAL and mission.state == AWAITING_APPROVAL",
		"agent.speed+1-2>=0",
	} {
		if _, err := compileConstraint(src); err != nil {
			t.Errorf("compileConstraint(%q) = %v", src, err)
		}
	}
}

func TestCompileConditionWindows(t *testing.T) {
	cases := []struct {
		src  string
		want int64
	}{
		{"agent.link_state == LOST", 0},
		{"agent.link_state == LOST for 5s", 5000},
		{"agent.link_state == DEGRADED for 500ms", 500},
		{"agent.battery_pct < 20 for 2m", 120_000},
		{"violates(battery-reserve)", 0},
		{"violates( geofence ) and agent.mode != RTB for 100ms", 100},
	}
	for _, c := range cases {
		cond, err := compileCondition(c.src, testConstraints)
		if err != nil {
			t.Errorf("compileCondition(%q) = %v", c.src, err)
			continue
		}
		if cond.WindowMs != c.want {
			t.Errorf("compileCondition(%q).WindowMs = %d, want %d", c.src, cond.WindowMs, c.want)
		}
		if cond.Expr.String() != c.src {
			t.Errorf("source not kept: %q", cond.Expr.String())
		}
	}
}

// "and" binds tighter than "or": a and b or c is (a and b) or c.
func TestPrecedence(t *testing.T) {
	e, err := compileConstraint("agent.speed > 1 and agent.speed < 5 or agent.mode == IDLE")
	if err != nil {
		t.Fatal(err)
	}
	if e.root.op != boolOr || len(e.root.terms) != 2 {
		t.Fatalf("root = %+v, want an or of two terms", e.root)
	}
	if e.root.terms[0].op != boolAnd || e.root.terms[1].op != boolCmp {
		t.Fatalf("terms = %v %v, want and, cmp", e.root.terms[0].op, e.root.terms[1].op)
	}
}

func TestCompileRejects(t *testing.T) {
	cases := []struct {
		src       string
		condition bool // compile as a rule condition rather than a constraint
		want      string
	}{
		{"", false, "operand expected, found end of expression"},
		{"agent.batery_pct > 1", false, `unknown path "agent.batery_pct"`},
		{"vehicle.x == 1", false, `unknown identifier "vehicle"`},
		{"agent == 1", false, `unknown path "agent"`},
		{"agent.link_state == Lost", false, "neither a lowercase path segment"},
		{"agent.link_state == FLYING", false, "FLYING is not a link_state value, valid: DEGRADED, LOST, OK"},
		{`agent.link_state == "lost"`, false, "compare with an enum literal such as DEGRADED"},
		{"LOST == LOST", false, "must be compared with an enum path"},
		{"agent.battery_pct == LOST", false, "enum literal LOST compared with 'agent.battery_pct', which is a number"},
		{"agent.link_state < 3", false, "< compares numbers"},
		{"agent.position == agent.position", false, "use within"},
		{`agent.id == 3`, false, "'agent.id' is a string and '3' is a number"},
		{"agent.battery_pct within mission.area", false, "within needs a position on its left"},
		{"agent.position within agent.position", false, "within needs an area on its right"},
		{"agent.battery_pct + agent.mode > 1", false, "+ and - take numbers"},
		{"agent.battery_pct + LOST > 1", false, "is an enum literal"},
		{"agent.battery_pct", false, "comparison expected after 'agent.battery_pct'"},
		{"agent.battery_pct > 1 extra", false, "unexpected 'extra'"},
		{"agent.battery_pct > and", false, `found keyword "and"`},
		{"agent.battery_pct > 5s", false, "only valid after 'for'"},
		{"agent.battery_pct ! 3", false, "unexpected character '!'"},
		{"agent.battery_pct > (3)", false, "operand expected, found '('"},
		{`agent.id == "abc`, false, "unterminated string"},
		{`agent.id == "a\n"`, false, "invalid escape"},
		{"agent.battery_pct > 1.", false, "digit expected after '.'"},
		{"agent.battery_pct > 1 for 5s", false, "takes no 'for' window"},
		{"violates(geofence)", false, "only allowed in a rule's when"},
		{"violates(unknown)", true, `unknown constraint "unknown", the pack declares: battery-reserve, geofence`},
		{"violates(Bad_Id)", true, "takes a constraint id"},
		{"violates geofence", true, "'(' expected after violates"},
		{"violates(geofence", true, "unterminated violates("},
		{"agent.link_state == LOST for 5", true, "duration expected after 'for'"},
		{"agent.link_state == LOST for", true, "duration expected after 'for', found end of expression"},
		{"agent.link_state == LOST for 5x", true, `unknown duration unit "x"`},
		{"agent.link_state == LOST for 150ms", true, "not a multiple of the 100 ms tick"},
		{"agent.link_state == LOST for 0s", true, "is zero"},
		{"agent.link_state == LOST for 1.5s", true, "must be a whole number"},
		{"agent.link_state == LOST for 99999999999999999999s", true, "out of range"},
		{"agent.link_state == LOST for 5s and agent.speed > 1", true, "unexpected 'and'"},
	}
	for _, c := range cases {
		var err error
		if c.condition {
			_, err = compileCondition(c.src, testConstraints)
		} else {
			_, err = compileConstraint(c.src)
		}
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

func TestErrorPosition(t *testing.T) {
	cases := []struct {
		src string
		pos int
	}{
		{"agent.batery_pct > 1", 0},
		{"agent.battery_pct > 1 extra", 22},
		{"agent.link_state == FLYING", 20},
		{"violates( nope )", 10},
	}
	for _, c := range cases {
		_, err := compileCondition(c.src, testConstraints)
		var ee *ExprError
		if !errors.As(err, &ee) {
			t.Fatalf("%q: %v", c.src, err)
		}
		if ee.Pos != c.pos {
			t.Errorf("%q: Pos = %d, want %d (%v)", c.src, ee.Pos, c.pos, err)
		}
	}
}

func TestEnumMembersSorted(t *testing.T) {
	for name, members := range enumMembers {
		for i := 1; i < len(members); i++ {
			if members[i-1] >= members[i] {
				t.Errorf("enum %s is not sorted: %v", name, members)
			}
		}
	}
}
