package world

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
)

func referenceScenarioPath() string {
	return filepath.Join("..", "..", "examples", "sims", "reference.yaml")
}

func loadReferenceScenario(t testing.TB) *Scenario {
	t.Helper()
	src, err := os.ReadFile(referenceScenarioPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	sc, err := ParseScenario(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return sc
}

func TestReferenceScenarioParses(t *testing.T) {
	sc := loadReferenceScenario(t)
	if sc.Name != "reference" || sc.Seed != 42 || sc.Mission != "MSN-042" {
		t.Fatalf("identity %q seed %d mission %q", sc.Name, sc.Seed, sc.Mission)
	}
	for _, p := range []string{sc.WorldPath, sc.PlanPath} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(referenceScenarioPath()), p)); err != nil {
			t.Fatalf("referenced file %s: %v", p, err)
		}
	}
	ph := sc.Physics
	if ph.ClimbRateMps != 5 || ph.Comms.RangeM != 15000 || ph.Comms.LossPct != 2 || ph.Comms.JitterMs != 200 {
		t.Fatalf("physics %+v", ph)
	}
	if len(ph.Blackouts) != 1 || len(ph.Terrain.Blocked) != 1 || ph.Terrain.CellM != 25 {
		t.Fatalf("zones %+v terrain %+v", ph.Blackouts, ph.Terrain)
	}
	if len(sc.Faults) != 1 {
		t.Fatalf("faults %+v", sc.Faults)
	}
	f := sc.Faults[0]
	if f.Trigger != TriggerCoverage || f.AtCoverage != 60 || f.Fault != (domain.Fault{Kind: domain.FaultLinkLoss, Vector: "DRONE-02"}) {
		t.Fatalf("fault %+v", f)
	}
	if f.Due(0, 59.9) || !f.Due(0, 60) {
		t.Fatal("a coverage trigger fires at its percentage, not before")
	}
}

func TestScenarioRejects(t *testing.T) {
	const base = `apiVersion: keel.sim/v1
name: t
seed: 1
mission: M
world: w.yaml
plan: p.json
kinematics: {climb_rate_mps: 5}
`
	cases := []struct {
		name, src, want string
	}{
		{"unknown field", base + "wind: 3\n", "field wind not found"},
		{"wrong api version", strings.Replace(base, "keel.sim/v1", "keel.sim/v2", 1), "apiVersion"},
		{"no seed", strings.Replace(base, "seed: 1\n", "", 1), "seed is required"},
		{"no climb rate", strings.Replace(base, "kinematics: {climb_rate_mps: 5}\n", "", 1), "climb_rate_mps is required"},
		{"loss above 100", base + "comms: {loss_pct: 101}\n", "loss_pct"},
		{"jitter not whole ticks", base + "comms: {jitter_ms: 150}\n", "jitter_ms"},
		{"zero radius", base + "blackout_zones: [{name: z, center: {lat: 45, lon: 5}, radius_m: 0}]\n", "radius_m"},
		{"duplicate zone name", base + "blackout_zones: [{name: z, center: {lat: 45, lon: 5}, radius_m: 1}, {name: z, center: {lat: 45, lon: 5}, radius_m: 1}]\n", "duplicate"},
		{"blocked polygon of two points", base + "terrain: {blocked: [{name: b, polygon: [{lat: 45, lon: 5}, {lat: 46, lon: 5}]}]}\n", "3 vertices"},
		{"two triggers", base + "faults: [{at_ms: 100, at_coverage_pct: 5, vector: V, kind: kill}]\n", "exactly one"},
		{"no trigger", base + "faults: [{vector: V, kind: kill}]\n", "exactly one"},
		{"unknown fault kind", base + "faults: [{at_ms: 100, vector: V, kind: meteor}]\n", "unknown kind"},
		{"drain without magnitude", base + "faults: [{at_ms: 100, vector: V, kind: battery_drain}]\n", "magnitude"},
		{"fault time not whole ticks", base + "faults: [{at_ms: 150, vector: V, kind: kill}]\n", "at_ms"},
		{"second document", base + "---\nname: again\n", "single YAML document"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseScenario([]byte(tc.src))
			if !errors.Is(err, ErrInvalidScenario) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want ErrInvalidScenario mentioning %q", err, tc.want)
			}
		})
	}
	if _, err := ParseScenario([]byte(base)); err != nil {
		t.Fatalf("the minimal scenario is refused: %v", err)
	}
}
