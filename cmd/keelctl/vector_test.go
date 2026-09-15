package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v4"

	"github.com/dsanchez31/keel/adapters/mavlink/mavlinktest"
	"github.com/dsanchez31/keel/adapters/native"
	"github.com/dsanchez31/keel/vector"
)

var referenceWorld = "../../examples/worlds/reference.yaml"

// servedVehicle serves one scripted native vehicle until the test ends:
// frames every 20 ms, commands applied idempotently by Seq.
func servedVehicle(t *testing.T) string {
	t.Helper()
	caps := vector.Capabilities{ID: "DRONE-02", Domain: vector.DomainAerial, Tags: []string{"camera", "gps"}, CruiseSpeed: 18, MaxRangeM: 60000, SensorRadiusM: 90}
	srv, err := native.NewServer(native.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v, err := srv.Add(caps, vector.CommandTypes())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		s := vector.VectorState{ID: caps.ID, Position: vector.Position{Lat: 45.02698, Lon: 5.00526}, BatteryPct: 100, Link: vector.LinkOK, Mode: vector.ModeIdle}
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
				if c.Seq > s.AckSeq {
					s.AckSeq = c.Seq
					s.Mode = vector.ModeIdle
					if c.Type == vector.CommandGoto {
						s.Mode = vector.ModeTransit
					}
				}
			case <-tick.C:
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
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/vectors/DRONE-02"
}

// fast are thresholds scaled down from 5 s and 10 s, for the adapter and
// the suite alike.
var fast = []string{"--degraded-after", "200ms", "--lost-after", "500ms"}

func TestVectorNamesExactlyOneVehicle(t *testing.T) {
	cases := map[string][]string{
		"no vehicle":             {"vector", "describe"},
		"two vehicles":           {"vector", "describe", "--native", "ws://127.0.0.1:1/v1/vectors/X", "--mavlink", "127.0.0.1:14550", "--vector", "DRONE-01"},
		"MAVLink without vector": {"vector", "describe", "--mavlink", "127.0.0.1:14550"},
		"unknown fleet entry":    {"vector", "validate", "--mavlink", "127.0.0.1:0", "--vector", "NOPE", "--world", referenceWorld},
	}
	for name, args := range cases {
		if code, _, errOut := keelctl(t, args...); code != exitError || errOut == "" {
			t.Errorf("%s: exit %d, stderr %q", name, code, errOut)
		}
	}
}

func TestDescribeNative(t *testing.T) {
	code, out, errOut := keelctl(t, "vector", "describe", "--native", servedVehicle(t))
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"vector       DRONE-02, aerial, tags [camera, gps]", "supports     abort, goto, hold, rtb", "frame        idle", "health       ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("describe output lacks %q:\n%s", want, out)
		}
	}
}

// freeUDP is a loopback UDP address nothing listens on, for the command to
// take.
func freeUDP(t *testing.T) string {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr := c.LocalAddr().String()
	_ = c.Close()
	return addr
}

func TestDescribeMAVLink(t *testing.T) {
	addr := freeUDP(t)
	ap, err := mavlinktest.New(mavlinktest.Options{SystemID: 1, Endpoint: &gomavlib.EndpointUDPClient{Address: addr}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ap.Close)
	code, out, errOut := keelctl(t, "vector", "describe", "--mavlink", addr, "--sysid", "1", "--vector", "DRONE-01", "--world", referenceWorld, "--geoid-separation", "0")
	if code != exitOK {
		t.Fatalf("exit %d: %s\n%s", code, errOut, out)
	}
	for _, want := range []string{"vector       DRONE-01, aerial", "supports     abort, goto, hold, rtb", "frame        idle", "  380.0 m"} {
		if !strings.Contains(out, want) {
			t.Errorf("describe output lacks %q:\n%s", want, out)
		}
	}
}

// syncBuffer is a bytes.Buffer safe to read while a command writes to it.
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

func TestPlugStreamsUntilInterrupted(t *testing.T) {
	var out syncBuffer
	root := newRootCmd(&out, io.Discard)
	root.SetArgs([]string{"vector", "plug", "--native", servedVehicle(t)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(out.String(), "  idle ") < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("no stream of frames:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("interrupted plug returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("plug did not stop")
	}
	if !strings.Contains(out.String(), "health ok") {
		t.Errorf("no health line:\n%s", out.String())
	}
}

func TestValidateNative(t *testing.T) {
	url := servedVehicle(t)
	code, out, errOut := keelctl(t, append([]string{"vector", "validate", "--native", url, "--allow-motion"}, fast...)...)
	if code != exitOK {
		t.Fatalf("exit %d: %s\n%s", code, errOut, out)
	}
	if n := strings.Count(out, "  pass  "); n != 9 {
		t.Errorf("%d cases passed, want 9:\n%s", n, out)
	}
	if !strings.Contains(out, "conformant: all 9 cases passed") {
		t.Errorf("no verdict:\n%s", out)
	}
}

func TestValidateWithoutMotionIsNotConformant(t *testing.T) {
	url := servedVehicle(t)
	code, out, _ := keelctl(t, append([]string{"vector", "validate", "--native", url}, fast...)...)
	if code != exitNotConformant {
		t.Fatalf("exit %d, want %d:\n%s", code, exitNotConformant, out)
	}
	if !strings.Contains(out, "not conformant: C3, C4, C5 skipped, run with --allow-motion") {
		t.Errorf("verdict does not say how to conform:\n%s", out)
	}
}
