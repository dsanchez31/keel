package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v4"
	"github.com/coder/websocket"

	"github.com/dsanchez31/keel/adapters/mavlink/mavlinktest"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/files"
	"github.com/dsanchez31/keel/internal/httpjson"
	"github.com/dsanchez31/keel/internal/pacer"
	"github.com/dsanchez31/keel/internal/transport"
)

// clockEndpoint is a fake keelsim clock endpoint: it records each pace put,
// with the daemon's pace when it arrived, and refuses the speed it is told
// to.
type clockEndpoint struct {
	mu     sync.Mutex
	pace   pacer.Scalable
	refuse int
	puts   []int
	seen   []float64
}

func (c *clockEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var s pacer.Setting
	if r.Method != http.MethodPut || json.NewDecoder(r.Body).Decode(&s) != nil {
		httpjson.WriteProblem(w, http.StatusBadRequest, "not a pace")
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts = append(c.puts, s.Speed)
	if c.pace != nil {
		c.seen = append(c.seen, c.pace.Speed())
	}
	if s.Speed == c.refuse {
		httpjson.WriteProblem(w, http.StatusInternalServerError, "the world stalled")
		return
	}
	httpjson.WriteJSON(w, http.StatusOK, s)
}

func (c *clockEndpoint) received() (puts []int, seen []float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.puts), slices.Clone(c.seen)
}

func (c *clockEndpoint) failOn(speed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refuse = speed
}

// newClockDaemon is a daemon over the bindings given, not running, paced by p.
func newClockDaemon(t *testing.T, cfg *Config, p pacer.Pacer) *Daemon {
	t.Helper()
	w := readReferenceWorld(t)
	packs, err := files.ReadPacks(shippedPacks)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Name, cfg.DataDir = "clock", t.TempDir()
	hub, err := transport.NewHub(transport.HubOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	d, err := New(Options{Config: cfg, World: w, Packs: packs, Fleet: newTestFleet(t, cfg, w), Hub: hub, Pacer: p})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// simulated binds a vector to nothing that answers, paced by a clock
// endpoint.
func simulated(id, clock string) Binding {
	return Binding{ID: domain.VectorID(id), Native: &NativeBinding{URL: "ws://127.0.0.1:1/v1/vectors/" + id, Clock: clock}}
}

func realTime(t *testing.T) *pacer.RealTime {
	t.Helper()
	p, err := pacer.NewRealTime(1)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Raising the pace, every simulator takes it before the daemon; lowering it,
// the daemon goes first. A clock endpoint shared by several vectors is put
// once.
func TestSetClockPacesTheWholeLoop(t *testing.T) {
	p := realTime(t)
	a, b := &clockEndpoint{pace: p}, &clockEndpoint{pace: p}
	sa, sb := httptest.NewServer(a), httptest.NewServer(b)
	t.Cleanup(sa.Close)
	t.Cleanup(sb.Close)
	d := newClockDaemon(t, &Config{Vectors: []Binding{
		simulated("DRONE-02", sa.URL), simulated("DRONE-03", sa.URL), simulated("DRONE-04", sb.URL),
	}}, p)

	if got, _ := d.Clock(context.Background()); got != (transport.ClockView{Speed: 1, MaxSpeed: pacer.MaxLiveSpeed}) {
		t.Fatalf("clock %+v", got)
	}
	got, err := d.SetClock(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if got.Speed != 20 || p.Speed() != 20 {
		t.Fatalf("clock %+v, pacer at %v", got, p.Speed())
	}
	if _, err := d.SetClock(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*clockEndpoint{a, b} {
		puts, seen := c.received()
		if !slices.Equal(puts, []int{20, 2}) {
			t.Errorf("paces put %v, want [20 2], once per endpoint", puts)
		}
		// Raised to 20 while the daemon was at 1, lowered to 2 once the
		// daemon was there already.
		if !slices.Equal(seen, []float64{1, 2}) {
			t.Errorf("daemon's pace when each arrived %v, want [1 2]", seen)
		}
	}
	// The pace in force changes nothing.
	if _, err := d.SetClock(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if puts, _ := a.received(); len(puts) != 2 {
		t.Errorf("the pace in force was put again: %v", puts)
	}
}

// A simulator refusing the pace leaves the whole loop where it was.
func TestSetClockRollsBack(t *testing.T) {
	p := realTime(t)
	a, b := &clockEndpoint{}, &clockEndpoint{}
	sa, sb := httptest.NewServer(a), httptest.NewServer(b)
	t.Cleanup(sa.Close)
	t.Cleanup(sb.Close)
	d := newClockDaemon(t, &Config{Vectors: []Binding{simulated("DRONE-02", sa.URL), simulated("DRONE-03", sb.URL)}}, p)

	b.failOn(5)
	if _, err := d.SetClock(context.Background(), 5); !errors.Is(err, transport.ErrUpstream) || !strings.Contains(err.Error(), "the world stalled") {
		t.Fatalf("raising against a refusal: %v", err)
	}
	if p.Speed() != 1 {
		t.Fatalf("pacer at %v after a refused raise", p.Speed())
	}
	for _, c := range []*clockEndpoint{a, b} {
		if puts, _ := c.received(); !slices.Equal(puts, []int{5, 1}) {
			t.Errorf("paces put %v, want [5 1], the refusing one brought back too", puts)
		}
	}

	b.failOn(0)
	if _, err := d.SetClock(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	b.failOn(2)
	if _, err := d.SetClock(context.Background(), 2); !errors.Is(err, transport.ErrUpstream) {
		t.Fatalf("lowering against a refusal: %v", err)
	}
	if p.Speed() != 10 {
		t.Fatalf("pacer at %v after a refused lowering, want back at 10", p.Speed())
	}
}

// A fleet holding a vector no simulator paces, or a loop not paced in real
// time, stays in real time, and says why.
func TestSetClockRefusals(t *testing.T) {
	sa := httptest.NewServer(&clockEndpoint{})
	t.Cleanup(sa.Close)
	d := newClockDaemon(t, &Config{Vectors: []Binding{
		simulated("DRONE-02", sa.URL),
		{ID: "DRONE-03", Native: &NativeBinding{URL: "ws://127.0.0.1:1/v1/vectors/DRONE-03"}},
	}}, realTime(t))
	c, _ := d.Clock(context.Background())
	if !strings.Contains(c.Fixed, "DRONE-03 is not simulated") {
		t.Fatalf("clock %+v, want it fixed by DRONE-03", c)
	}
	if _, err := d.SetClock(context.Background(), 2); !errors.Is(err, transport.ErrConflict) || !strings.Contains(err.Error(), "DRONE-03") {
		t.Fatalf("a pace with a real vehicle bound: %v", err)
	}
	if _, err := d.SetClock(context.Background(), 1); err != nil {
		t.Fatalf("real time with a real vehicle bound: %v", err)
	}

	fast := newClockDaemon(t, &Config{Vectors: []Binding{simulated("DRONE-02", sa.URL)}}, pacer.Fast{})
	if c, _ := fast.Clock(context.Background()); c.Speed != 1 || !strings.Contains(c.Fixed, "not paced in real time") {
		t.Fatalf("clock %+v", c)
	}
	if _, err := fast.SetClock(context.Background(), 2); !errors.Is(err, transport.ErrConflict) {
		t.Fatalf("a pace on a loop not in real time: %v", err)
	}
}

// A simulated vehicle heard again is brought to the fleet's pace: its
// simulator may have restarted in real time.
func TestFleetRepacesAReconnectedSimulator(t *testing.T) {
	w := readReferenceWorld(t)
	vs := newVehicles(t, w)
	c := &clockEndpoint{}
	sc := httptest.NewServer(c)
	t.Cleanup(sc.Close)
	f := newTestFleet(t, &Config{Vectors: []Binding{{ID: "DRONE-02", Native: &NativeBinding{URL: vs.url("DRONE-02"), Clock: sc.URL}}}}, w)
	if err := f.SetSimSpeed(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	v := vs.add(t, "DRONE-02")
	fv, _ := fleetVector(w, "DRONE-02")
	go func() {
		for range 200 {
			if v.Send(frame("DRONE-02", fv)) != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	drainUntil(t, f, "DRONE-02", "the link verdict ok", func(r Report) bool { return r.Link == domain.LinkOK })
	eventually(t, "the pace put again", func() bool { puts, _ := c.received(); return len(puts) == 2 && puts[1] == 20 })
}

// A SITL takes the pace through its adapter.
func TestFleetPacesASITL(t *testing.T) {
	w := readReferenceWorld(t)
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()
	f := newTestFleet(t, &Config{
		MAVLink: &MAVLinkConfig{Listen: addr},
		Vectors: []Binding{{ID: "DRONE-01", MAVLink: &MAVLinkBinding{SystemID: 1, SITL: true}}},
	}, w)
	ap, err := mavlinktest.New(mavlinktest.Options{SystemID: 1, SITL: true, Endpoint: &gomavlib.EndpointUDPClient{Address: addr}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ap.Close)
	drainUntil(t, f, "DRONE-01", "a frame of DRONE-01", func(r Report) bool { return r.Frame != nil })
	if err := f.SetSimSpeed(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if got, _ := ap.SimSpeed(); got != 10 {
		t.Fatalf("SIM_SPEEDUP %v, want 10", got)
	}
}

// Faster than real time, the stream carries the pace and telemetry and tick
// frames for one tick in speed.
func TestStreamIsThinnedByThePace(t *testing.T) {
	h := newHarness(t, t.TempDir()) // paced ten times faster than real time
	handler, err := transport.NewHandler(h.d, h.hub, transport.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	h.tickUntilFleet(t)

	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+transport.StreamPath, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	read := func() transport.Frame {
		t.Helper()
		_, b, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var f transport.Frame
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	var snap transport.MissionView
	if f := read(); f.Type != transport.FrameMission || json.Unmarshal(f.Data, &snap) != nil || snap.Speed != 10 {
		t.Fatalf("snapshot %s %s, want the pace in it", f.Type, f.Data)
	}

	for range 25 {
		if _, err := h.d.tick(); err != nil {
			t.Fatal(err)
		}
	}
	for ticks := 0; ticks < 2; {
		f := read()
		switch f.Type {
		case transport.FrameTick:
			var s transport.TickSummary
			if err := json.Unmarshal(f.Data, &s); err != nil {
				t.Fatal(err)
			}
			if s.Tick%10 != 0 || s.Speed != 10 {
				t.Fatalf("tick frame %s, want one tick in ten, the pace in it", f.Data)
			}
			ticks++
		case transport.FrameTelemetry:
			if f.T%1000 != 0 {
				t.Fatalf("telemetry frame at %d ms, want one tick in ten", f.T)
			}
		}
	}
}
