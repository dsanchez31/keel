package conformance_test

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v4"

	"github.com/dsanchez31/keel/adapters/mavlink"
	"github.com/dsanchez31/keel/adapters/mavlink/mavlinktest"
	"github.com/dsanchez31/keel/adapters/native"
	"github.com/dsanchez31/keel/internal/conformance"
	"github.com/dsanchez31/keel/vector"
)

// The thresholds of the tests' adapters, scaled down from spec section 7.1's
// 5 s and 10 s so a run takes seconds, and the suite told to expect them.
const (
	degradedAfter = 200 * time.Millisecond
	lostAfter     = 500 * time.Millisecond
)

func fastSuite(motion bool) conformance.Options {
	return conformance.Options{
		AllowMotion:     motion,
		DegradedAfter:   degradedAfter,
		LostAfter:       lostAfter,
		Tolerance:       200 * time.Millisecond,
		Frames:          20,
		FramesWithin:    5 * time.Second,
		ApplyWithin:     5 * time.Second,
		ReconnectWithin: 5 * time.Second,
	}
}

func agentOptions() vector.Options {
	return vector.Options{DegradedAfter: degradedAfter, LostAfter: lostAfter}
}

var nativeCaps = vector.Capabilities{ID: "DRONE-C", Domain: vector.DomainAerial, Tags: []string{"camera", "gps"}, CruiseSpeed: 15, MaxRangeM: 30000, SensorRadiusM: 60}

// nativeVehicle serves one scripted vehicle over the native protocol until
// the test ends: frames every 20 ms, commands applied idempotently by Seq.
func nativeVehicle(t *testing.T) string {
	t.Helper()
	srv, err := native.NewServer(native.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v, err := srv.Add(nativeCaps, vector.CommandTypes())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		s := vector.VectorState{ID: nativeCaps.ID, Position: vector.Position{Lat: 45.02, Lon: 5.02, AltM: 0}, BatteryPct: 90, Link: vector.LinkOK, Mode: vector.ModeIdle}
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case c, ok := <-v.Commands():
				if !ok {
					return
				}
				if c.Seq <= s.AckSeq {
					continue
				}
				s.AckSeq = c.Seq
				switch c.Type {
				case vector.CommandGoto:
					s.Mode, s.Speed = vector.ModeTransit, 10
				case vector.CommandRTB:
					s.Mode, s.Speed = vector.ModeRTB, 10
				default:
					s.Mode, s.Speed = vector.ModeIdle, 0
				}
			case <-tick.C:
				s.LastSeenMs += 20
				_ = v.Send(s)
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
		_ = srv.Close()
		ts.Close()
	})
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/vectors/" + string(nativeCaps.ID)
}

func nativeSubject(t *testing.T) *conformance.Subject {
	t.Helper()
	s, err := conformance.NativeSubject(nativeVehicle(t), native.Options{
		Agent:      agentOptions(),
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func runSuite(t *testing.T, s *conformance.Subject, o conformance.Options) []conformance.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rs, err := conformance.Run(ctx, s, o)
	if err != nil {
		t.Fatal(err)
	}
	var cases []conformance.Case
	for _, r := range rs {
		cases = append(cases, r.Case)
		t.Logf("%s %-24s %-4s %s (%v)", r.Case, r.Name, r.Status, r.Detail, r.Elapsed)
	}
	want := []conformance.Case{conformance.C1, conformance.C2, conformance.C3, conformance.C4, conformance.C5, conformance.C8, conformance.C6, conformance.C7, conformance.C9}
	if !slices.Equal(cases, want) {
		t.Fatalf("cases %v, want %v", cases, want)
	}
	return rs
}

func requireAllPassed(t *testing.T, rs []conformance.Result) {
	t.Helper()
	for _, r := range rs {
		if r.Status != conformance.Pass {
			t.Errorf("%s %s: %s, %s", r.Case, r.Name, r.Status, r.Detail)
		}
	}
	if !conformance.Passed(rs) {
		t.Error("Passed reports a failure")
	}
}

// The phase 7 definition of done, for the native adapter.
func TestNativeAdapterPassesTheSuite(t *testing.T) {
	requireAllPassed(t, runSuite(t, nativeSubject(t), fastSuite(true)))
}

// The phase 7 definition of done, for the MAVLink adapter, against the fake
// ArduPilot pushing its datagrams the way SITL does.
func TestMAVLinkAdapterPassesTheSuite(t *testing.T) {
	s, err := conformance.MAVLinkSubject("127.0.0.1:0", mavlink.Config{
		SystemID:     1,
		Capabilities: vector.Capabilities{ID: "DRONE-01", Domain: vector.DomainAerial, Tags: []string{"camera", "gps"}, CruiseSpeed: 18, MaxRangeM: 60000, SensorRadiusM: 90},
		Agent:        agentOptions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	proxy := s.Faults.(*conformance.UDPProxy)
	ap, err := mavlinktest.New(mavlinktest.Options{SystemID: 1, Endpoint: &gomavlib.EndpointUDPClient{Address: proxy.VehicleAddr()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ap.Close)
	requireAllPassed(t, runSuite(t, s, fastSuite(true)))
	if armed, _, _ := ap.State(); !armed {
		t.Error("the copter C3 launched is no longer armed: C5's hold should leave it hovering")
	}
}

func TestMotionIsRefusedByDefault(t *testing.T) {
	rs := runSuite(t, nativeSubject(t), fastSuite(false))
	for _, r := range rs {
		want := conformance.Pass
		if r.Case == conformance.C3 || r.Case == conformance.C4 || r.Case == conformance.C5 {
			want = conformance.Skip
		}
		if r.Status != want {
			t.Errorf("%s: %s (%s), want %s", r.Case, r.Status, r.Detail, want)
		}
	}
	if conformance.Passed(rs) {
		t.Error("a run with skipped cases reported as passed")
	}
}

// swallowing is the adapter C8 exists to catch: it accepts a command it
// cannot perform and silently drops it.
type swallowing struct{ vector.Vector }

func (s swallowing) Execute(c vector.Command) error {
	if !slices.Contains(vector.CommandTypes(), c.Type) {
		return nil
	}
	return s.Vector.Execute(c)
}

func TestSuiteCatchesASilentAdapter(t *testing.T) {
	s := nativeSubject(t)
	open := s.Open
	s.Open = func(ctx context.Context) (vector.Vector, error) {
		v, err := open(ctx)
		if err != nil {
			return nil, err
		}
		return swallowing{v}, nil
	}
	for _, r := range runSuite(t, s, fastSuite(false)) {
		if r.Case == conformance.C8 && (r.Status != conformance.Fail || !strings.Contains(r.Detail, "teleport")) {
			t.Errorf("C8 %s: %s, want a failure naming the swallowed command", r.Status, r.Detail)
		}
	}
}
