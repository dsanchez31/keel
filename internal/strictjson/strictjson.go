// Package strictjson decodes JSON against the closed schema of a Go type.
//
// encoding/json alone is lenient in three ways that each turn a malformed
// message into a plausible value: it ignores unknown keys, it matches keys
// case-insensitively, and it leaves a missing field at its zero value, so a
// frame without a longitude lands on the prime meridian. Every boundary that
// takes JSON from outside (the native vehicle protocol, the operator's REST
// bodies) reads it through this package instead, so the repository has one
// definition of "strict" rather than one per boundary.
//
// The schema is the Go type itself, read off its JSON tags: a field tagged
// without omitempty is required, one tagged omitempty is optional, and
// anything else is unknown. A second list of required fields kept beside the
// type would drift from it by construction.
package strictjson

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// Error is a violation of the closed schema, naming where it is.
type Error struct {
	// Path locates the offending value, the root name given to Check then
	// one segment per field: "telemetry.position.lon".
	Path string
	// Problem is what is wrong there: "unknown field", "missing", "want a
	// JSON object".
	Problem string
}

func (e *Error) Error() string { return e.Path + ": " + e.Problem }

// Check holds raw to the fields of the Go type t.
//
// An object key t does not declare is an unknown field. A field t declares
// without omitempty is required, and null counts as absent. Keys match
// exactly. Struct fields and pointers to structs are checked recursively;
// the path of the returned *Error names the offending field, starting from
// root. A t that is not a struct accepts any raw: the value's own type is
// then the decoder's to check.
func Check(raw []byte, t reflect.Type, root string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return &Error{Path: root, Problem: "want a JSON object"}
	}
	known := make(map[string]bool, t.NumField())
	type field struct {
		name     string
		optional bool
		typ      reflect.Type
	}
	var fields []field
	for f := range t.Fields() {
		tag := f.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if !f.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		known[name] = true
		fields = append(fields, field{name: name, optional: slices.Contains(strings.Split(opts, ","), "omitempty"), typ: f.Type})
	}
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if !known[k] {
			return &Error{Path: root + "." + k, Problem: "unknown field"}
		}
	}
	for _, f := range fields {
		v, ok := obj[f.name]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			if f.optional {
				continue
			}
			return &Error{Path: root + "." + f.name, Problem: "missing"}
		}
		if err := Check(v, f.typ, root+"."+f.name); err != nil {
			return err
		}
	}
	return nil
}

// Decode checks raw against the closed schema of T, then decodes it.
//
// raw must hold exactly one JSON value: trailing data after it is refused,
// since a body carrying two objects is not one request.
func Decode[T any](raw []byte, root string) (T, error) {
	var v T
	if !json.Valid(raw) {
		return v, &Error{Path: root, Problem: "not one valid JSON value"}
	}
	if err := Check(raw, reflect.TypeFor[T](), root); err != nil {
		return v, err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, &Error{Path: root, Problem: err.Error()}
	}
	return v, nil
}
