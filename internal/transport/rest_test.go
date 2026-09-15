package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/httpjson"
	"github.com/dsanchez31/keel/internal/planner"
)

// fakeService answers from its fields and records what it was asked.
type fakeService struct {
	compile  func(CompileRequest) (planner.Outcome, error)
	approve  func(domain.PlanHash) (domain.MissionID, error)
	discard  func(domain.PlanHash) error
	mission  func(domain.MissionID) (MissionView, error)
	swap     func(domain.MissionID, domain.DoctrineRef) error
	packs    func() ([]DoctrineInfo, error)
	world    func() (WorldView, error)
	fleet    func() ([]FleetVectorView, error)
	fault    func(domain.Fault) error
	replay   func(domain.MissionID) (io.ReadCloser, error)
	window   func(domain.MissionID, int64, int64) (ReplayWindow, error)
	setClock func(int) (ClockView, error)
	clock    ClockView
	compiled []CompileRequest
	faults   []domain.Fault
}

var errUnset = errors.New("fake: not set")

func (f *fakeService) Compile(_ context.Context, req CompileRequest) (planner.Outcome, error) {
	f.compiled = append(f.compiled, req)
	if f.compile == nil {
		return planner.Outcome{}, errUnset
	}
	return f.compile(req)
}

func (f *fakeService) Approve(_ context.Context, h domain.PlanHash) (domain.MissionID, error) {
	if f.approve == nil {
		return "", errUnset
	}
	return f.approve(h)
}

func (f *fakeService) Discard(_ context.Context, h domain.PlanHash) error {
	if f.discard == nil {
		return errUnset
	}
	return f.discard(h)
}

func (f *fakeService) Mission(_ context.Context, id domain.MissionID) (MissionView, error) {
	if f.mission == nil {
		return MissionView{}, errUnset
	}
	return f.mission(id)
}

func (f *fakeService) SwapDoctrine(_ context.Context, id domain.MissionID, ref domain.DoctrineRef) error {
	if f.swap == nil {
		return errUnset
	}
	return f.swap(id, ref)
}

func (f *fakeService) Doctrines(context.Context) ([]DoctrineInfo, error) {
	if f.packs == nil {
		return nil, errUnset
	}
	return f.packs()
}

func (f *fakeService) World(context.Context) (WorldView, error) {
	if f.world == nil {
		return WorldView{}, errUnset
	}
	return f.world()
}

func (f *fakeService) Fleet(context.Context) ([]FleetVectorView, error) {
	if f.fleet == nil {
		return nil, errUnset
	}
	return f.fleet()
}

func (f *fakeService) InjectFault(_ context.Context, fault domain.Fault) error {
	f.faults = append(f.faults, fault)
	if f.fault == nil {
		return errUnset
	}
	return f.fault(fault)
}

func (f *fakeService) Replay(_ context.Context, id domain.MissionID) (io.ReadCloser, error) {
	if f.replay == nil {
		return nil, errUnset
	}
	return f.replay(id)
}

func (f *fakeService) ReplayWindow(_ context.Context, id domain.MissionID, fromMs, toMs int64) (ReplayWindow, error) {
	if f.window == nil {
		return ReplayWindow{}, errUnset
	}
	return f.window(id, fromMs, toMs)
}

func (f *fakeService) Clock(context.Context) (ClockView, error) { return f.clock, nil }

func (f *fakeService) SetClock(_ context.Context, speed int) (ClockView, error) {
	if f.setClock == nil {
		return ClockView{}, errUnset
	}
	return f.setClock(speed)
}

func newTestHandler(t *testing.T, svc Service, opts Options) http.Handler {
	t.Helper()
	hub, err := NewHub(HubOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	h, err := NewHandler(svc, hub, opts)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// call sends one request. A body is sent as application/json unless a
// Content-Type header says otherwise.
func call(h http.Handler, method, path, body string, header ...string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
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

func decodeOK[T any](t *testing.T, rec *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q, want application/json", ct)
	}
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	return v
}

const planHash = "3f2c9a0e5b7d41c8a6e2f90b1d3c5a7e9f0b2d4c6e8a0b1c3d5e7f9a1b3c5d7e"

func TestCompile(t *testing.T) {
	plan := &domain.ApprovedPlan{Hash: planHash, Intent: "sweep"}
	svc := &fakeService{compile: func(req CompileRequest) (planner.Outcome, error) {
		return planner.Outcome{Backend: "ollama", Intent: req.Intent, Plan: plan, Attempts: []planner.Attempt{{N: 1, Reply: "{}"}}}, nil
	}}
	h := newTestHandler(t, svc, Options{})

	out := decodeOK[planner.Outcome](t, call(h, "POST", "/api/v1/plans", `{"intent":"sweep","backend":"claude"}`), http.StatusOK)
	if out.Plan == nil || out.Plan.Hash != planHash || len(out.Attempts) != 1 {
		t.Fatalf("outcome %+v", out)
	}
	if want := (CompileRequest{Intent: "sweep", Backend: "claude"}); len(svc.compiled) != 1 || svc.compiled[0] != want {
		t.Fatalf("service asked %+v, want %+v", svc.compiled, want)
	}
}

func TestCompileRefusedIsAnOutcome(t *testing.T) {
	svc := &fakeService{compile: func(req CompileRequest) (planner.Outcome, error) {
		return planner.Outcome{Backend: "ollama", Intent: req.Intent, Attempts: []planner.Attempt{{N: 1}, {N: 2}, {N: 3}}}, nil
	}}
	out := decodeOK[planner.Outcome](t, call(newTestHandler(t, svc, Options{}), "POST", "/api/v1/plans", `{"intent":"sweep"}`), http.StatusOK)
	if out.OK() || len(out.Attempts) != planner.MaxAttempts {
		t.Fatalf("a refused compilation answers 200 with its attempts, got %+v", out)
	}
}

func TestCompileBackendFailure(t *testing.T) {
	svc := &fakeService{compile: func(req CompileRequest) (planner.Outcome, error) {
		out := planner.Outcome{Backend: "ollama", Intent: req.Intent, Attempts: []planner.Attempt{{N: 1, Reply: "{}"}}}
		return out, fmt.Errorf("attempt 2 of 3: %w: connection refused", planner.ErrBackend)
	}}
	p := problemOf(t, call(newTestHandler(t, svc, Options{}), "POST", "/api/v1/plans", `{"intent":"sweep"}`), http.StatusBadGateway)
	if !strings.Contains(p.Detail, "connection refused") || p.Outcome == nil || len(p.Outcome.Attempts) != 1 {
		t.Fatalf("a backend failure carries its cause and the attempts made: %+v", p)
	}
}

func TestBodies(t *testing.T) {
	h := newTestHandler(t, &fakeService{}, Options{})
	cases := []struct {
		name   string
		body   string
		header []string
		status int
		detail string
	}{
		{"not JSON media type", `{"intent":"sweep"}`, []string{"Content-Type", "text/plain"}, http.StatusUnsupportedMediaType, "application/json"},
		{"no media type", `{"intent":"sweep"}`, []string{"Content-Type", ""}, http.StatusUnsupportedMediaType, "application/json"},
		{"area is not a field", `{"intent":"sweep","area":"fog_of_war_east"}`, nil, http.StatusBadRequest, "body.area: unknown field"},
		{"key case", `{"Intent":"sweep"}`, nil, http.StatusBadRequest, "body.Intent: unknown field"},
		{"missing intent", `{"backend":"ollama"}`, nil, http.StatusBadRequest, "body.intent: missing"},
		{"null intent", `{"intent":null}`, nil, http.StatusBadRequest, "body.intent: missing"},
		{"two values", `{"intent":"a"} {"intent":"b"}`, nil, http.StatusBadRequest, "not one valid JSON value"},
		{"wrong type", `{"intent":7}`, nil, http.StatusBadRequest, "body"},
		{"blank intent", `{"intent":"  "}`, nil, http.StatusUnprocessableEntity, "intent is empty"},
		{"too large", `{"intent":"` + strings.Repeat("x", httpjson.MaxBodyBytes) + `"}`, nil, http.StatusRequestEntityTooLarge, "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := problemOf(t, call(h, "POST", "/api/v1/plans", tc.body, tc.header...), tc.status)
			if !strings.Contains(p.Detail, tc.detail) {
				t.Fatalf("detail %q, want it to mention %q", p.Detail, tc.detail)
			}
		})
	}
}

func TestApprove(t *testing.T) {
	svc := &fakeService{approve: func(h domain.PlanHash) (domain.MissionID, error) {
		if h != planHash {
			return "", fmt.Errorf("%w: no pending plan %s", ErrNotFound, h)
		}
		return "MSN-042", nil
	}}
	h := newTestHandler(t, svc, Options{})

	rec := call(h, "POST", "/api/v1/plans/"+planHash+"/approve", "")
	a := decodeOK[Approval](t, rec, http.StatusAccepted)
	if a != (Approval{Mission: "MSN-042", Plan: planHash}) {
		t.Fatalf("approval %+v", a)
	}
	if loc := rec.Header().Get("Location"); loc != "/api/v1/missions/MSN-042" {
		t.Fatalf("Location %q", loc)
	}
	p := problemOf(t, call(h, "POST", "/api/v1/plans/0000/approve", ""), http.StatusNotFound)
	if !strings.Contains(p.Detail, "no pending plan 0000") {
		t.Fatalf("detail %q", p.Detail)
	}
}

func TestDiscard(t *testing.T) {
	var got domain.PlanHash
	svc := &fakeService{discard: func(h domain.PlanHash) error { got = h; return nil }}
	rec := call(newTestHandler(t, svc, Options{}), "DELETE", "/api/v1/plans/"+planHash, "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 || got != planHash {
		t.Fatalf("discard: status %d, body %q, service got %q", rec.Code, rec.Body, got)
	}
}

func TestMission(t *testing.T) {
	svc := &fakeService{mission: func(id domain.MissionID) (MissionView, error) {
		if id != "MSN-042" {
			return MissionView{}, fmt.Errorf("%w: mission %s", ErrNotFound, id)
		}
		return MissionView{Tick: 7, Mission: domain.Mission{ID: id, State: domain.MissionRunning}}, nil
	}}
	h := newTestHandler(t, svc, Options{})
	v := decodeOK[MissionView](t, call(h, "GET", "/api/v1/missions/MSN-042", ""), http.StatusOK)
	if v.Tick != 7 || v.Mission.ID != "MSN-042" || v.Mission.State != domain.MissionRunning {
		t.Fatalf("view %+v", v)
	}
	problemOf(t, call(h, "GET", "/api/v1/missions/MSN-001", ""), http.StatusNotFound)
}

func TestSwapDoctrine(t *testing.T) {
	var gotID domain.MissionID
	var gotRef domain.DoctrineRef
	svc := &fakeService{swap: func(id domain.MissionID, ref domain.DoctrineRef) error {
		if id != "MSN-042" {
			return fmt.Errorf("%w: no mission running", ErrConflict)
		}
		gotID, gotRef = id, ref
		return nil
	}}
	h := newTestHandler(t, svc, Options{})

	rec := call(h, "POST", "/api/v1/missions/MSN-042/doctrine", `{"name":"recon-standard","version":"2.2.0"}`)
	if rec.Code != http.StatusAccepted || gotID != "MSN-042" || gotRef != (domain.DoctrineRef{Name: "recon-standard", Version: "2.2.0"}) {
		t.Fatalf("swap: status %d, service got %s %+v", rec.Code, gotID, gotRef)
	}
	problemOf(t, call(h, "POST", "/api/v1/missions/MSN-042/doctrine", `{"name":"recon-standard"}`), http.StatusBadRequest)
	problemOf(t, call(h, "POST", "/api/v1/missions/MSN-001/doctrine", `{"name":"recon-standard","version":"2.2.0"}`), http.StatusConflict)
}

func TestDoctrines(t *testing.T) {
	svc := &fakeService{packs: func() ([]DoctrineInfo, error) {
		return []DoctrineInfo{{Ref: "recon-standard@2.2.0", Name: "recon-standard", Version: "2.2.0", Hash: "ab"}}, nil
	}}
	packs := decodeOK[[]DoctrineInfo](t, call(newTestHandler(t, svc, Options{}), "GET", "/api/v1/doctrine", ""), http.StatusOK)
	if len(packs) != 1 || packs[0].Ref != "recon-standard@2.2.0" {
		t.Fatalf("packs %+v", packs)
	}
}

func TestWorld(t *testing.T) {
	svc := &fakeService{world: func() (WorldView, error) {
		return WorldView{Name: "reference", Areas: []AreaView{{Name: "fog_of_war_east", CellM: 50, ScanAltM: 120, AreaKm2: 14.3}}}, nil
	}}
	w := decodeOK[WorldView](t, call(newTestHandler(t, svc, Options{}), "GET", "/api/v1/world", ""), http.StatusOK)
	if w.Name != "reference" || len(w.Areas) != 1 || w.Areas[0].Name != "fog_of_war_east" {
		t.Fatalf("world %+v", w)
	}
}

func TestFleet(t *testing.T) {
	svc := &fakeService{fleet: func() ([]FleetVectorView, error) {
		return []FleetVectorView{
			{ID: "DRONE-01", Health: domain.HealthLost, Detail: "no frame yet; system 1 has no absolute position estimate"},
			{ID: "DRONE-02", Health: domain.HealthOK},
		}, nil
	}}
	rec := call(newTestHandler(t, svc, Options{}), "GET", "/api/v1/fleet", "")
	want := `[{"detail":"no frame yet; system 1 has no absolute position estimate","health":"lost","id":"DRONE-01"},{"health":"ok","id":"DRONE-02"}]`
	decodeOK[[]FleetVectorView](t, rec, http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("fleet %s, want %s", got, want)
	}
}

func TestInjectFault(t *testing.T) {
	svc := &fakeService{fault: func(f domain.Fault) error {
		if f.Vector != "DRONE-02" {
			return fmt.Errorf("%w: unknown vector %s", ErrInvalid, f.Vector)
		}
		return nil
	}}
	h := newTestHandler(t, svc, Options{})

	rec := call(h, "POST", "/api/v1/faults", `{"vector":"DRONE-02","kind":"link_loss","duration_ms":5000}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if want := (domain.Fault{Kind: domain.FaultLinkLoss, Vector: "DRONE-02", DurationMs: 5000}); len(svc.faults) != 1 || svc.faults[0] != want {
		t.Fatalf("service got %+v, want %+v", svc.faults, want)
	}
	problemOf(t, call(h, "POST", "/api/v1/faults", `{"vector":"DRONE-09","kind":"kill"}`), http.StatusUnprocessableEntity)
	svc.fault = func(domain.Fault) error { return fmt.Errorf("%w: the simulator is not answering", ErrUpstream) }
	if p := problemOf(t, call(h, "POST", "/api/v1/faults", `{"vector":"DRONE-02","kind":"kill"}`), http.StatusBadGateway); !strings.Contains(p.Detail, "not answering") {
		t.Errorf("an upstream failure: detail %q", p.Detail)
	}
	// Fault.Check refuses before the service is asked.
	for body, detail := range map[string]string{
		`{"kind":"kill"}`:                                     "vector is required",
		`{"vector":"DRONE-02","kind":"meteor"}`:               "unknown kind",
		`{"vector":"DRONE-02","kind":"battery_drain"}`:        "finite positive magnitude",
		`{"vector":"DRONE-02","kind":"kill","duration_ms":5}`: "whole number of ticks",
	} {
		if p := problemOf(t, call(h, "POST", "/api/v1/faults", body), http.StatusUnprocessableEntity); !strings.Contains(p.Detail, detail) {
			t.Errorf("%s: detail %q, want it to mention %q", body, p.Detail, detail)
		}
	}
	problemOf(t, call(h, "POST", "/api/v1/faults", `{"vector":"DRONE-02"}`), http.StatusBadRequest)
	problemOf(t, call(h, "POST", "/api/v1/faults", `{"vector":"DRONE-02","kind":"kill","params":{}}`), http.StatusBadRequest)
	if len(svc.faults) != 3 {
		t.Fatalf("refused bodies reached the service: %+v", svc.faults)
	}
}

func TestClock(t *testing.T) {
	svc := &fakeService{clock: ClockView{Speed: 1, MaxSpeed: 20}}
	svc.setClock = func(speed int) (ClockView, error) {
		if speed == 5 {
			return ClockView{}, fmt.Errorf("%w: UGV-01 is not simulated", ErrConflict)
		}
		svc.clock.Speed = speed
		return svc.clock, nil
	}
	h := newTestHandler(t, svc, Options{})

	if got := decodeOK[ClockView](t, call(h, "GET", "/api/v1/clock", ""), http.StatusOK); got != (ClockView{Speed: 1, MaxSpeed: 20}) {
		t.Fatalf("GET %+v", got)
	}
	if got := decodeOK[ClockView](t, call(h, "PUT", "/api/v1/clock", `{"speed":20}`), http.StatusOK); got.Speed != 20 {
		t.Fatalf("PUT %+v", got)
	}
	if p := problemOf(t, call(h, "PUT", "/api/v1/clock", `{"speed":5}`), http.StatusConflict); !strings.Contains(p.Detail, "UGV-01") {
		t.Errorf("conflict detail %q", p.Detail)
	}
	svc.setClock = func(int) (ClockView, error) {
		return ClockView{}, fmt.Errorf("%w: SITL DRONE-01 did not confirm", ErrUpstream)
	}
	problemOf(t, call(h, "PUT", "/api/v1/clock", `{"speed":2}`), http.StatusBadGateway)
	// Checked before the service is asked.
	svc.setClock = func(int) (ClockView, error) { t.Fatal("a refused pace reached the service"); return ClockView{}, nil }
	problemOf(t, call(h, "PUT", "/api/v1/clock", `{"speed":0}`), http.StatusUnprocessableEntity)
	problemOf(t, call(h, "PUT", "/api/v1/clock", `{"speed":21}`), http.StatusUnprocessableEntity)
	problemOf(t, call(h, "PUT", "/api/v1/clock", `{"speed":1.5}`), http.StatusBadRequest)
	problemOf(t, call(h, "PUT", "/api/v1/clock", `{"speed":2}`, "Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example"), http.StatusForbidden)
	if rec := call(h, "POST", "/api/v1/clock", `{"speed":2}`); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD, PUT" {
		t.Errorf("POST: status %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

func TestReplay(t *testing.T) {
	const log = `{"hash":"a","kind":"header","payload":{},"prev_hash":"0","seq":1,"tick_ms":0}` + "\n"
	rc := &closeRecorder{Reader: strings.NewReader(log)}
	svc := &fakeService{replay: func(id domain.MissionID) (io.ReadCloser, error) {
		if id != "MSN-042" {
			return nil, fmt.Errorf("%w: no log for %s", ErrNotFound, id)
		}
		return rc, nil
	}}
	h := newTestHandler(t, svc, Options{})

	rec := call(h, "GET", "/api/v1/replays/MSN-042", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-ndjson" || rec.Body.String() != log {
		t.Fatalf("replay: status %d, type %q, body %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	if !rc.closed {
		t.Fatal("the log was not closed")
	}
	problemOf(t, call(h, "GET", "/api/v1/replays/MSN-001", ""), http.StatusNotFound)
}

func TestReplayWindow(t *testing.T) {
	svc := &fakeService{window: func(id domain.MissionID, from, to int64) (ReplayWindow, error) {
		if id != "MSN-042" {
			return ReplayWindow{}, fmt.Errorf("%w: no log for %s", ErrNotFound, id)
		}
		return ReplayWindow{FromMs: from, ToMs: to, Frames: []EncodedFrame{{Data: []byte(`{"tick":1}`), Seq: 1, T: 100, Type: FrameTick}}, VerifiedMs: to}, nil
	}}
	h := newTestHandler(t, svc, Options{})

	rec := call(h, "GET", "/api/v1/replays/MSN-042/frames?from=0&to=60000", "")
	want := `{"frames":[{"data":{"tick":1},"seq":1,"t":100,"type":"tick"}],"from_ms":0,"to_ms":60000,"verified_ms":60000}`
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("window: status %d, body %s", rec.Code, rec.Body)
	}
	problemOf(t, call(h, "GET", "/api/v1/replays/MSN-042/frames?from=0", ""), http.StatusBadRequest)
	problemOf(t, call(h, "GET", "/api/v1/replays/MSN-042/frames?from=5&to=1", ""), http.StatusUnprocessableEntity)
	problemOf(t, call(h, "GET", "/api/v1/replays/MSN-042/frames?from=0&to=3600000", ""), http.StatusUnprocessableEntity)
	problemOf(t, call(h, "GET", "/api/v1/replays/MSN-001/frames?from=0&to=1", ""), http.StatusNotFound)
}

func TestServerErrorIsNotDisclosed(t *testing.T) {
	var logs bytes.Buffer
	svc := &fakeService{packs: func() ([]DoctrineInfo, error) {
		return nil, errors.New("reading /srv/keel/doctrine-packs: permission denied")
	}}
	h := newTestHandler(t, svc, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})

	p := problemOf(t, call(h, "GET", "/api/v1/doctrine", ""), http.StatusInternalServerError)
	if strings.Contains(p.Detail, "permission denied") {
		t.Fatalf("a 500 discloses its cause: %q", p.Detail)
	}
	if !strings.Contains(logs.String(), "permission denied") {
		t.Fatalf("the cause was not logged: %q", logs.String())
	}
}

func TestCrossOriginProtection(t *testing.T) {
	svc := &fakeService{approve: func(domain.PlanHash) (domain.MissionID, error) { return "MSN-042", nil }}
	h := newTestHandler(t, svc, Options{TrustedOrigins: []string{"http://localhost:5173"}})
	path := "/api/v1/plans/" + planHash + "/approve"

	p := problemOf(t, call(h, "POST", path, "", "Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example"), http.StatusForbidden)
	if !strings.Contains(p.Detail, "cross-origin") {
		t.Fatalf("detail %q", p.Detail)
	}
	problemOf(t, call(h, "POST", path, "", "Origin", "https://evil.example"), http.StatusForbidden)
	for name, header := range map[string][]string{
		"same origin":    {"Sec-Fetch-Site", "same-origin"},
		"trusted origin": {"Sec-Fetch-Site", "cross-site", "Origin", "http://localhost:5173"},
		"not a browser":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := call(h, "POST", path, "", header...); rec.Code != http.StatusAccepted {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
		})
	}
	// Reading stays open: GET is safe, and without CORS headers a foreign
	// page cannot read the answer anyway.
	svc.mission = func(domain.MissionID) (MissionView, error) { return MissionView{}, nil }
	if rec := call(h, "GET", "/api/v1/missions/MSN-042", "", "Sec-Fetch-Site", "cross-site"); rec.Code != http.StatusOK {
		t.Fatalf("GET refused cross-site: %d", rec.Code)
	}
}

func TestNewHandlerRefusesMalformedOrigin(t *testing.T) {
	hub, err := NewHub(HubOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hub.Close() }()
	if _, err := NewHandler(&fakeService{}, hub, Options{TrustedOrigins: []string{"localhost:5173"}}); err == nil {
		t.Fatal("an origin without a scheme was accepted")
	}
}
