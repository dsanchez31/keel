package httpjson

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type body struct {
	Name  string `json:"name"`
	Count int    `json:"count,omitempty"`
}

func read(ctype, payload string) (*httptest.ResponseRecorder, body, bool) {
	req := httptest.NewRequest("POST", "/", strings.NewReader(payload))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	rec := httptest.NewRecorder()
	v, ok := ReadBody[body](rec, req)
	return rec, v, ok
}

func problemOf(t *testing.T, rec *httptest.ResponseRecorder, status int) Problem {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content type %q, want application/problem+json", ct)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem body %s: %v", rec.Body, err)
	}
	if p.Status != status || p.Title != http.StatusText(status) {
		t.Fatalf("problem %+v for status %d", p, status)
	}
	return p
}

func TestReadBodyAccepts(t *testing.T) {
	rec, v, ok := read("application/json; charset=utf-8", `{"name":"DRONE-02","count":3}`)
	if !ok || v != (body{Name: "DRONE-02", Count: 3}) {
		t.Fatalf("read %+v, ok %v: %s", v, ok, rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("an accepted body was answered: %s", rec.Body)
	}
	if _, v, ok := read("application/json", `{"name":"DRONE-02"}`); !ok || v.Count != 0 {
		t.Fatalf("an omitempty field is optional: %+v, ok %v", v, ok)
	}
}

func TestReadBodyRefuses(t *testing.T) {
	cases := []struct {
		name, ctype, payload string
		status               int
		detail               string
	}{
		{"not JSON media type", "text/plain", `{"name":"a"}`, http.StatusUnsupportedMediaType, "application/json"},
		{"no media type", "", `{"name":"a"}`, http.StatusUnsupportedMediaType, "application/json"},
		{"unknown field", "application/json", `{"name":"a","extra":1}`, http.StatusBadRequest, "body.extra: unknown field"},
		{"key case", "application/json", `{"Name":"a"}`, http.StatusBadRequest, "body.Name: unknown field"},
		{"missing field", "application/json", `{"count":1}`, http.StatusBadRequest, "body.name: missing"},
		{"null field", "application/json", `{"name":null}`, http.StatusBadRequest, "body.name: missing"},
		{"two values", "application/json", `{"name":"a"} {"name":"b"}`, http.StatusBadRequest, "not one valid JSON value"},
		{"too large", "application/json", `{"name":"` + strings.Repeat("x", MaxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge, "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, _, ok := read(tc.ctype, tc.payload)
			if ok {
				t.Fatal("accepted")
			}
			if p := problemOf(t, rec, tc.status); !strings.Contains(p.Detail, tc.detail) {
				t.Fatalf("detail %q, want it to mention %q", p.Detail, tc.detail)
			}
		})
	}
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusAccepted, map[string]any{"b": 1, "a": "x"})
	if rec.Code != http.StatusAccepted || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if got, want := rec.Body.String(), `{"a":"x","b":1}`; got != want {
		t.Fatalf("body %s, want the canonical %s", got, want)
	}
}

func TestWriteJSONErrorStatusIsAProblem(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, http.StatusConflict, "a mission is running")
	if p := problemOf(t, rec, http.StatusConflict); p.Detail != "a mission is running" {
		t.Fatalf("detail %q", p.Detail)
	}
}

func TestWriteJSONRefusedValueIsAServerError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, map[string]float64{"x": math.NaN()})
	if p := problemOf(t, rec, http.StatusInternalServerError); p.Detail != "internal error" {
		t.Fatalf("detail %q", p.Detail)
	}
}
