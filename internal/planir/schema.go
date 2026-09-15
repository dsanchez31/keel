package planir

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/dsanchez31/keel/internal/coverage"
	"github.com/dsanchez31/keel/internal/domain"
)

// schemaJSON is the closed JSON Schema of Plan IR, spec section 5.1.
//
// It is the one definition of the schema. Gate 1 validates against it, and
// ModelSchema derives from it what the planner backends are given, so the two
// cannot drift apart. Every bound is restated in its property description
// because the derivation strips the keywords some backends reject, and the
// description is what carries the bound to the model.
const schemaJSON = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "KEEL Plan IR keel.plan/v1",
  "description": "A reconnaissance mission plan. It declares tactic only: it has no field for a coordinate, the area and the ground station are names the server resolves.",
  "type": "object",
  "additionalProperties": false,
  "required": ["apiVersion", "intent", "doctrine", "mission", "tactic", "assignment", "rationale"],
  "properties": {
    "apiVersion": {
      "description": "Exactly keel.plan/v1.",
      "type": "string",
      "const": "keel.plan/v1"
    },
    "intent": {
      "description": "The operator's intent, copied back verbatim, character for character.",
      "type": "string",
      "minLength": 1,
      "pattern": "\\S"
    },
    "doctrine": {
      "description": "The doctrine pack to fly under, written <name>@<semver>, one of the packs the server lists.",
      "type": "string",
      "minLength": 1,
      "pattern": "\\S"
    },
    "mission": {
      "type": "object",
      "additionalProperties": false,
      "required": ["type", "priority", "area"],
      "properties": {
        "type": {
          "description": "What the mission is for.",
          "type": "string",
          "enum": ["systematic_reconnaissance", "perimeter_patrol"]
        },
        "priority": {
          "description": "Operator-facing urgency.",
          "type": "string",
          "enum": ["low", "normal", "high"]
        },
        "area": {
          "description": "The name of an area of operations the server lists. A name, never coordinates.",
          "type": "string",
          "minLength": 1,
          "pattern": "\\S"
        }
      }
    },
    "tactic": {
      "type": "object",
      "additionalProperties": false,
      "required": ["pattern"],
      "properties": {
        "pattern": {
          "description": "Coverage pattern. Only parallel_lanes can be expanded into lanes today.",
          "type": "string",
          "enum": ["parallel_lanes", "spiral", "perimeter"]
        },
        "orientation": {
          "description": "Direction the lanes run in. Required when pattern is parallel_lanes. long_axis follows the longer side of the area.",
          "type": "string",
          "enum": ["north_south", "east_west", "long_axis"]
        },
        "lanes": {
          "description": "Number of lanes, an integer from 1 to 16. Required when pattern is parallel_lanes. At most one lane per capable vector.",
          "type": "integer",
          "minimum": 1,
          "maximum": 16
        },
        "overlap_pct": {
          "description": "Overlap between adjacent lanes in percent, an integer from 0 to 50. Optional, 10 when omitted.",
          "type": "integer",
          "minimum": 0,
          "maximum": 50
        }
      },
      "if": {
        "properties": {"pattern": {"const": "parallel_lanes"}},
        "required": ["pattern"]
      },
      "then": {
        "required": ["orientation", "lanes"]
      }
    },
    "assignment": {
      "type": "object",
      "additionalProperties": false,
      "required": ["policy", "requires"],
      "properties": {
        "policy": {
          "description": "How lanes are matched to vectors.",
          "type": "string",
          "enum": ["nearest_capable", "round_robin", "lowest_cost"]
        },
        "requires": {
          "description": "Capability tags every vector flying a lane must declare. At least one tag, no duplicates.",
          "type": "array",
          "minItems": 1,
          "uniqueItems": true,
          "items": {
            "type": "string",
            "minLength": 1,
            "pattern": "\\S"
          }
        },
        "gcs": {
          "description": "Optional name of the ground control station vectors return to. A name the server lists, never coordinates.",
          "type": "string",
          "minLength": 1,
          "pattern": "\\S"
        }
      }
    },
    "rationale": {
      "description": "Operator-facing prose explaining the choices, in two or three sentences. Required, not empty.",
      "type": "string",
      "minLength": 1,
      "pattern": "\\S"
    }
  }
}`

// schemaURL is the resource name the schema is compiled under. It never leaves
// the process.
const schemaURL = "keel-plan-v1.json"

var compiled = mustCompile()

func mustCompile() *jsonschema.Schema {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schemaJSON))
	if err != nil {
		panic(fmt.Sprintf("planir: schema is not JSON: %v", err))
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaURL, doc); err != nil {
		panic(fmt.Sprintf("planir: schema resource: %v", err))
	}
	s, err := c.Compile(schemaURL)
	if err != nil {
		panic(fmt.Sprintf("planir: schema does not compile: %v", err))
	}
	return s
}

// Schema returns the canonical schema document, the one gate 1 enforces.
func Schema() json.RawMessage { return json.RawMessage(schemaJSON) }

// modelUnsupported lists the keywords stripped from the schema a planner
// backend is given. Claude structured outputs reject numeric and string
// bounds, complex array constraints and conditionals, and the Go SDK does not
// strip them itself. Removing a keyword only loosens the schema, and gate 1
// still enforces everything removed here.
var modelUnsupported = []string{
	"$schema",
	"else",
	"exclusiveMaximum",
	"exclusiveMinimum",
	"if",
	"maxItems",
	"maxLength",
	"maximum",
	"minLength",
	"minimum",
	"multipleOf",
	"pattern",
	"then",
	"uniqueItems",
}

// ModelSchema derives from the canonical schema the document planner backends
// constrain their output with: the tactic narrowed to what the server can
// expand (narrowTactic), then the unsupported keywords stripped. The encoding
// is deterministic (encoding/json sorts object keys), so a backend that caches
// compiled schemas sees one schema, not one per process.
func ModelSchema() json.RawMessage {
	var doc map[string]any
	dec := json.NewDecoder(strings.NewReader(schemaJSON))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		panic(fmt.Sprintf("planir: schema is not JSON: %v", err))
	}
	narrowTactic(doc)
	stripSchema(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		panic(fmt.Sprintf("planir: encoding model schema: %v", err))
	}
	return out
}

// narrowTactic offers the model only the patterns coverage.Decompose expands.
// The canonical schema accepts the others, which gate 3 refuses (spec section
// 5.5): offering them only spends an attempt.
//
// With the enum narrowed, the tactic's conditional may be decided: when the
// pattern it tests is required and is the only value left, its then clause
// always applies and becomes plain required properties. Structured-output
// modes enforce those, where they reject if and then, which stripSchema would
// remove and leave orientation and lanes optional to the model.
func narrowTactic(doc map[string]any) {
	tactic := schemaObject(doc, "properties", "tactic")
	pattern := schemaObject(tactic, "properties", "pattern")
	enum, _ := pattern["enum"].([]any)
	var allowed []any
	for _, v := range enum {
		if s, ok := v.(string); ok && slices.Contains(coverage.ExpandablePatterns(), domain.Pattern(s)) {
			allowed = append(allowed, v)
		}
	}
	if len(allowed) == 0 {
		panic("planir: no pattern of the schema has an expansion")
	}
	pattern["enum"] = allowed

	required, _ := tactic["required"].([]any)
	cond, _ := tactic["if"].(map[string]any)
	then, _ := tactic["then"].(map[string]any)
	if cond == nil || then == nil || len(allowed) != 1 || !slices.Contains(required, any("pattern")) {
		return
	}
	tested, _ := cond["properties"].(map[string]any)
	if len(tested) != 1 || len(cond) != 2 || tested["pattern"] == nil {
		return
	}
	if c, _ := tested["pattern"].(map[string]any); len(c) != 1 || c["const"] != allowed[0] {
		return
	}
	extra, _ := then["required"].([]any)
	if len(then) != 1 || len(extra) == 0 {
		return
	}
	for _, name := range extra {
		if !slices.Contains(required, name) {
			required = append(required, name)
		}
	}
	tactic["required"] = required
	delete(tactic, "if")
	delete(tactic, "then")
}

// schemaObject walks keys down a decoded schema. The schema is a constant, so
// a missing object is a defect of this file.
func schemaObject(s map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, ok := s[k].(map[string]any)
		if !ok {
			panic(fmt.Sprintf("planir: schema has no object at %q", k))
		}
		s = next
	}
	return s
}

// stripSchema removes modelUnsupported from a schema object and from every
// subschema. Keys under "properties" are property names, not keywords, so the
// walk follows the schema structure rather than every nested map: a property
// named "pattern" survives while the keyword "pattern" does not.
func stripSchema(s map[string]any) {
	for _, k := range modelUnsupported {
		delete(s, k)
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for _, name := range sortedKeys(props) {
			if sub, ok := props[name].(map[string]any); ok {
				stripSchema(sub)
			}
		}
	}
	if items, ok := s["items"].(map[string]any); ok {
		stripSchema(items)
	}
	for _, k := range []string{"allOf", "anyOf", "oneOf"} {
		if subs, ok := s[k].([]any); ok {
			for _, sub := range subs {
				if m, ok := sub.(map[string]any); ok {
					stripSchema(m)
				}
			}
		}
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// printer renders validation messages. English, because every artifact the
// system produces is.
var printer = message.NewPrinter(language.English)

// checkSchema is gate 1. It validates the raw bytes rather than a decoded
// struct, so an unknown field or a wrong type is seen before decoding could
// hide it, then decodes and normalises the plan.
//
// Every violation is reported, one diagnostic per leaf of the validation
// error tree, sorted: the library walks properties in map order, and the
// sort is what makes the list a function of the input.
func checkSchema(raw []byte) (*Plan, []Diagnostic) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, []Diagnostic{{
			Gate:    GateSchema,
			Code:    "invalid_json",
			Message: "the reply is not a single JSON document: " + err.Error(),
		}}
	}
	if err := compiled.Validate(doc); err != nil {
		var verr *jsonschema.ValidationError
		if !errors.As(err, &verr) {
			return nil, []Diagnostic{{Gate: GateSchema, Code: "invalid", Message: err.Error()}}
		}
		var out []Diagnostic
		for _, leaf := range leaves(verr) {
			out = append(out, schemaDiagnostics(leaf)...)
		}
		return nil, sortDiagnostics(out)
	}

	var p Plan
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		// The schema accepted the document, so this is a value the schema
		// allows and Go cannot hold, such as an integer written 4.0.
		d := Diagnostic{Gate: GateSchema, Code: "type", Message: err.Error()}
		var terr *json.UnmarshalTypeError
		if errors.As(err, &terr) && terr.Field != "" {
			d.Pointer = "/" + strings.ReplaceAll(terr.Field, ".", "/")
		}
		return nil, []Diagnostic{d}
	}
	p.normalise()
	return &p, nil
}

// leaves flattens a validation error tree into the errors that carry no
// further cause: the actual violations, rather than the "doesn't validate
// with" wrappers above them.
func leaves(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(e.Causes) == 0 {
		return []*jsonschema.ValidationError{e}
	}
	var out []*jsonschema.ValidationError
	for _, c := range e.Causes {
		out = append(out, leaves(c)...)
	}
	return out
}

// schemaDiagnostics turns one leaf into diagnostics. A missing property and an
// unexpected one each get their own diagnostic pointing at the property,
// which is where a repair has to act.
func schemaDiagnostics(e *jsonschema.ValidationError) []Diagnostic {
	at := pointer(e.InstanceLocation)
	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		out := make([]Diagnostic, len(k.Missing))
		for i, name := range k.Missing {
			out[i] = Diagnostic{
				Gate:    GateSchema,
				Code:    "required",
				Pointer: pointer(append(slices.Clone(e.InstanceLocation), name)),
				Message: fmt.Sprintf("missing required property %q", name),
			}
		}
		return out
	case *kind.AdditionalProperties:
		out := make([]Diagnostic, len(k.Properties))
		for i, name := range k.Properties {
			out[i] = Diagnostic{
				Gate:    GateSchema,
				Code:    "additional_property",
				Pointer: pointer(append(slices.Clone(e.InstanceLocation), name)),
				Message: fmt.Sprintf("property %q is not part of the schema, which is closed", name),
			}
		}
		return out
	}
	return []Diagnostic{{
		Gate:    GateSchema,
		Code:    keywordCode(e.ErrorKind.KeywordPath()),
		Pointer: at,
		Message: e.ErrorKind.LocalizedString(printer),
	}}
}

// pointer renders an instance location as an RFC 6901 JSON pointer. The empty
// location is the whole document, rendered "".
func pointer(loc []string) string {
	var b strings.Builder
	for _, tok := range loc {
		b.WriteByte('/')
		tok = strings.ReplaceAll(tok, "~", "~0")
		b.WriteString(strings.ReplaceAll(tok, "/", "~1"))
	}
	return b.String()
}

// keywordCode names a violation after the schema keyword it broke, in
// snake_case: maxLength becomes max_length.
func keywordCode(path []string) string {
	if len(path) == 0 {
		return "invalid"
	}
	var b strings.Builder
	for i, r := range path[len(path)-1] {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
