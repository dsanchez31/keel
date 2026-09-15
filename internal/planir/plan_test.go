package planir

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
)

// referencePlanJSON is the Plan IR spec section 14 expects, written as a
// planner would emit it.
const referencePlanJSON = `{
  "apiVersion": "keel.plan/v1",
  "intent": "Grid-search the unexplored area to lift the fog of war",
  "doctrine": "recon-standard@2.1.0",
  "mission": {"type": "systematic_reconnaissance", "priority": "normal", "area": "fog_of_war_east"},
  "tactic": {"pattern": "parallel_lanes", "orientation": "long_axis", "lanes": 4, "overlap_pct": 10},
  "assignment": {"policy": "nearest_capable", "requires": ["camera", "aerial"], "gcs": "gcs-west"},
  "rationale": "Four camera drones are available, so four lanes along the long axis keep turns few."
}`

// mutate decodes the reference plan into a generic document, applies edit and
// re-encodes it, so a test states only the defect it introduces.
func mutate(t *testing.T, edit func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(referencePlanJSON), &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func obj(doc map[string]any, key string) map[string]any { return doc[key].(map[string]any) }

func TestTacticCoverageDefaultsOverlap(t *testing.T) {
	spec := TacticSpec{Pattern: domain.PatternParallelLanes, Orientation: domain.OrientationLongAxis, Lanes: 4}
	if got := spec.Coverage().OverlapPct; got != DefaultOverlapPct {
		t.Fatalf("omitted overlap: got %d, want %d", got, DefaultOverlapPct)
	}
	zero := 0
	spec.OverlapPct = &zero
	if got := spec.Coverage().OverlapPct; got != 0 {
		t.Fatalf("explicit zero overlap: got %d, want 0", got)
	}
}

func TestNormaliseSortsRequires(t *testing.T) {
	p, diags := checkSchema([]byte(referencePlanJSON))
	if len(diags) != 0 {
		t.Fatalf("reference plan refused: %+v", diags)
	}
	if want := []string{"aerial", "camera"}; !slices.Equal(p.Assignment.Requires, want) {
		t.Fatalf("requires: got %v, want %v", p.Assignment.Requires, want)
	}
}

func TestCompareDiagnosticsIsTotal(t *testing.T) {
	in := []Diagnostic{
		{Pointer: "/tactic", Code: "b"},
		{Pointer: "/mission", Code: "z"},
		{Pointer: "/tactic", Code: "a", Subject: "y"},
		{Pointer: "/tactic", Code: "a", Subject: "x"},
	}
	got := sortDiagnostics(slices.Clone(in))
	want := []Diagnostic{in[1], in[3], in[2], in[0]}
	for i := range want {
		if compareDiagnostics(got[i], want[i]) != 0 {
			t.Fatalf("position %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}
