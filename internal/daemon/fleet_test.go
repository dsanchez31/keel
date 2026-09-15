package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v4"

	"github.com/dsanchez31/keel/adapters/mavlink/mavlinktest"
	"github.com/dsanchez31/keel/adapters/native"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/files"
	"github.com/dsanchez31/keel/internal/httpjson"
	"github.com/dsanchez31/keel/internal/planir"
	"github.com/dsanchez31/keel/vector"
)

// wait bounds every wait on the network.
const wait = 5 * time.Second

func readReferenceWorld(t *testing.T) *planir.World {
	t.Helper()
	w, err := files.ReadWorld(referenceWorld)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// vehicles serves native vehicles for the reference fleet entries named,
// until the test ends.
type vehicles struct {
	srv *native.Server
	ts  *httptest.Server
	w   *planir.World
}

func newVehicles(t *testing.T, w *planir.World) *vehicles {
	t.Helper()
	srv, err := native.NewServer(native.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		_ = srv.Close()
		ts.Close()
	})
	return &vehicles{srv: srv, ts: ts, w: w}
}

func (vs *vehicles) url(id domain.VectorID) string {
	return "ws" + strings.TrimPrefix(vs.ts.URL, "http") + "/v1/vectors/" + string(id)
}

func (vs *vehicles) add(t *testing.T, id domain.VectorID) *native.Vehicle {
	t.Helper()
	fv, ok := fleetVector(vs.w, id)
	if !ok {
		t.Fatalf("%s is not in the reference fleet", id)
	}
	v, err := vs.srv.Add(fv.Caps, vector.CommandTypes())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func newTestFleet(t *testing.T, cfg *Config, w *planir.World) *Fleet {
	t.Helper()
	f, err := NewFleet(cfg, w, FleetOptions{Agent: vector.Options{DegradedAfter: 200 * time.Millisecond, LostAfter: 500 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// drainUntil drains the fleet until cond holds for a report of id.
func drainUntil(t *testing.T, f *Fleet, id domain.VectorID, what string, cond func(Report) bool) Report {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		for _, r := range f.Drain() {
			if r.Vector == id && cond(r) {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
	return Report{}
}

func frame(id domain.VectorID, fv planir.FleetVector) vector.VectorState {
	s := fv.State
	s.ID = id
	s.LastSeenMs = 0
	return s
}

func TestFleetNativeVehicle(t *testing.T) {
	w := readReferenceWorld(t)
	vs := newVehicles(t, w)
	f := newTestFleet(t, &Config{Vectors: []Binding{{ID: "DRONE-02", Native: &NativeBinding{URL: vs.url("DRONE-02")}}}}, w)

	// Not registered yet: the vehicle answers 404, and the fleet redials.
	waitHealth(t, f, "DRONE-02", "the dial failure in Health", func(h domain.HealthStatus) bool {
		return h.Kind == domain.HealthLost && strings.Contains(h.Detail, "not connected yet:")
	})
	if got := f.Drain(); len(got) != 0 {
		t.Fatalf("a vector not connected is reported: %+v", got)
	}

	v := vs.add(t, "DRONE-02")
	fv, _ := fleetVector(w, "DRONE-02")
	sent := frame("DRONE-02", fv)
	go func() {
		for range 200 {
			if err := v.Send(sent); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	r := drainUntil(t, f, "DRONE-02", "a frame of DRONE-02", func(r Report) bool { return r.Frame != nil })
	if r.Caps.ID != "DRONE-02" || r.Caps.CruiseSpeed != fv.Caps.CruiseSpeed {
		t.Errorf("capabilities %+v, want the hello's", r.Caps)
	}
	if r.Frame.Position != sent.Position || r.Frame.BatteryPct != sent.BatteryPct {
		t.Errorf("frame %+v, want %+v", *r.Frame, sent)
	}
	drainUntil(t, f, "DRONE-02", "the link verdict ok", func(r Report) bool { return r.Link == domain.LinkOK })

	wp := domain.Position{Lat: 45.03, Lon: 5.03, AltM: 120}
	f.Dispatch([]domain.Command{{Vector: "DRONE-02", Seq: 1, Type: domain.CommandGoto, Waypoint: &wp}})
	select {
	case c := <-v.Commands():
		if c.Seq != 1 || c.Type != domain.CommandGoto || *c.Waypoint != wp {
			t.Errorf("vehicle received %+v", c)
		}
	case <-time.After(wait):
		t.Fatal("the command never reached the vehicle")
	}
}

func waitHealth(t *testing.T, f *Fleet, id domain.VectorID, what string, cond func(domain.HealthStatus) bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond(f.Health()[id]) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %+v", what, f.Health()[id])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A vehicle whose hello names another vector is not flown under the bound
// name.
func TestFleetRefusesAMisroutedVehicle(t *testing.T) {
	w := readReferenceWorld(t)
	vs := newVehicles(t, w)
	vs.add(t, "DRONE-03")
	f := newTestFleet(t, &Config{Vectors: []Binding{{ID: "DRONE-02", Native: &NativeBinding{URL: vs.url("DRONE-03")}}}}, w)
	waitHealth(t, f, "DRONE-02", "the refusal in Health", func(h domain.HealthStatus) bool {
		return strings.Contains(h.Detail, "its hello declares DRONE-03")
	})
}

func TestFleetMAVLinkVehicle(t *testing.T) {
	w := readReferenceWorld(t)
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()
	f := newTestFleet(t, &Config{
		MAVLink: &MAVLinkConfig{Listen: addr},
		Vectors: []Binding{{ID: "DRONE-01", MAVLink: &MAVLinkBinding{SystemID: 1}}},
	}, w)
	ap, err := mavlinktest.New(mavlinktest.Options{SystemID: 1, Endpoint: &gomavlib.EndpointUDPClient{Address: addr}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ap.Close)

	r := drainUntil(t, f, "DRONE-01", "a frame of DRONE-01", func(r Report) bool { return r.Frame != nil })
	fv, _ := fleetVector(w, "DRONE-01")
	if r.Caps.ID != "DRONE-01" || r.Caps.SensorRadiusM != fv.Caps.SensorRadiusM {
		t.Errorf("capabilities %+v, want the world's fleet entry", r.Caps)
	}
	if d := domain.HaversineM(r.Frame.Position, domain.Position{Lat: mavlinktest.PadLat, Lon: mavlinktest.PadLon}); d > 1 {
		t.Errorf("frame at %+v, %.1f m from the pad", r.Frame.Position, d)
	}
	if err := f.InjectFault(context.Background(), domain.Fault{Kind: domain.FaultKill, Vector: "DRONE-01"}); !errors.Is(err, ErrNotSimulated) {
		t.Errorf("a fault on an autopilot: err %v, want ErrNotSimulated", err)
	}
}

// simulator is a fake fault endpoint answering what it is told to.
type simulator struct {
	mu     sync.Mutex
	status int
	detail string
	bodies []string
}

func (s *simulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies = append(s.bodies, r.Header.Get("Content-Type")+" "+string(b))
	if s.status == http.StatusAccepted {
		w.WriteHeader(s.status)
		return
	}
	httpjson.WriteProblem(w, s.status, s.detail)
}

func (s *simulator) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func (s *simulator) answer(status int, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.detail = status, detail
}

func TestFleetForwardsFaults(t *testing.T) {
	w := readReferenceWorld(t)
	vs := newVehicles(t, w)
	sim := &simulator{status: http.StatusAccepted}
	ss := httptest.NewServer(sim)
	t.Cleanup(ss.Close)
	f := newTestFleet(t, &Config{Vectors: []Binding{
		{ID: "DRONE-02", Native: &NativeBinding{URL: vs.url("DRONE-02"), Faults: ss.URL + "/v1/faults"}},
		{ID: "DRONE-03", Native: &NativeBinding{URL: vs.url("DRONE-03")}},
	}}, w)
	ctx := context.Background()
	loss := domain.Fault{Kind: domain.FaultLinkLoss, Vector: "DRONE-02", DurationMs: 5000}

	if err := f.InjectFault(ctx, loss); err != nil {
		t.Fatal(err)
	}
	if got, want := sim.received(), `application/json {"duration_ms":5000,"kind":"link_loss","vector":"DRONE-02"}`; len(got) != 1 || got[0] != want {
		t.Fatalf("simulator received %q, want %q", got, want)
	}
	sim.answer(http.StatusUnprocessableEntity, "vector DRONE-02 is not in the fleet")
	if err := f.InjectFault(ctx, loss); !errors.Is(err, ErrFaultRefused) || !strings.Contains(err.Error(), "not in the fleet") {
		t.Errorf("a refusal: err %v, want ErrFaultRefused with the simulator's detail", err)
	}
	sim.answer(http.StatusServiceUnavailable, "64 faults already wait for the next tick")
	if err := f.InjectFault(ctx, loss); !errors.Is(err, ErrSimulator) || !strings.Contains(err.Error(), "64 faults") {
		t.Errorf("a failing simulator: err %v, want ErrSimulator", err)
	}
	ss.Close()
	if err := f.InjectFault(ctx, loss); !errors.Is(err, ErrSimulator) {
		t.Errorf("an unreachable simulator: err %v, want ErrSimulator", err)
	}
	if err := f.InjectFault(ctx, domain.Fault{Kind: domain.FaultKill, Vector: "DRONE-03"}); !errors.Is(err, ErrNotSimulated) {
		t.Errorf("a vehicle bound to no simulator: err %v, want ErrNotSimulated", err)
	}
	if err := f.InjectFault(ctx, domain.Fault{Kind: domain.FaultKill, Vector: "DRONE-09"}); !errors.Is(err, ErrUnbound) {
		t.Errorf("an unbound vector: err %v, want ErrUnbound", err)
	}
}
