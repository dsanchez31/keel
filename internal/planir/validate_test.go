package planir

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/coverage"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/eventlog"
)

// testArrival is the engine's default arrival budget: a 5 m radius and 5 m
// of position error, 10 m under the 12.5 m clearance of the reference AO's
// 50 m cells.
var testArrival = coverage.Arrival{RadiusM: 5, ErrorM: 5}

// referenceIntent is the operator intent of spec section 14.
const referenceIntent = "Grid-search the unexplored area to lift the fog of war"

func referenceInput(t *testing.T, raw []byte) Input {
	t.Helper()
	return Input{Intent: referenceIntent, Raw: raw, World: loadReferenceWorld(t), Doctrines: loadPacks(t), Arrival: testArrival}
}

// withFleet returns a copy of the world whose fleet is edited by edit.
func withFleet(w *World, edit func(v *FleetVector)) *World {
	out := *w
	out.Fleet = slices.Clone(w.Fleet)
	for i := range out.Fleet {
		edit(&out.Fleet[i])
	}
	return &out
}

func TestReferencePlanValidates(t *testing.T) {
	res := Validate(referenceInput(t, []byte(referencePlanJSON)))
	if !res.OK() {
		t.Fatalf("reference plan refused: %+v", res.Diagnostics)
	}
	p := res.Plan
	if len(p.Lanes) != 4 {
		t.Fatalf("got %d lanes, want 4", len(p.Lanes))
	}
	if p.Hash == "" || p.Mission != "" {
		t.Fatalf("hash %q, mission %q: want a hash and no mission id before approval", p.Hash, p.Mission)
	}
	if p.Doctrine.String() != "recon-standard@2.1.0" || len(p.GCS) != 1 || p.GCS[0].Name != "gcs-west" {
		t.Fatalf("resolution not carried into the plan: %s %v", p.Doctrine, p.GCS)
	}
	// Gate 2 pins the pack it resolved, by content address.
	pack, err := loadPacks(t).Resolve(p.Doctrine)
	if err != nil || p.DoctrineHash != pack.Hash {
		t.Fatalf("doctrine hash %q, want the resolved pack's (%v)", p.DoctrineHash, err)
	}
	// Twice the narrowest footprint of the capable drones (90 m), and the
	// AO's scan altitude: the geometry redecomposition reuses mid mission.
	if p.SwathM != 180 || p.ScanAltM != 120 {
		t.Fatalf("swath %v m, altitude %v m: want 180 m and 120 m fixed in the plan", p.SwathM, p.ScanAltM)
	}
	for _, l := range p.Lanes {
		t.Logf("%s: %d cells, %d waypoints, %.0f m", l.ID, len(l.Cells), len(l.Waypoints), l.LengthM())
		if l.Assigned() {
			t.Errorf("%s is assigned in the plan, assignment happens at launch", l.ID)
		}
		// Spec section 14: seven passes per lane, each entered and left on
		// its centre line, with the corner cells of the slanted edges as
		// waypoints of their own, roughly 25 to 35 in all.
		if n := len(l.Waypoints); n < 20 || n > 40 {
			t.Errorf("%s has %d waypoints, spec section 14 expects roughly 25 to 35", l.ID, n)
		}
	}

	winners := map[domain.VectorID]bool{}
	for _, a := range res.Assignments {
		if !strings.HasPrefix(string(a.Winner), "DRONE-") {
			t.Errorf("%s projected to %q, want a camera drone", a.Lane, a.Winner)
		}
		winners[a.Winner] = true
		if len(a.Candidates) != 6 {
			t.Errorf("%s lists %d candidates, want the whole fleet of 6", a.Lane, len(a.Candidates))
		}
	}
	if len(winners) != 4 {
		t.Fatalf("projected winners %v, want four distinct drones", winners)
	}
}

// The plan is content addressed: the same reply yields the same hash, and
// the same defects yield the same diagnostics, on every run.
func TestValidateIsDeterministic(t *testing.T) {
	good := referenceInput(t, []byte(referencePlanJSON))
	first := Validate(good)
	bad := referenceInput(t, mutate(t, func(doc map[string]any) {
		obj(doc, "mission")["area"] = "fog_of_war_west"
		obj(doc, "assignment")["gcs"] = "gcs-east"
		doc["doctrine"] = "recon-standard@9.9.9"
	}))
	firstBad := Validate(bad)
	for range 100 {
		if again := Validate(good); again.Plan.Hash != first.Plan.Hash {
			t.Fatalf("hash changed between runs: %s then %s", first.Plan.Hash, again.Plan.Hash)
		}
		if again := Validate(bad); !reflect.DeepEqual(again.Diagnostics, firstBad.Diagnostics) {
			t.Fatalf("diagnostics changed between runs:\n%+v\n%+v", firstBad.Diagnostics, again.Diagnostics)
		}
	}
}

func TestHashCoversMeaning(t *testing.T) {
	base := Validate(referenceInput(t, []byte(referencePlanJSON)))
	other := Validate(referenceInput(t, mutate(t, func(doc map[string]any) {
		obj(doc, "tactic")["overlap_pct"] = 20
	})))
	if !base.OK() || !other.OK() {
		t.Fatalf("plans refused: %+v %+v", base.Diagnostics, other.Diagnostics)
	}
	if base.Plan.Hash == other.Plan.Hash {
		t.Fatal("a different overlap produced the same plan hash")
	}
	reordered := Validate(referenceInput(t, mutate(t, func(doc map[string]any) {
		obj(doc, "assignment")["requires"] = []any{"aerial", "camera"}
	})))
	if reordered.Plan.Hash != base.Plan.Hash {
		t.Fatal("the order of a set changed the plan hash")
	}
	// The swath is geometry the plan fixes, so it is part of what is hashed.
	wider := *base.Plan
	wider.Hash = ""
	wider.SwathM *= 2
	if h := domain.PlanHash(mustHex(t, wider)); h == base.Plan.Hash {
		t.Fatal("a different swath produced the same plan hash")
	}
	// The pin is part of what the human approved: another pack under the same
	// reference is another plan.
	repinned := *base.Plan
	repinned.Hash = ""
	repinned.DoctrineHash = strings.Repeat("0", 64)
	if h := domain.PlanHash(mustHex(t, repinned)); h == base.Plan.Hash {
		t.Fatal("a different doctrine hash produced the same plan hash")
	}
}

// Gate 3 refuses an AO whose route clearance does not cover the engine's
// arrival budget, before expanding anything, and a budget that cannot be
// checked is refused rather than read as none.
func TestGate3ChecksTheArrivalBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrival coverage.Arrival
		code    string
		pointer string
		subject string
	}{
		{"the clearance exactly spent", coverage.Arrival{RadiusM: 5, ErrorM: 7.5}, "clearance_too_small", "/mission/area", "fog_of_war_east"},
		{"no budget", coverage.Arrival{}, "invalid_arrival_budget", "", ""},
		{"a negative error", coverage.Arrival{RadiusM: 5, ErrorM: -1}, "invalid_arrival_budget", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := referenceInput(t, []byte(referencePlanJSON))
			in.Arrival = tc.arrival
			res := Validate(in)
			if res.OK() || len(res.Diagnostics) != 1 {
				t.Fatalf("diagnostics %+v, want one", res.Diagnostics)
			}
			d := res.Diagnostics[0]
			if d.Gate != GateFeasibility || d.Code != tc.code || d.Pointer != tc.pointer || d.Subject != tc.subject {
				t.Fatalf("diagnostic %+v, want %s at %q on %q", d, tc.code, tc.pointer, tc.subject)
			}
		})
	}
}

func mustHex(t *testing.T, p domain.ApprovedPlan) string {
	t.Helper()
	h, err := eventlog.HexOf(p)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

func TestGateDiagnostics(t *testing.T) {
	type want struct {
		gate    Gate
		code    string
		pointer string
		subject string
	}
	low := func(pct int) func(v *FleetVector) {
		return func(v *FleetVector) {
			if strings.HasPrefix(string(v.Caps.ID), "DRONE-") {
				v.State.BatteryPct = pct
			}
		}
	}
	cases := []struct {
		name   string
		intent string
		edit   func(doc map[string]any)
		world  func(w *World) *World
		want   []want
		check  func(t *testing.T, d []Diagnostic)
	}{
		{
			name:   "intent rewritten by the model",
			intent: "Search the area",
			want:   []want{{GateResolution, "intent_mismatch", "/intent", ""}},
		},
		{
			name: "every unresolved name reported at once",
			edit: func(doc map[string]any) {
				obj(doc, "mission")["area"] = "fog_of_war_west"
				obj(doc, "assignment")["gcs"] = "gcs-east"
				doc["doctrine"] = "recon-standard@9.9.9"
			},
			want: []want{
				{GateResolution, "unknown_gcs", "/assignment/gcs", ""},
				{GateResolution, "unknown_doctrine", "/doctrine", ""},
				{GateResolution, "unknown_area", "/mission/area", ""},
			},
			check: func(t *testing.T, d []Diagnostic) {
				alts := map[string][]string{}
				for _, x := range d {
					alts[x.Code] = x.Alternatives
				}
				if !slices.Equal(alts["unknown_doctrine"], []string{"recon-standard@2.0.0", "recon-standard@2.1.0", "recon-standard@2.2.0"}) ||
					!slices.Equal(alts["unknown_area"], []string{"fog_of_war_east"}) ||
					!slices.Equal(alts["unknown_gcs"], []string{"gcs-west"}) {
					t.Fatalf("alternatives: %v", alts)
				}
			},
		},
		{
			name: "malformed doctrine reference",
			edit: func(doc map[string]any) { doc["doctrine"] = "recon-standard" },
			want: []want{{GateResolution, "unknown_doctrine", "/doctrine", ""}},
		},
		{
			name: "gate 2 stops before gate 3",
			edit: func(doc map[string]any) {
				obj(doc, "tactic")["lanes"] = 5
				obj(doc, "assignment")["gcs"] = "gcs-east"
			},
			want: []want{{GateResolution, "unknown_gcs", "/assignment/gcs", ""}},
		},
		{
			name: "pattern without expansion",
			edit: func(doc map[string]any) { obj(doc, "tactic")["pattern"] = "spiral" },
			want: []want{{GateFeasibility, "unsupported_pattern", "/tactic/pattern", ""}},
		},
		{
			name: "more lanes than capable vectors",
			edit: func(doc map[string]any) { obj(doc, "tactic")["lanes"] = 5 },
			want: []want{{GateFeasibility, "insufficient_vectors", "/tactic/lanes", ""}},
		},
		{
			name: "nobody declares the tag",
			edit: func(doc map[string]any) { obj(doc, "assignment")["requires"] = []any{"sonar"} },
			want: []want{{GateFeasibility, "no_capable_vector", "/assignment/requires", ""}},
		},
		{
			name: "capable vector without a footprint",
			edit: func(doc map[string]any) {
				obj(doc, "assignment")["requires"] = []any{"radio_mesh"}
				obj(doc, "tactic")["lanes"] = 1
			},
			want: []want{{GateFeasibility, "no_sensor_footprint", "/assignment/requires", "RELAY-01"}},
		},
		{
			name:  "no drone has the range",
			world: func(w *World) *World { return withFleet(w, low(20)) },
			want: []want{
				{GateFeasibility, "lane_unassigned", "", "lane-00"},
				{GateFeasibility, "lane_unassigned", "", "lane-01"},
				{GateFeasibility, "lane_unassigned", "", "lane-02"},
				{GateFeasibility, "lane_unassigned", "", "lane-03"},
			},
			check: func(t *testing.T, d []Diagnostic) {
				for _, x := range d {
					if len(x.Candidates) != 6 {
						t.Fatalf("%s lists %d candidates, want the whole fleet", x.Subject, len(x.Candidates))
					}
					for _, c := range x.Candidates {
						if !c.Rejected || c.Reason == "" {
							t.Fatalf("%s: candidate %+v carries no rejection reason", x.Subject, c)
						}
					}
				}
			},
		},
		{
			name: "idle vector below the battery reserve",
			world: func(w *World) *World {
				return withFleet(w, func(v *FleetVector) {
					if v.Caps.ID == "UGV-01" {
						v.State.BatteryPct = 10
					}
				})
			},
			want: []want{{GateDoctrine, "constraint_violated", "/doctrine", "battery-reserve"}},
			check: func(t *testing.T, d []Diagnostic) {
				if !strings.Contains(d[0].Message, "UGV-01") {
					t.Fatalf("the violation does not name the vector: %s", d[0].Message)
				}
			},
		},
		{
			name: "a vector written off is outside the envelope",
			world: func(w *World) *World {
				return withFleet(w, func(v *FleetVector) {
					if v.Caps.ID == "UGV-01" {
						v.State.BatteryPct = 10
						v.State.Mode = domain.ModeDown
					}
				})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(referencePlanJSON)
			if tc.edit != nil {
				raw = mutate(t, tc.edit)
			}
			in := referenceInput(t, raw)
			if tc.intent != "" {
				in.Intent = tc.intent
			}
			if tc.world != nil {
				in.World = tc.world(in.World)
			}
			res := Validate(in)
			if res.OK() != (len(tc.want) == 0) {
				t.Fatalf("OK %v with diagnostics %+v", res.OK(), res.Diagnostics)
			}
			if len(res.Diagnostics) != len(tc.want) {
				t.Fatalf("got %d diagnostics %+v, want %d", len(res.Diagnostics), res.Diagnostics, len(tc.want))
			}
			for i, w := range tc.want {
				g := res.Diagnostics[i]
				if g.Gate != w.gate || g.Code != w.code || g.Pointer != w.pointer || g.Subject != w.subject {
					t.Errorf("diagnostic %d: got %s %s %q %q (%s), want %s %s %q %q", i, g.Gate, g.Code, g.Pointer, g.Subject, g.Message, w.gate, w.code, w.pointer, w.subject)
				}
				if g.Message == "" {
					t.Errorf("diagnostic %d has no message", i)
				}
			}
			if tc.check != nil && len(res.Diagnostics) > 0 {
				tc.check(t, res.Diagnostics)
			}
		})
	}
}
