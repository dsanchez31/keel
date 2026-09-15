package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dsanchez31/keel/adapters/native"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/httpjson"
	"github.com/dsanchez31/keel/internal/pacer"
	"github.com/dsanchez31/keel/internal/world"
	"github.com/dsanchez31/keel/vector"
)

// serveWait bounds every wait on the network.
const serveWait = 5 * time.Second

var testCaps = domain.Capabilities{ID: "DRONE-T", Domain: domain.DomainAerial, Tags: []string{"gps"}, CruiseSpeed: 15, MaxRangeM: 30000, SensorRadiusM: 60}

// newIdleServe builds a one-vehicle world with a lossless radio behind a
// native server, ten times faster than real time, its loop not running.
func newIdleServe(t *testing.T) (*Serve, *native.Server) {
	t.Helper()
	sim, err := world.New(world.Config{
		Seed:    1,
		Physics: world.Physics{ClimbRateMps: 5},
		Fleet: []world.Vehicle{{
			Caps:  testCaps,
			State: domain.VectorState{ID: testCaps.ID, Position: domain.Position{Lat: 45.02, Lon: 5.02}, BatteryPct: 100, Link: domain.LinkOK, Mode: domain.ModeIdle},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := native.NewServer(native.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	v, err := srv.Add(testCaps, vector.CommandTypes())
	if err != nil {
		t.Fatal(err)
	}
	p, err := pacer.NewRealTime(10)
	if err != nil {
		t.Fatal(err)
	}
	return &Serve{World: sim, Vehicles: []*native.Vehicle{v}, Pacer: p}, srv
}

// newServe serves newIdleServe's world, the loop running, until the test
// ends.
func newServe(t *testing.T, edit func(*Serve)) *httptest.Server {
	t.Helper()
	sv, srv := newIdleServe(t)
	if edit != nil {
		edit(sv)
	}
	ts := httptest.NewServer(sv.Handler(srv))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		// The native server first: its connections are requests the test
		// server would otherwise wait on.
		_ = srv.Close()
		ts.Close()
	})
	return ts
}

func dialServed(t *testing.T, base string, id domain.VectorID, opts native.Options) *native.Adapter {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), serveWait)
	defer cancel()
	url := "ws" + strings.TrimPrefix(base, "http") + "/v1/vectors/" + string(id)
	a, err := native.Dial(ctx, url, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func frameUntil(t *testing.T, a *native.Adapter, what string, cond func(vector.VectorState) bool) vector.VectorState {
	t.Helper()
	deadline := time.After(serveWait)
	for {
		select {
		case s, ok := <-a.Telemetry():
			if !ok {
				t.Fatalf("telemetry closed waiting for %s", what)
			}
			if cond(s) {
				return s
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(serveWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// syncBuffer is a bytes.Buffer safe to read while the command writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestServeDrivesTheWorldOverNative(t *testing.T) {
	ts := newServe(t, nil)
	a := dialServed(t, ts.URL, testCaps.ID, native.Options{})
	if got := a.Describe(); got.ID != testCaps.ID || got.CruiseSpeed != testCaps.CruiseSpeed {
		t.Fatalf("hello declares %+v", got)
	}
	if got := a.Supports(); !slices.Equal(got, vector.CommandTypes()) {
		t.Fatalf("hello supports %v, want every command type", got)
	}
	s := frameUntil(t, a, "a frame", func(vector.VectorState) bool { return true })
	if s.LastSeenMs != 0 {
		t.Errorf("frame carries mission time %d ms, want 0 for the engine to stamp", s.LastSeenMs)
	}
	if s.Mode != vector.ModeIdle || s.AckSeq != 0 {
		t.Errorf("first frame mode %q, ack %d", s.Mode, s.AckSeq)
	}

	wp := vector.Position{Lat: 45.03, Lon: 5.02, AltM: 120}
	if err := a.Execute(vector.Command{Vector: testCaps.ID, Seq: 1, Type: vector.CommandGoto, Waypoint: &wp}); err != nil {
		t.Fatal(err)
	}
	frameUntil(t, a, "the world flying the goto", func(s vector.VectorState) bool {
		return s.AckSeq == 1 && s.Mode == vector.ModeTransit && s.Speed > 0
	})
}

func TestServeAppliesTimedFaults(t *testing.T) {
	injected := make(chan domain.Fault, 1)
	ts := newServe(t, func(s *Serve) {
		s.Faults = []world.ScheduledFault{{Trigger: world.TriggerTime, AtMs: 3000, Fault: domain.Fault{Kind: domain.FaultKill, Vector: testCaps.ID}}}
		s.OnFault = func(_ int64, f domain.Fault) { injected <- f }
	})
	// The idle timeout outlasts the wait, so Health ages the silence rather
	// than reporting the redial it would otherwise trigger.
	a := dialServed(t, ts.URL, testCaps.ID, native.Options{
		Agent:       vector.Options{DegradedAfter: 100 * time.Millisecond, LostAfter: 300 * time.Millisecond},
		IdleTimeout: time.Minute,
	})
	frameUntil(t, a, "a frame", func(vector.VectorState) bool { return true })
	select {
	case f := <-injected:
		if f.Kind != domain.FaultKill || f.Vector != testCaps.ID {
			t.Errorf("injected %+v", f)
		}
	case <-time.After(serveWait):
		t.Fatal("the kill was never injected")
	}
	waitFor(t, "the killed vehicle to fall silent", func() bool {
		h := a.Health()
		return h.Kind == vector.HealthLost && strings.Contains(h.Detail, "no frame for")
	})
}

// postFault posts a body to FaultPath as application/json, headers given as
// name, value pairs overriding it.
func postFault(h http.Handler, body string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, FaultPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func problemOf(t *testing.T, rec *httptest.ResponseRecorder, status int) httpjson.Problem {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content type %q, want application/problem+json", ct)
	}
	var p httpjson.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem body %s: %v", rec.Body, err)
	}
	return p
}

func TestServeInjectsPostedFaults(t *testing.T) {
	injected := make(chan domain.Fault, 1)
	ts := newServe(t, func(s *Serve) {
		s.OnFault = func(_ int64, f domain.Fault) { injected <- f }
	})
	a := dialServed(t, ts.URL, testCaps.ID, native.Options{
		Agent:       vector.Options{DegradedAfter: 100 * time.Millisecond, LostAfter: 300 * time.Millisecond},
		IdleTimeout: time.Minute,
	})
	frameUntil(t, a, "a frame", func(vector.VectorState) bool { return true })

	resp, err := http.Post(ts.URL+FaultPath, "application/json", strings.NewReader(`{"vector":"DRONE-T","kind":"kill"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
	select {
	case f := <-injected:
		if f != (domain.Fault{Kind: domain.FaultKill, Vector: testCaps.ID}) {
			t.Errorf("injected %+v", f)
		}
	case <-time.After(serveWait):
		t.Fatal("the posted kill was never injected")
	}
	waitFor(t, "the killed vehicle to fall silent", func() bool {
		h := a.Health()
		return h.Kind == vector.HealthLost && strings.Contains(h.Detail, "no frame for")
	})
}

func TestServeQueuesFaultsInArrivalOrder(t *testing.T) {
	sv, srv := newIdleServe(t)
	h := sv.Handler(srv)
	for _, body := range []string{
		`{"vector":"DRONE-T","kind":"link_loss","duration_ms":5000}`,
		`{"vector":"DRONE-T","kind":"battery_drain","magnitude":30}`,
	} {
		if rec := postFault(h, body); rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
			t.Fatalf("%s: status %d, body %q", body, rec.Code, rec.Body)
		}
	}
	want := []domain.Fault{
		{Kind: domain.FaultLinkLoss, Vector: testCaps.ID, DurationMs: 5000},
		{Kind: domain.FaultBatteryDrain, Vector: testCaps.ID, Magnitude: 30},
	}
	if got := sv.takePosted(); !slices.Equal(got, want) {
		t.Fatalf("queued %+v, want %+v", got, want)
	}
	if got := sv.takePosted(); len(got) != 0 {
		t.Fatalf("a tick's faults were handed out twice: %+v", got)
	}
}

func TestServeRefusesFaults(t *testing.T) {
	sv, srv := newIdleServe(t)
	h := sv.Handler(srv)
	const kill = `{"vector":"DRONE-T","kind":"kill"}`
	cases := []struct {
		name, body string
		header     []string
		status     int
		detail     string
	}{
		{"unknown vector", `{"vector":"DRONE-X","kind":"kill"}`, nil, http.StatusUnprocessableEntity, "DRONE-X is not in the fleet"},
		{"no vector", `{"kind":"kill"}`, nil, http.StatusUnprocessableEntity, "vector is required"},
		{"drain without magnitude", `{"vector":"DRONE-T","kind":"battery_drain"}`, nil, http.StatusUnprocessableEntity, "finite positive magnitude"},
		{"part of a tick", `{"vector":"DRONE-T","kind":"link_loss","duration_ms":50}`, nil, http.StatusUnprocessableEntity, "whole number of ticks"},
		{"a scenario trigger", `{"vector":"DRONE-T","kind":"kill","at_ms":100}`, nil, http.StatusBadRequest, "body.at_ms: unknown field"},
		{"no kind", `{"vector":"DRONE-T"}`, nil, http.StatusBadRequest, "body.kind: missing"},
		{"not JSON", kill, []string{"Content-Type", "text/plain"}, http.StatusUnsupportedMediaType, "application/json"},
		{"cross-origin", kill, []string{"Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example"}, http.StatusForbidden, "cross-origin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if p := problemOf(t, postFault(h, tc.body, tc.header...), tc.status); !strings.Contains(p.Detail, tc.detail) {
				t.Fatalf("detail %q, want it to mention %q", p.Detail, tc.detail)
			}
		})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, FaultPath, nil))
	problemOf(t, rec, http.StatusMethodNotAllowed)
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow %q, want POST", allow)
	}
	if got := sv.takePosted(); len(got) != 0 {
		t.Fatalf("refused faults were queued: %+v", got)
	}
}

// putClock puts a body to ClockPath as application/json.
func putClock(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, ClockPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServeTakesItsPace(t *testing.T) {
	sv, srv := newIdleServe(t)
	h := sv.Handler(srv)
	rec := putClock(h, `{"speed":20}`)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"speed":20}` {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if got := sv.Pacer.(pacer.Scalable).Speed(); got != 20 {
		t.Fatalf("pacer at %v, want 20", got)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ClockPath, nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"speed":20}` {
		t.Fatalf("GET: status %d, body %s", rec.Code, rec.Body)
	}

	for body, status := range map[string]int{
		`{"speed":0}`:       http.StatusUnprocessableEntity,
		`{"speed":21}`:      http.StatusUnprocessableEntity,
		`{"speed":2.5}`:     http.StatusBadRequest,
		`{"speed":2,"x":1}`: http.StatusBadRequest,
	} {
		problemOf(t, putClock(h, body), status)
	}
	if got := sv.Pacer.(pacer.Scalable).Speed(); got != 20 {
		t.Fatalf("a refused pace changed the pacer to %v", got)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, ClockPath, strings.NewReader(`{"speed":2}`)))
	problemOf(t, rec, http.StatusMethodNotAllowed)
	if allow := rec.Header().Get("Allow"); allow != "GET, PUT" {
		t.Errorf("Allow %q, want GET, PUT", allow)
	}

	sv.Pacer = pacer.Fast{}
	if p := problemOf(t, putClock(h, `{"speed":2}`), http.StatusConflict); !strings.Contains(p.Detail, "not paced in real time") {
		t.Fatalf("detail %q", p.Detail)
	}
}

func TestServeBoundsPendingFaults(t *testing.T) {
	sv, srv := newIdleServe(t)
	h := sv.Handler(srv)
	const kill = `{"vector":"DRONE-T","kind":"kill"}`
	for i := range MaxPendingFaults {
		if rec := postFault(h, kill); rec.Code != http.StatusAccepted {
			t.Fatalf("fault %d: status %d: %s", i+1, rec.Code, rec.Body)
		}
	}
	rec := postFault(h, kill)
	problemOf(t, rec, http.StatusServiceUnavailable)
	if ra := rec.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After %q, want 1", ra)
	}
	if got := sv.takePosted(); len(got) != MaxPendingFaults {
		t.Fatalf("%d faults queued, want %d", len(got), MaxPendingFaults)
	}
	if rec := postFault(h, kill); rec.Code != http.StatusAccepted {
		t.Fatalf("a drained queue still refuses: status %d", rec.Code)
	}
}

func TestLoadServeSkipsCoverageFaults(t *testing.T) {
	sv, srv, skipped, err := loadServe(referenceScenario)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	var ids []domain.VectorID
	for _, v := range sv.Vehicles {
		ids = append(ids, v.ID())
	}
	want := []domain.VectorID{"DRONE-01", "DRONE-02", "DRONE-03", "DRONE-04", "RELAY-01", "UGV-01"}
	if !slices.Equal(ids, want) {
		t.Errorf("served %v, want the reference fleet %v", ids, want)
	}
	if len(sv.Faults) != 0 {
		t.Errorf("time-triggered faults %+v, the reference scenario has none", sv.Faults)
	}
	if len(skipped) != 1 || skipped[0].Trigger != world.TriggerCoverage || skipped[0].Fault.Vector != "DRONE-02" {
		t.Errorf("skipped %+v, want the link loss of DRONE-02 at 60 %% coverage", skipped)
	}
}

func TestServeRefusesTheClosedLoopOptions(t *testing.T) {
	cases := map[string][]string{
		"--out":       {"--out", t.TempDir()},
		"--max-ticks": {"--max-ticks", "10"},
		"--speed":     {"--speed", "2"},
	}
	for flag, extra := range cases {
		var stderr bytes.Buffer
		args := append([]string{"--serve", "--scenario", referenceScenario}, extra...)
		if code := run(args, io.Discard, &stderr); code != exitError {
			t.Errorf("%s: exit %d, want %d", flag, code, exitError)
		}
		if !strings.Contains(stderr.String(), flag) {
			t.Errorf("%s: stderr %q does not name the flag", flag, stderr.String())
		}
	}
}

func TestServeCommandServesUntilInterrupted(t *testing.T) {
	var out syncBuffer
	cmd := newRootCmd(&out, io.Discard)
	cmd.SetArgs([]string{"--serve", "--listen", "127.0.0.1:0", "--scenario", referenceScenario})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	addr := regexp.MustCompile(`serving 6 vectors on ws://(\S+)/v1/vectors/\{id\}`)
	waitFor(t, "the serving line", func() bool { return addr.MatchString(out.String()) })
	base := "http://" + addr.FindStringSubmatch(out.String())[1]
	if want := "taking faults on POST " + base + FaultPath; !strings.Contains(out.String(), want) {
		t.Errorf("no %q line in %q", want, out.String())
	}
	if want := "skipped: link_loss on DRONE-02 at 60 % coverage, coverage is the engine's to observe: post it to " + base + FaultPath; !strings.Contains(out.String(), want) {
		t.Errorf("no notice of the skipped coverage fault in %q", out.String())
	}
	a := dialServed(t, base, "DRONE-01", native.Options{})
	if s := frameUntil(t, a, "a frame of DRONE-01", func(vector.VectorState) bool { return true }); s.ID != "DRONE-01" {
		t.Errorf("frame of %s", s.ID)
	}
	_ = a.Close()

	// The demo's loss of DRONE-02, posted rather than triggered by coverage.
	resp, err := http.Post(base+FaultPath, "application/json", strings.NewReader(`{"vector":"DRONE-02","kind":"link_loss"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("posting a fault: status %d", resp.StatusCode)
	}
	waitFor(t, "the posted fault's line", func() bool { return strings.Contains(out.String(), "fault  link_loss on DRONE-02") })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("interrupted --serve returned %v, want nil", err)
		}
	case <-time.After(serveWait):
		t.Fatal("--serve did not stop")
	}
	if !strings.Contains(out.String(), "stopped after") {
		t.Errorf("no stop line in %q", out.String())
	}
}
