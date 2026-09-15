package strictjson

import (
	"errors"
	"reflect"
	"testing"
)

type inner struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type outer struct {
	ID       string  `json:"id"`
	Position inner   `json:"position"`
	Note     string  `json:"note,omitempty"`
	Next     *inner  `json:"next,omitempty"`
	Tags     []inner `json:"tags,omitempty"`
	Ignored  string  `json:"-"`
}

func TestCheck(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // the *Error rendering, empty for a pass
	}{
		{"complete", `{"id":"a","position":{"lat":1,"lon":2}}`, ""},
		{"optional present", `{"id":"a","position":{"lat":1,"lon":2},"note":"n","next":{"lat":0,"lon":0}}`, ""},
		{"optional null", `{"id":"a","position":{"lat":1,"lon":2},"next":null}`, ""},
		{"unknown field", `{"id":"a","position":{"lat":1,"lon":2},"wind":3}`, "body.wind: unknown field"},
		{"key case", `{"ID":"a","position":{"lat":1,"lon":2}}`, "body.ID: unknown field"},
		{"dash field is unknown", `{"id":"a","position":{"lat":1,"lon":2},"Ignored":"x"}`, "body.Ignored: unknown field"},
		{"missing", `{"position":{"lat":1,"lon":2}}`, "body.id: missing"},
		{"null is absent", `{"id":null,"position":{"lat":1,"lon":2}}`, "body.id: missing"},
		{"nested missing", `{"id":"a","position":{"lat":1}}`, "body.position.lon: missing"},
		{"nested through a pointer", `{"id":"a","position":{"lat":1,"lon":2},"next":{"lat":1,"lon":2,"alt":3}}`, "body.next.alt: unknown field"},
		{"not an object", `[1,2]`, "body: want a JSON object"},
		{"nested not an object", `{"id":"a","position":7}`, "body.position: want a JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Check([]byte(tc.raw), reflect.TypeFor[outer](), "body")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Check = %v, want nil", err)
				}
				return
			}
			var se *Error
			if !errors.As(err, &se) {
				t.Fatalf("Check = %v, want a *Error", err)
			}
			if se.Error() != tc.want {
				t.Fatalf("Check = %q, want %q", se.Error(), tc.want)
			}
		})
	}
}

func TestCheckNonStructAcceptsAnything(t *testing.T) {
	if err := Check([]byte(`[1,"x"]`), reflect.TypeFor[[]any](), "body"); err != nil {
		t.Fatalf("Check on a slice type = %v, want nil", err)
	}
}

func TestDecode(t *testing.T) {
	v, err := Decode[outer]([]byte(`{"id":"a","position":{"lat":1.5,"lon":-2}}`), "body")
	if err != nil {
		t.Fatalf("Decode = %v", err)
	}
	if v.ID != "a" || v.Position != (inner{Lat: 1.5, Lon: -2}) {
		t.Fatalf("Decode = %+v", v)
	}

	for name, raw := range map[string]string{
		"invalid JSON":  `{"id":`,
		"trailing data": `{"id":"a","position":{"lat":1,"lon":2}} {}`,
		"wrong type":    `{"id":3,"position":{"lat":1,"lon":2}}`,
		"schema":        `{"id":"a"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var se *Error
			if _, err := Decode[outer]([]byte(raw), "body"); !errors.As(err, &se) {
				t.Fatalf("Decode = %v, want a *Error", err)
			}
		})
	}
}
