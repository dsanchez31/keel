package doctrine

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
)

const testPackYAML = `apiVersion: keel.doctrine/v1
name: test-pack
version: 1.2.0
params:
  battery_reserve_pct: 15
roles:
  - id: scanner
    requires: [camera, aerial, camera]
  - id: relay
    requires: [radio_mesh, aerial]
constraints:
  - id: geofence
    rule: agent.position within mission.area
  - id: battery-reserve
    rule: agent.battery_pct >= agent.rtb_cost_pct + doctrine.battery_reserve_pct
rules:
  - id: notify-on-degraded-link
    when: agent.link_state == DEGRADED for 3s
    then: notify_operator(message="link degraded")
    priority: 10
  - id: reassign-on-link-loss
    when: agent.link_state == LOST for 5s
    then: release_lanes(agent), redecompose(policy=nearest_capable)
    priority: 100
  - id: rtb-on-low-battery
    when: violates(battery-reserve)
    then: return_to_base(agent), release_lanes(agent), redecompose(policy=nearest_capable)
    priority: 200
  - id: also-100
    when: agent.mode == DOWN
    then: release_lanes(agent)
    priority: 100
`

func mustParse(t *testing.T, src string) *Pack {
	t.Helper()
	p, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func TestParsePack(t *testing.T) {
	p := mustParse(t, testPackYAML)

	if want := (domain.DoctrineRef{Name: "test-pack", Version: "1.2.0"}); p.Ref != want {
		t.Errorf("Ref = %+v, want %+v", p.Ref, want)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(p.Hash) {
		t.Errorf("Hash = %q, want 64 lowercase hex digits", p.Hash)
	}
	if p.Params.BatteryReservePct != 15 {
		t.Errorf("BatteryReservePct = %d", p.Params.BatteryReservePct)
	}

	var roles []string
	for _, r := range p.Roles {
		roles = append(roles, r.ID+":"+strings.Join(r.Requires, ","))
	}
	if want := []string{"relay:aerial,radio_mesh", "scanner:aerial,camera"}; !slices.Equal(roles, want) {
		t.Errorf("roles = %v, want %v (sorted by id, tags sorted and deduplicated)", roles, want)
	}

	var constraints []string
	for _, c := range p.Constraints {
		constraints = append(constraints, c.ID)
	}
	if want := []string{"battery-reserve", "geofence"}; !slices.Equal(constraints, want) {
		t.Errorf("constraints = %v, want %v", constraints, want)
	}

	var rules []string
	for _, r := range p.Rules {
		rules = append(rules, r.ID)
	}
	want := []string{"rtb-on-low-battery", "also-100", "reassign-on-link-loss", "notify-on-degraded-link"}
	if !slices.Equal(rules, want) {
		t.Errorf("rules = %v, want %v (priority descending, then id)", rules, want)
	}

	r, ok := p.Rule("reassign-on-link-loss")
	if !ok || r.When.WindowMs != 5000 || len(r.Then) != 2 || r.Then[1].Policy != domain.PolicyNearestCapable {
		t.Errorf("Rule(reassign-on-link-loss) = %+v, %v", r, ok)
	}
	if c, ok := p.Constraint("geofence"); !ok || c.Rule.String() != "agent.position within mission.area" {
		t.Errorf("Constraint(geofence) = %+v, %v", c, ok)
	}
	if _, ok := p.Constraint("nope"); ok {
		t.Error("Constraint(nope) found")
	}
}

// The hash covers the normalised document: comments, whitespace, list order
// and tag order do not change it, any change of meaning does.
func TestPackHashNormalisation(t *testing.T) {
	base := mustParse(t, testPackYAML).Hash

	reordered := `# a comment
apiVersion: keel.doctrine/v1
version: 1.2.0
name: test-pack
params: {battery_reserve_pct: 15}
rules:
  - id: also-100
    priority: 100
    when: agent.mode == DOWN
    then: release_lanes(agent)
  - id: rtb-on-low-battery
    when: violates(battery-reserve)
    then: return_to_base(agent), release_lanes(agent), redecompose(policy=nearest_capable)
    priority: 200
  - id: reassign-on-link-loss
    when: agent.link_state == LOST for 5s
    then: release_lanes(agent), redecompose(policy=nearest_capable)
    priority: 100
  - id: notify-on-degraded-link
    when: "agent.link_state == DEGRADED for 3s"
    then: 'notify_operator(message="link degraded")'
    priority: 10
constraints:
  - id: battery-reserve
    rule: agent.battery_pct >= agent.rtb_cost_pct + doctrine.battery_reserve_pct
  - id: geofence
    rule: agent.position within mission.area
roles:
  - id: relay
    requires: [aerial, radio_mesh]
  - id: scanner
    requires: [aerial, camera]
`
	if got := mustParse(t, reordered).Hash; got != base {
		t.Errorf("reordered pack hash %s != %s", got, base)
	}

	for name, edit := range map[string][2]string{
		"version":  {"version: 1.2.0", "version: 1.2.1"},
		"reserve":  {"battery_reserve_pct: 15", "battery_reserve_pct: 20"},
		"window":   {"LOST for 5s", "LOST for 10s"},
		"priority": {"priority: 200", "priority: 300"},
		"action":   {"policy=nearest_capable)\n    priority: 100", "policy=lowest_cost)\n    priority: 100"},
		"tag":      {"[radio_mesh, aerial]", "[radio_mesh, ground]"},
	} {
		edited := strings.Replace(testPackYAML, edit[0], edit[1], 1)
		if edited == testPackYAML {
			t.Fatalf("%s: edit did not apply", name)
		}
		if got := mustParse(t, edited).Hash; got == base {
			t.Errorf("%s: hash unchanged after a change of meaning", name)
		}
	}
}

func TestParseDeterministic(t *testing.T) {
	first := mustParse(t, testPackYAML)
	for range 50 {
		p := mustParse(t, testPackYAML)
		if p.Hash != first.Hash {
			t.Fatalf("hash %s != %s", p.Hash, first.Hash)
		}
	}
}

func TestParseRejects(t *testing.T) {
	replace := func(old, new string) string {
		out := strings.Replace(testPackYAML, old, new, 1)
		if out == testPackYAML {
			t.Fatalf("edit %q did not apply", old)
		}
		return out
	}
	cases := []struct {
		name, src, want string
	}{
		{"empty", "", "empty document"},
		{"api version", replace("keel.doctrine/v1", "keel.doctrine/v2"), `apiVersion "keel.doctrine/v2"`},
		{"name", replace("name: test-pack", "name: Test_Pack"), "not lowercase kebab-case"},
		{"version", replace("version: 1.2.0", "version: 1.2"), "invalid version"},
		{"unknown top-level field", testPackYAML + "extra: 1\n", "field extra not found"},
		{"unknown rule field", replace("    priority: 10\n", "    priority: 10\n    enabled: true\n"), "field enabled not found"},
		{"unknown param", replace("battery_reserve_pct: 15", "battery_reserve_pct: 15\n  max_speed: 3"), "field max_speed not found"},
		{"duplicate key", replace("name: test-pack", "name: test-pack\nname: other"), "already defined"},
		{"second document", testPackYAML + "---\napiVersion: x\n", "single YAML document"},
		{"missing params", replace("params:\n  battery_reserve_pct: 15\n", ""), "params.battery_reserve_pct is required"},
		{"missing reserve", replace("  battery_reserve_pct: 15\n", "  {}\n"), "params.battery_reserve_pct is required"},
		{"reserve range", replace("battery_reserve_pct: 15", "battery_reserve_pct: 101"), "outside [0, 100]"},
		{"empty requires", replace("[radio_mesh, aerial]", "[]"), "roles[relay].requires is empty"},
		{"empty tag", replace("[radio_mesh, aerial]", `[radio_mesh, ""]`), "empty tag"},
		{"duplicate id", replace("id: relay", "id: geofence"), `id "geofence" is used by a role and a constraint`},
		{"bad rule id", replace("id: also-100", "id: Also100"), `rule id "Also100" is not lowercase kebab-case`},
		{"missing when", replace("    when: agent.mode == DOWN\n", ""), "rules[also-100].when is required"},
		{"missing then", replace("    then: release_lanes(agent)\n", ""), "rules[also-100].then is required"},
		{"missing priority", replace("    priority: 10\n", ""), "rules[notify-on-degraded-link].priority is required"},
		{"when not scalar", replace("when: agent.mode == DOWN", "when: [a, b]"), "rules[also-100].when (line 30) must be a string"},
		{"empty rule", replace("rule: agent.position within mission.area", `rule: ""`), "constraints[geofence].rule (line 13) is empty"},
		{"type error with line", replace("LOST for 5s", "LOST for 5x"), "rules[reassign-on-link-loss].when (line 22): col 30: unknown duration unit"},
		{"violates in constraint", replace("rule: agent.position within mission.area", "rule: violates(battery-reserve)"), "only allowed in a rule's when"},
		{"violates unknown", replace("violates(battery-reserve)", "violates(fuel)"), `unknown constraint "fuel"`},
		{"bad action", replace("release_lanes(agent), redecompose", "release_lanes(), redecompose"), "rules[reassign-on-link-loss].then (line 23)"},
	}
	for _, c := range cases {
		_, err := Parse([]byte(c.src))
		if err == nil {
			t.Errorf("%s: parsed, want error containing %q", c.name, c.want)
			continue
		}
		if !errors.Is(err, ErrInvalidPack) {
			t.Errorf("%s: %v is not ErrInvalidPack", c.name, err)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not contain %q", c.name, err, c.want)
		}
	}
}

// An expression error keeps its type through the pack error, so a caller can
// point at the column.
func TestParseErrorUnwrapsToExprError(t *testing.T) {
	_, err := Parse([]byte(strings.Replace(testPackYAML, "agent.mode == DOWN", "agent.mode == SLEEPING", 1)))
	var ee *ExprError
	if !errors.As(err, &ee) {
		t.Fatalf("error %v does not unwrap to *ExprError", err)
	}
	if ee.Pos != len("agent.mode == ") {
		t.Errorf("Pos = %d", ee.Pos)
	}
}

func packVersion(t *testing.T, version string) *Pack {
	t.Helper()
	return mustParse(t, strings.Replace(testPackYAML, "version: 1.2.0", "version: "+version, 1))
}

func TestRegistry(t *testing.T) {
	reg, err := NewRegistry(packVersion(t, "2.10.0"), packVersion(t, "2.9.0"), packVersion(t, "2.10.0-rc.1"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range reg.Refs() {
		got = append(got, r.String())
	}
	want := []string{"test-pack@2.9.0", "test-pack@2.10.0-rc.1", "test-pack@2.10.0"}
	if !slices.Equal(got, want) {
		t.Fatalf("Refs = %v, want %v", got, want)
	}

	p, err := reg.Resolve(domain.DoctrineRef{Name: "test-pack", Version: "2.9.0"})
	if err != nil || p.Ref.Version != "2.9.0" {
		t.Fatalf("Resolve = %v, %v", p, err)
	}

	_, err = reg.Resolve(domain.DoctrineRef{Name: "test-pack", Version: "3.0.0"})
	if !errors.Is(err, ErrUnknownPack) || !strings.Contains(err.Error(), "available: test-pack@2.9.0, test-pack@2.10.0-rc.1, test-pack@2.10.0") {
		t.Fatalf("Resolve unknown = %v", err)
	}

	if _, err := NewRegistry(packVersion(t, "1.0.0"), packVersion(t, "1.0.0")); !errors.Is(err, ErrInvalidPack) {
		t.Fatalf("duplicate registration = %v", err)
	}
}
