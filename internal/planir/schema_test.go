package planir

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/dsanchez31/keel/internal/coverage"
	"github.com/dsanchez31/keel/internal/domain"
)

func TestReferencePlanPassesSchema(t *testing.T) {
	p, diags := checkSchema([]byte(referencePlanJSON))
	if len(diags) != 0 {
		t.Fatalf("reference plan refused: %+v", diags)
	}
	if p.Tactic.Lanes != 4 || p.Mission.Area != "fog_of_war_east" || p.Assignment.GCS != "gcs-west" {
		t.Fatalf("decoded plan: %+v", p)
	}
}

func TestSchemaDiagnostics(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want []Diagnostic // Gate, Code and Pointer are compared
	}{
		{
			name: "not json",
			raw:  []byte(`{"apiVersion": "keel.plan/v1",`),
			want: []Diagnostic{{Gate: GateSchema, Code: "invalid_json"}},
		},
		{
			name: "trailing document",
			raw:  []byte(referencePlanJSON + ` {}`),
			want: []Diagnostic{{Gate: GateSchema, Code: "invalid_json"}},
		},
		{
			name: "unknown field carrying a coordinate",
			raw: mutate(t, func(doc map[string]any) {
				obj(doc, "tactic")["waypoints"] = []any{[]any{45.0, 5.0}}
			}),
			want: []Diagnostic{{Gate: GateSchema, Code: "additional_property", Pointer: "/tactic/waypoints"}},
		},
		{
			name: "lanes above bound",
			raw:  mutate(t, func(doc map[string]any) { obj(doc, "tactic")["lanes"] = 17 }),
			want: []Diagnostic{{Gate: GateSchema, Code: "maximum", Pointer: "/tactic/lanes"}},
		},
		{
			name: "parallel lanes without orientation",
			raw:  mutate(t, func(doc map[string]any) { delete(obj(doc, "tactic"), "orientation") }),
			want: []Diagnostic{{Gate: GateSchema, Code: "required", Pointer: "/tactic/orientation"}},
		},
		{
			name: "spiral needs no orientation",
			raw: mutate(t, func(doc map[string]any) {
				tactic := obj(doc, "tactic")
				tactic["pattern"] = "spiral"
				delete(tactic, "orientation")
				delete(tactic, "lanes")
			}),
		},
		{
			name: "blank rationale",
			raw:  mutate(t, func(doc map[string]any) { doc["rationale"] = "   " }),
			want: []Diagnostic{{Gate: GateSchema, Code: "pattern", Pointer: "/rationale"}},
		},
		{
			name: "duplicate tag",
			raw: mutate(t, func(doc map[string]any) {
				obj(doc, "assignment")["requires"] = []any{"camera", "camera"}
			}),
			want: []Diagnostic{{Gate: GateSchema, Code: "unique_items", Pointer: "/assignment/requires"}},
		},
		{
			name: "several defects, all reported in pointer order",
			raw: mutate(t, func(doc map[string]any) {
				delete(doc, "rationale")
				obj(doc, "mission")["priority"] = "urgent"
				doc["apiVersion"] = "keel.plan/v2"
				obj(doc, "assignment")["policy"] = "fastest"
			}),
			want: []Diagnostic{
				{Gate: GateSchema, Code: "const", Pointer: "/apiVersion"},
				{Gate: GateSchema, Code: "enum", Pointer: "/assignment/policy"},
				{Gate: GateSchema, Code: "enum", Pointer: "/mission/priority"},
				{Gate: GateSchema, Code: "required", Pointer: "/rationale"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, got := checkSchema(tc.raw)
			if (p == nil) != (len(tc.want) > 0) {
				t.Fatalf("plan %v with diagnostics %+v", p, got)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d diagnostics %+v, want %d", len(got), got, len(tc.want))
			}
			for i, w := range tc.want {
				g := got[i]
				if g.Gate != w.Gate || g.Code != w.Code || g.Pointer != w.Pointer {
					t.Errorf("diagnostic %d: got %s %s %q (%s), want %s %s %q", i, g.Gate, g.Code, g.Pointer, g.Message, w.Gate, w.Code, w.Pointer)
				}
				if g.Message == "" {
					t.Errorf("diagnostic %d has no message", i)
				}
			}
		})
	}
}

// The library walks object properties in map order. Diagnostics of a plan
// with several defects must not depend on it.
func TestSchemaDiagnosticsAreDeterministic(t *testing.T) {
	raw := mutate(t, func(doc map[string]any) {
		doc["extra_a"] = 1
		doc["extra_b"] = 2
		obj(doc, "tactic")["lanes"] = 0
		obj(doc, "mission")["area"] = ""
		delete(doc, "intent")
	})
	_, first := checkSchema(raw)
	for range 100 {
		if _, again := checkSchema(raw); !reflect.DeepEqual(again, first) {
			t.Fatalf("diagnostics changed between runs:\n%+v\n%+v", first, again)
		}
	}
}

func TestModelSchemaStripsUnsupportedKeywords(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(ModelSchema(), &doc); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, s map[string]any)
	walk = func(path string, s map[string]any) {
		for _, k := range modelUnsupported {
			if _, ok := s[k]; ok {
				t.Errorf("%s still carries %q", path, k)
			}
		}
		if s["type"] == "object" && s["additionalProperties"] != false {
			t.Errorf("%s: objects must stay closed", path)
		}
		if props, ok := s["properties"].(map[string]any); ok {
			for name, sub := range props {
				walk(path+"/"+name, sub.(map[string]any))
			}
		}
		if items, ok := s["items"].(map[string]any); ok {
			walk(path+"/items", items)
		}
	}
	walk("", doc)

	tactic := doc["properties"].(map[string]any)["tactic"].(map[string]any)["properties"].(map[string]any)
	if _, ok := tactic["pattern"]; !ok {
		t.Fatal("the property named pattern was stripped along with the keyword")
	}
	if !strings.Contains(tactic["lanes"].(map[string]any)["description"].(string), "1 to 16") {
		t.Fatal("the lanes bound is no longer stated to the model")
	}
}

func TestModelSchemaIsCanonicalNarrowedThenStripped(t *testing.T) {
	var canonical, model map[string]any
	if err := json.Unmarshal(Schema(), &canonical); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ModelSchema(), &model); err != nil {
		t.Fatal(err)
	}
	narrowTactic(canonical)
	stripSchema(canonical)
	if !reflect.DeepEqual(canonical, model) {
		t.Fatal("the model schema is not the canonical schema narrowed, then with keywords removed")
	}
	first, second := ModelSchema(), ModelSchema()
	if string(first) != string(second) {
		t.Fatal("the model schema encoding is not stable")
	}
}

// The model is offered only the patterns the server expands, and the
// conditional that makes orientation and lanes required for them survives the
// stripping as plain required properties.
func TestModelSchemaOffersOnlyExpandablePatterns(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(ModelSchema(), &doc); err != nil {
		t.Fatal(err)
	}
	tactic := doc["properties"].(map[string]any)["tactic"].(map[string]any)
	var enum []domain.Pattern
	for _, v := range tactic["properties"].(map[string]any)["pattern"].(map[string]any)["enum"].([]any) {
		enum = append(enum, domain.Pattern(v.(string)))
	}
	if !reflect.DeepEqual(enum, coverage.ExpandablePatterns()) {
		t.Fatalf("pattern enum %v, want %v", enum, coverage.ExpandablePatterns())
	}
	if !reflect.DeepEqual(tactic["required"], []any{"pattern", "orientation", "lanes"}) {
		t.Fatalf("tactic required %v", tactic["required"])
	}

	// The derived document is a schema in its own right, and decides what the
	// canonical one leaves to gate 3 or to a conditional.
	c := jsonschema.NewCompiler()
	src, err := jsonschema.UnmarshalJSON(bytes.NewReader(ModelSchema()))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddResource("model.json", src); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("model.json")
	if err != nil {
		t.Fatal(err)
	}
	plan := func(tactic string) any {
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(`{"apiVersion":"keel.plan/v1","intent":"i","doctrine":"recon-standard@2.1.0",` +
			`"mission":{"type":"systematic_reconnaissance","priority":"normal","area":"fog_of_war_east"},"tactic":` + tactic + `,` +
			`"assignment":{"policy":"nearest_capable","requires":["camera"]},"rationale":"r"}`))
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	if err := s.Validate(plan(`{"pattern":"parallel_lanes","orientation":"long_axis","lanes":4}`)); err != nil {
		t.Fatalf("a valid tactic refused: %v", err)
	}
	for _, tactic := range []string{`{"pattern":"spiral","orientation":"long_axis","lanes":4}`, `{"pattern":"parallel_lanes","lanes":4}`, `{"pattern":"parallel_lanes","orientation":"long_axis"}`} {
		if s.Validate(plan(tactic)) == nil {
			t.Errorf("tactic %s accepted", tactic)
		}
	}
}
