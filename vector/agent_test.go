package vector_test

import (
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dsanchez31/keel/vector"
)

func caps() vector.Capabilities {
	return vector.Capabilities{
		ID:            "DRONE-01",
		Domain:        vector.DomainAerial,
		Tags:          []string{"gps", "camera", "gps"},
		CruiseSpeed:   15,
		MaxRangeM:     30000,
		SensorRadiusM: 60,
	}
}

// fakeClock is a wall clock the test moves by hand.
type fakeClock struct{ now time.Time }

func (c *fakeClock) read() time.Time         { return c.now }
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// recorder is a Transmit that keeps what it was handed.
type recorder struct {
	sent []uint64
	fail error
}

func (r *recorder) transmit(c vector.Command) error {
	if r.fail != nil {
		return r.fail
	}
	r.sent = append(r.sent, c.Seq)
	return nil
}

func newAgent(t *testing.T, opts vector.Options) (*vector.Agent, *recorder, *fakeClock) {
	t.Helper()
	rec := &recorder{}
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	if opts.Clock == nil {
		opts.Clock = clk.read
	}
	a, err := vector.NewAgent(caps(), rec.transmit, opts)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, rec, clk
}

func frame(ms int64) vector.VectorState {
	return vector.VectorState{
		ID:         "DRONE-01",
		Position:   vector.Position{Lat: 45.05, Lon: 5.06, AltM: 120},
		Heading:    90,
		Speed:      15,
		BatteryPct: 80,
		Link:       vector.LinkOK,
		Mode:       vector.ModeTransit,
		LastSeenMs: ms,
	}
}

func gotoCmd(seq uint64) vector.Command {
	return vector.Command{
		Vector:   "DRONE-01",
		Seq:      seq,
		Type:     vector.CommandGoto,
		Waypoint: &vector.Position{Lat: 45.06, Lon: 5.07, AltM: 120},
		Lane:     "L0",
	}
}

func TestNewAgentSortsTags(t *testing.T) {
	a, _, _ := newAgent(t, vector.Options{})
	got := a.Describe()
	if !slices.Equal(got.Tags, []string{"camera", "gps"}) {
		t.Fatalf("tags %v, want [camera gps]", got.Tags)
	}
	got.Tags[0] = "radio_mesh"
	if a.Describe().Tags[0] != "camera" {
		t.Fatal("editing the described tags edited the agent's set")
	}
	if !slices.Equal(a.Supports(), vector.CommandTypes()) {
		t.Fatalf("supports %v by default, want every command type", a.Supports())
	}
}

func TestNewAgentRefusesWhatCannotBeConformant(t *testing.T) {
	nop := func(vector.Command) error { return nil }
	cases := []struct {
		name     string
		edit     func(*vector.Capabilities)
		transmit vector.Transmit
		opts     vector.Options
	}{
		{name: "no id", edit: func(c *vector.Capabilities) { c.ID = "" }},
		{name: "maritime", edit: func(c *vector.Capabilities) { c.Domain = "maritime" }},
		{name: "zero speed", edit: func(c *vector.Capabilities) { c.CruiseSpeed = 0 }},
		{name: "infinite range", edit: func(c *vector.Capabilities) { c.MaxRangeM = math.Inf(1) }},
		{name: "NaN sensor", edit: func(c *vector.Capabilities) { c.SensorRadiusM = math.NaN() }},
		{name: "nil transmit"},
		{name: "unknown type", opts: vector.Options{Supports: []vector.CommandType{"orbit"}}},
		{name: "supports nothing", opts: vector.Options{Supports: []vector.CommandType{}}},
		{name: "lost before degraded", opts: vector.Options{DegradedAfter: 10 * time.Second, LostAfter: 5 * time.Second}},
		{name: "negative buffer", opts: vector.Options{Buffer: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := caps()
			if tc.edit != nil {
				tc.edit(&c)
			}
			tr := tc.transmit
			if tr == nil && tc.name != "nil transmit" {
				tr = nop
			}
			if a, err := vector.NewAgent(c, tr, tc.opts); err == nil {
				_ = a.Close()
				t.Fatal("accepted")
			}
		})
	}
}

// The engine re-sends a standing command with its Seq because the first copy
// may have been lost: the agent forwards it until a frame acknowledges it.
func TestExecuteFiltersOnAcknowledgement(t *testing.T) {
	a, rec, _ := newAgent(t, vector.Options{})
	for _, seq := range []uint64{1, 1, 3, 2, 3} {
		if err := a.Execute(gotoCmd(seq)); err != nil {
			t.Fatalf("seq %d: %v", seq, err)
		}
	}
	if want := []uint64{1, 1, 3, 3}; !slices.Equal(rec.sent, want) {
		t.Fatalf("transmitted %v, want %v: new, re-sent, gap, superseded dropped, re-sent", rec.sent, want)
	}
	ack := func(ms int64, seq uint64) {
		t.Helper()
		f := frame(ms)
		f.AckSeq = seq
		if err := a.Publish(f); err != nil {
			t.Fatal(err)
		}
	}
	ack(100, 3)
	for _, seq := range []uint64{3, 4} {
		if err := a.Execute(gotoCmd(seq)); err != nil {
			t.Fatalf("seq %d: %v", seq, err)
		}
	}
	if want := []uint64{1, 1, 3, 3, 4}; !slices.Equal(rec.sent, want) {
		t.Fatalf("transmitted %v, want %v: an acknowledged seq is a no-op", rec.sent, want)
	}
	// A vehicle that restarts has applied nothing: the re-send of its
	// standing command must reach it.
	ack(200, 0)
	if err := a.Execute(gotoCmd(4)); err != nil {
		t.Fatal(err)
	}
	if want := []uint64{1, 1, 3, 3, 4, 4}; !slices.Equal(rec.sent, want) {
		t.Fatalf("transmitted %v, want %v: the acknowledgement follows the vehicle down", rec.sent, want)
	}
}

func TestExecuteRefusesUnsupportedAndMalformed(t *testing.T) {
	a, rec, _ := newAgent(t, vector.Options{Supports: []vector.CommandType{vector.CommandHold, vector.CommandGoto}})
	nan := vector.Position{Lat: math.NaN()}
	cases := []struct {
		name string
		cmd  vector.Command
		want error
	}{
		{"undeclared", vector.Command{Vector: "DRONE-01", Seq: 1, Type: vector.CommandRTB}, vector.ErrUnsupported},
		{"unknown", vector.Command{Vector: "DRONE-01", Seq: 1, Type: "orbit"}, vector.ErrUnsupported},
		{"other vector", vector.Command{Vector: "DRONE-02", Seq: 1, Type: vector.CommandHold}, vector.ErrInvalidCommand},
		{"seq 0", vector.Command{Vector: "DRONE-01", Type: vector.CommandHold}, vector.ErrInvalidCommand},
		{"goto without waypoint", vector.Command{Vector: "DRONE-01", Seq: 1, Type: vector.CommandGoto}, vector.ErrInvalidCommand},
		{"waypoint not finite", vector.Command{Vector: "DRONE-01", Seq: 1, Type: vector.CommandGoto, Waypoint: &nan}, vector.ErrInvalidCommand},
	}
	for _, tc := range cases {
		if err := a.Execute(tc.cmd); !errors.Is(err, tc.want) {
			t.Errorf("%s: error %v, want %v", tc.name, err, tc.want)
		}
	}
	if len(rec.sent) != 0 {
		t.Fatalf("transmitted %v, want nothing", rec.sent)
	}
}

func TestExecuteReportsTransmitFailure(t *testing.T) {
	a, rec, _ := newAgent(t, vector.Options{})
	boom := errors.New("queue full")
	rec.fail = boom
	if err := a.Execute(gotoCmd(5)); !errors.Is(err, boom) {
		t.Fatalf("error %v, want the transport's", err)
	}
	rec.fail = nil
	if err := a.Execute(gotoCmd(4)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.sent, []uint64{4}) {
		t.Fatalf("transmitted %v: a seq that failed to leave supersedes nothing", rec.sent)
	}
}

func TestHealthAgesFromTheLastFrame(t *testing.T) {
	a, _, clk := newAgent(t, vector.Options{})
	if h := a.Health(); h.Kind != vector.HealthLost || !strings.Contains(h.Detail, "no frame yet") {
		t.Fatalf("before any frame: %+v, want lost", h)
	}
	if err := a.Publish(frame(1200)); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		after time.Duration
		want  vector.HealthKind
	}{
		{0, vector.HealthOK},
		{4900 * time.Millisecond, vector.HealthOK},
		{100 * time.Millisecond, vector.HealthDegraded},
		{4900 * time.Millisecond, vector.HealthDegraded},
		{100 * time.Millisecond, vector.HealthLost},
	}
	var elapsed time.Duration
	for _, s := range steps {
		clk.advance(s.after)
		elapsed += s.after
		if h := a.Health(); h.Kind != s.want || h.LastFrameMs != 1200 {
			t.Fatalf("after %v: %+v, want %s with the frame's mission time", elapsed, h, s.want)
		}
	}
	if err := a.Publish(frame(1300)); err != nil {
		t.Fatal(err)
	}
	if h := a.Health(); h.Kind != vector.HealthOK || h.Detail != "" {
		t.Fatalf("after a new frame: %+v, want ok", h)
	}
}

func TestDisconnectedIsLostUntilTheNextFrame(t *testing.T) {
	a, _, _ := newAgent(t, vector.Options{})
	if err := a.Publish(frame(100)); err != nil {
		t.Fatal(err)
	}
	a.Disconnected("socket reset")
	if h := a.Health(); h.Kind != vector.HealthLost || !strings.Contains(h.Detail, "socket reset") {
		t.Fatalf("after a disconnect: %+v, want lost with the reason", h)
	}
	if err := a.Publish(frame(200)); err != nil {
		t.Fatal(err)
	}
	if h := a.Health(); h.Kind != vector.HealthOK {
		t.Fatalf("after reconnecting: %+v, want ok", h)
	}
	<-a.Telemetry()
	if got := <-a.Telemetry(); got.LastSeenMs != 200 {
		t.Fatalf("frame at %d ms, want the post-reconnect frame on the same channel", got.LastSeenMs)
	}
}

func TestPublishRefusesNonConformantFrames(t *testing.T) {
	a, _, clk := newAgent(t, vector.Options{})
	if err := a.Publish(frame(1000)); err != nil {
		t.Fatal(err)
	}
	<-a.Telemetry()
	cases := []struct {
		name string
		edit func(*vector.VectorState)
	}{
		{"other vector", func(s *vector.VectorState) { s.ID = "DRONE-02" }},
		{"position", func(s *vector.VectorState) { s.Position.Lon = math.Inf(-1) }},
		{"heading", func(s *vector.VectorState) { s.Heading = math.NaN() }},
		{"negative speed", func(s *vector.VectorState) { s.Speed = -1 }},
		{"battery", func(s *vector.VectorState) { s.BatteryPct = 101 }},
		{"scanning", func(s *vector.VectorState) { s.Mode = vector.ModeScanning }},
		{"down", func(s *vector.VectorState) { s.Mode = vector.ModeDown }},
		{"time backwards", func(s *vector.VectorState) { s.LastSeenMs = 900 }},
	}
	clk.advance(6 * time.Second)
	for _, tc := range cases {
		s := frame(1100)
		tc.edit(&s)
		if err := a.Publish(s); !errors.Is(err, vector.ErrInvalidFrame) {
			t.Errorf("%s: error %v, want ErrInvalidFrame", tc.name, err)
		}
	}
	if h := a.Health(); h.Kind != vector.HealthDegraded {
		t.Fatalf("health %+v: a refused frame refreshed it", h)
	}
	select {
	case s := <-a.Telemetry():
		t.Fatalf("a refused frame was delivered: %+v", s)
	default:
	}
	if err := a.Publish(frame(1000)); err != nil {
		t.Fatalf("a repeated timestamp is monotonic: %v", err)
	}
}

// Telemetry is state: a slow reader gets the newest frames, never the oldest.
func TestTelemetryLatestWins(t *testing.T) {
	a, _, _ := newAgent(t, vector.Options{Buffer: 2})
	for ms := int64(100); ms <= 500; ms += 100 {
		if err := a.Publish(frame(ms)); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []int64{400, 500} {
		if got := <-a.Telemetry(); got.LastSeenMs != want {
			t.Fatalf("frame at %d ms, want %d", got.LastSeenMs, want)
		}
	}
	if h := a.Health(); !strings.Contains(h.Detail, "3 telemetry frames dropped") {
		t.Fatalf("health detail %q, want the drop count", h.Detail)
	}
}

func TestCloseEndsTheAgent(t *testing.T) {
	a, _, _ := newAgent(t, vector.Options{})
	if err := a.Publish(frame(100)); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	var got []int64
	for s := range a.Telemetry() {
		got = append(got, s.LastSeenMs)
	}
	if !slices.Equal(got, []int64{100}) {
		t.Fatalf("drained %v, want the buffered frame then a closed channel", got)
	}
	if err := a.Execute(gotoCmd(1)); !errors.Is(err, vector.ErrClosed) {
		t.Fatalf("Execute after Close: %v", err)
	}
	if err := a.Publish(frame(200)); !errors.Is(err, vector.ErrClosed) {
		t.Fatalf("Publish after Close: %v", err)
	}
	a.Disconnected("late")
	if h := a.Health(); h.Kind != vector.HealthLost || h.Detail != "closed" {
		t.Fatalf("health after Close: %+v", h)
	}
}

// Run under -race: the transport's reader, the daemon's dispatcher and its
// health poller all touch one agent at once.
func TestConcurrentUse(t *testing.T) {
	var mu sync.Mutex
	sent := 0
	a, err := vector.NewAgent(caps(), func(vector.Command) error {
		mu.Lock()
		sent++
		mu.Unlock()
		return nil
	}, vector.Options{Buffer: 4})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for ms := range int64(1000) {
			if err := a.Publish(frame(ms)); err != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for seq := uint64(1); seq <= 1000; seq++ {
			_ = a.Execute(gotoCmd(seq))
		}
	}()
	go func() {
		defer wg.Done()
		for range 1000 {
			_ = a.Health()
		}
	}()
	done := make(chan struct{})
	go func() {
		for range a.Telemetry() {
		}
		close(done)
	}()
	wg.Wait()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	if sent != 1000 {
		t.Fatalf("transmitted %d commands, want 1000", sent)
	}
}
