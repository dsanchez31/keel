// Package eventlog owns the append-only log, the SHA-256 hash chain and the
// one canonical encoder that everything hashed or compared routes through.
//
// There is exactly one encoder on purpose. A second, slightly different one
// anywhere in the tree makes the whole chain meaningless: two runs that agree
// on every decision would disagree on their head hash, or worse, two runs that
// disagree would collide. Everything that is hashed, diffed or compared for
// equality goes through Canonical.
package eventlog

import (
	"encoding/base64"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Canonical renders v as canonical JSON, per spec.md section 9:
//
//   - UTF-8, no insignificant whitespace
//   - object keys sorted by Unicode code point
//   - integers without exponent, floats via FormatFloat(f, 'g', 17, 64)
//   - absent optional fields omitted, never null
//   - no NaN and no infinity: their presence is a bug, and this reports it
//   - slices preserve order, because a slice that represents a set is sorted
//     at construction rather than here
//
// The last point is worth stating twice. This encoder does not sort slices. A
// set that reaches it unsorted encodes differently from the same set sorted,
// and that is deliberate: sorting here would hide the bug rather than the
// caller fixing it, and it would silently reorder the sequences (waypoint
// paths, candidate lists) whose order is their meaning.
func Canonical(v any) ([]byte, error) {
	var e encoder
	e.buf.Grow(512)
	if err := e.value(reflect.ValueOf(v), ""); err != nil {
		return nil, err
	}
	return []byte(e.buf.String()), nil
}

// MustCanonical is Canonical for call sites where a failure is a programming
// error rather than a condition to handle: a NaN in state, a channel in a
// payload. Tests use it so those surface as a panic at the point of the bug.
func MustCanonical(v any) []byte {
	b, err := Canonical(v)
	if err != nil {
		panic("eventlog: canonical encoding failed: " + err.Error())
	}
	return b
}

// CanonicalString is Canonical returning a string, for comparisons and keys.
func CanonicalString(v any) (string, error) {
	b, err := Canonical(v)
	return string(b), err
}

// RawJSON is a fragment that is already canonical, spliced into the output
// verbatim.
//
// It exists so that a record can carry an encoded payload without the payload
// being encoded twice (as base64, unreadable) or a second encoder being
// written for the enclosing envelope. Only values produced by Canonical
// belong here; anything else silently defeats the guarantee.
type RawJSON []byte

var rawJSONType = reflect.TypeOf(RawJSON(nil))

type encoder struct {
	buf strings.Builder
}

// NonFiniteError reports a NaN or an infinity reaching the encoder. It carries
// the path so the offending field is named rather than hunted for.
type NonFiniteError struct {
	Path  string
	Value float64
}

func (e *NonFiniteError) Error() string {
	return fmt.Sprintf("eventlog: non-finite float at %q: %v", pathOrRoot(e.Path), e.Value)
}

// UnsupportedTypeError reports a type the canonical encoding has no rendering
// for. Channels, functions and complex numbers have no place in state that is
// hashed.
type UnsupportedTypeError struct {
	Path string
	Kind reflect.Kind
}

func (e *UnsupportedTypeError) Error() string {
	return fmt.Sprintf("eventlog: unsupported type %s at %q", e.Kind, pathOrRoot(e.Path))
}

func pathOrRoot(p string) string {
	if p == "" {
		return "$"
	}
	return p
}

func (e *encoder) value(v reflect.Value, path string) error {
	if !v.IsValid() {
		e.buf.WriteString("null")
		return nil
	}

	if v.Type() == rawJSONType {
		if v.Len() == 0 {
			e.buf.WriteString("null")
			return nil
		}
		e.buf.Write(v.Bytes())
		return nil
	}

	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			e.buf.WriteString("null")
			return nil
		}
		return e.value(v.Elem(), path)

	case reflect.Bool:
		if v.Bool() {
			e.buf.WriteString("true")
		} else {
			e.buf.WriteString("false")
		}
		return nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		e.buf.WriteString(strconv.FormatInt(v.Int(), 10))
		return nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		e.buf.WriteString(strconv.FormatUint(v.Uint(), 10))
		return nil

	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return &NonFiniteError{Path: path, Value: f}
		}
		// 'g' with 17 significant digits round-trips every float64 exactly,
		// which is what makes two runs comparable byte for byte.
		e.buf.WriteString(strconv.FormatFloat(f, 'g', 17, 64))
		return nil

	case reflect.String:
		e.writeString(v.String())
		return nil

	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			// Byte slices are opaque payloads. Base64 keeps them one token
			// and keeps the encoding stable across platforms.
			e.buf.WriteByte('"')
			e.buf.WriteString(base64.StdEncoding.EncodeToString(v.Bytes()))
			e.buf.WriteByte('"')
			return nil
		}
		if v.IsNil() {
			e.buf.WriteString("[]")
			return nil
		}
		return e.array(v, path)

	case reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			b := make([]byte, v.Len())
			reflect.Copy(reflect.ValueOf(b), v)
			e.buf.WriteByte('"')
			e.buf.WriteString(base64.StdEncoding.EncodeToString(b))
			e.buf.WriteByte('"')
			return nil
		}
		return e.array(v, path)

	case reflect.Map:
		return e.mapValue(v, path)

	case reflect.Struct:
		return e.structValue(v, path)

	default:
		return &UnsupportedTypeError{Path: path, Kind: v.Kind()}
	}
}

func (e *encoder) array(v reflect.Value, path string) error {
	e.buf.WriteByte('[')
	for i := range v.Len() {
		if i > 0 {
			e.buf.WriteByte(',')
		}
		if err := e.value(v.Index(i), path+"["+strconv.Itoa(i)+"]"); err != nil {
			return err
		}
	}
	e.buf.WriteByte(']')
	return nil
}

// mapValue renders a map with its keys sorted by Unicode code point.
//
// This is the one place in the tree where a map is iterated, and it is safe
// precisely because the keys are collected and sorted first. The decision path
// does not reach here: it hands over already-built values.
func (e *encoder) mapValue(v reflect.Value, path string) error {
	if v.IsNil() {
		e.buf.WriteString("{}")
		return nil
	}

	type kv struct {
		key string
		val reflect.Value
	}
	entries := make([]kv, 0, v.Len())
	iter := v.MapRange()
	for iter.Next() {
		k, err := mapKeyString(iter.Key())
		if err != nil {
			return fmt.Errorf("at %s: %w", pathOrRoot(path), err)
		}
		entries = append(entries, kv{key: k, val: iter.Value()})
	}
	slices.SortFunc(entries, func(a, b kv) int { return strings.Compare(a.key, b.key) })

	e.buf.WriteByte('{')
	for i, en := range entries {
		if i > 0 {
			e.buf.WriteByte(',')
		}
		e.writeString(en.key)
		e.buf.WriteByte(':')
		if err := e.value(en.val, path+"."+en.key); err != nil {
			return err
		}
	}
	e.buf.WriteByte('}')
	return nil
}

func mapKeyString(k reflect.Value) (string, error) {
	switch k.Kind() {
	case reflect.String:
		return k.String(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(k.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(k.Uint(), 10), nil
	default:
		return "", fmt.Errorf("map key of kind %s has no canonical rendering", k.Kind())
	}
}

func (e *encoder) structValue(v reflect.Value, path string) error {
	fields := fieldsOf(v.Type())

	// Fields are pre-sorted by JSON name, so the object is emitted in key
	// order without sorting per value.
	e.buf.WriteByte('{')
	first := true
	for _, f := range fields {
		fv := v
		ok := true
		for _, idx := range f.index {
			if fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					ok = false
					break
				}
				fv = fv.Elem()
			}
			fv = fv.Field(idx)
		}
		if !ok {
			continue
		}

		// A nil pointer field is an absent optional. It is omitted whether or
		// not it is tagged omitempty: spec section 9 says absent optionals are
		// omitted, never emitted as null, and that holds for every field.
		if fv.Kind() == reflect.Pointer && fv.IsNil() {
			continue
		}
		if fv.Kind() == reflect.Interface && fv.IsNil() {
			continue
		}
		if f.omitEmpty && isEmpty(fv) {
			continue
		}

		if !first {
			e.buf.WriteByte(',')
		}
		first = false
		e.writeString(f.name)
		e.buf.WriteByte(':')
		if err := e.value(fv, path+"."+f.name); err != nil {
			return err
		}
	}
	e.buf.WriteByte('}')
	return nil
}

func isEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// writeString emits a JSON string with the minimal escape set of RFC 8259.
//
// encoding/json escapes <, > and & for HTML safety. That is a rendering
// choice, and a rendering choice inside a hash function is a way for the hash
// to change when nothing about the data did. The escape set here is fixed by
// the specification and by nothing else.
func (e *encoder) writeString(s string) {
	e.buf.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				e.buf.WriteString(`\"`)
			case '\\':
				e.buf.WriteString(`\\`)
			case '\n':
				e.buf.WriteString(`\n`)
			case '\r':
				e.buf.WriteString(`\r`)
			case '\t':
				e.buf.WriteString(`\t`)
			case '\b':
				e.buf.WriteString(`\b`)
			case '\f':
				e.buf.WriteString(`\f`)
			default:
				if c < 0x20 {
					e.buf.WriteString(`\u00`)
					const hex = "0123456789abcdef"
					e.buf.WriteByte(hex[c>>4])
					e.buf.WriteByte(hex[c&0xF])
				} else {
					e.buf.WriteByte(c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// Invalid UTF-8 becomes U+FFFD, so the output is always valid
			// UTF-8 and the same input always produces the same bytes.
			e.buf.WriteString(`�`)
			i++
			continue
		}
		e.buf.WriteString(s[i : i+size])
		i += size
	}
	e.buf.WriteByte('"')
}

// field describes one encodable struct field, resolved once per type.
type field struct {
	name      string
	index     []int
	omitEmpty bool
}

var fieldCache sync.Map // reflect.Type -> []field

// fieldsOf resolves the encodable fields of a struct type, flattening
// anonymous embedded structs and sorting by JSON name so the emitted object is
// already in key order.
func fieldsOf(t reflect.Type) []field {
	if cached, ok := fieldCache.Load(t); ok {
		return cached.([]field)
	}
	fields := collectFields(t, nil)
	slices.SortFunc(fields, func(a, b field) int { return strings.Compare(a.name, b.name) })
	fieldCache.Store(t, fields)
	return fields
}

func collectFields(t reflect.Type, prefix []int) []field {
	var out []field
	for i := range t.NumField() {
		sf := t.Field(i)
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")

		ft := sf.Type
		if sf.Anonymous && name == "" {
			base := ft
			if base.Kind() == reflect.Pointer {
				base = base.Elem()
			}
			if base.Kind() == reflect.Struct {
				out = append(out, collectFields(base, append(append([]int(nil), prefix...), i))...)
				continue
			}
		}
		if !sf.IsExported() {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		out = append(out, field{
			name:      name,
			index:     append(append([]int(nil), prefix...), i),
			omitEmpty: strings.Contains(","+opts+",", ",omitempty,"),
		})
	}
	return out
}
