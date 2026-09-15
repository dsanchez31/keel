package native_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dsanchez31/keel/adapters/native"
	"github.com/dsanchez31/keel/vector"
)

// wait bounds every wait on the network. Generous: a slow CI machine under
// -race is not a failure.
const wait = 5 * time.Second

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

// fast keeps reconnects quick.
func fast() native.Options {
	return native.Options{MinBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond}
}

func vectorURL(ts *httptest.Server, id vector.VectorID) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/vectors/" + url.PathEscape(string(id))
}

// fleet serves DRONE-01 declaring supports, every command type when none.
func fleet(t *testing.T, supports ...vector.CommandType) (*native.Vehicle, *native.Server, *httptest.Server) {
	t.Helper()
	srv, err := native.NewServer(native.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if supports == nil {
		supports = vector.CommandTypes()
	}
	v, err := srv.Add(caps(), supports)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		_ = srv.Close()
		ts.Close()
	})
	return v, srv, ts
}

func dial(t *testing.T, u string, opts native.Options) *native.Adapter {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), wait)
	defer cancel()
	a, err := native.Dial(ctx, u, opts)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func recv(t *testing.T, a *native.Adapter) vector.VectorState {
	t.Helper()
	select {
	case s, ok := <-a.Telemetry():
		if !ok {
			t.Fatal("telemetry channel closed")
		}
		return s
	case <-time.After(wait):
		t.Fatal("no frame")
	}
	return vector.VectorState{}
}

func command(t *testing.T, v *native.Vehicle) vector.Command {
	t.Helper()
	select {
	case c, ok := <-v.Commands():
		if !ok {
			t.Fatal("command channel closed")
		}
		return c
	case <-time.After(wait):
		t.Fatal("no command")
	}
	return vector.Command{}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func lostWith(a *native.Adapter, detail string) func() bool {
	return func() bool {
		h := a.Health()
		return h.Kind == vector.HealthLost && strings.Contains(h.Detail, detail)
	}
}

// peer is a scripted vehicle, for what the Server would never send.
// Connection n, from 1, runs script and is dropped when it returns.
func peer(t *testing.T, opts *websocket.AcceptOptions, script func(n int, c *websocket.Conn)) *httptest.Server {
	t.Helper()
	var n atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, opts)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		script(int(n.Add(1)), c)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func vehicleSide() *websocket.AcceptOptions {
	return &websocket.AcceptOptions{Subprotocols: []string{native.Subprotocol}}
}

func send(c *websocket.Conn, typ websocket.MessageType, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	_ = c.Write(ctx, typ, []byte(msg))
}

// closed reads until the connection ends and returns the close status the
// other end sent, -1 when it sent none.
func closed(c *websocket.Conn) websocket.StatusCode {
	for {
		if _, _, err := c.Read(context.Background()); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func message(typ string, data any) string {
	b, err := json.Marshal(map[string]any{"type": typ, "data": data})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// object is v as a JSON object, for a test to edit field by field.
func object(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		panic(err)
	}
	return m
}

func helloData(edit func(map[string]any)) map[string]any {
	h := map[string]any{"capabilities": object(caps()), "supports": []string{"goto", "hold"}}
	if edit != nil {
		edit(h)
	}
	return h
}

func frameData(ms int64, edit func(map[string]any)) map[string]any {
	f := object(frame(ms))
	if edit != nil {
		edit(f)
	}
	return f
}

func TestDialFixesTheHello(t *testing.T) {
	v, _, ts := fleet(t, vector.CommandHold, vector.CommandGoto, vector.CommandHold)
	a := dial(t, vectorURL(ts, v.ID()), fast())
	got := a.Describe()
	if got.ID != "DRONE-01" || got.CruiseSpeed != 15 || !slices.Equal(got.Tags, []string{"camera", "gps"}) {
		t.Fatalf("described %+v, want the hello's capabilities with sorted tags", got)
	}
	if want := []vector.CommandType{vector.CommandGoto, vector.CommandHold}; !slices.Equal(a.Supports(), want) {
		t.Fatalf("supports %v, want %v", a.Supports(), want)
	}
	if h := a.Health(); h.Kind != vector.HealthLost || !strings.Contains(h.Detail, "no frame yet") {
		t.Fatalf("health before any frame: %+v, want lost", h)
	}
}

func TestTelemetryAndCommandsFlow(t *testing.T) {
	v, _, ts := fleet(t)
	// Published before any adapter: kept for the first connection.
	if err := v.Send(frame(100)); err != nil {
		t.Fatal(err)
	}
	a := dial(t, vectorURL(ts, v.ID()), fast())
	if got := recv(t, a); got.LastSeenMs != 100 {
		t.Fatalf("first frame at %d ms, want 100", got.LastSeenMs)
	}
	if h := a.Health(); h.Kind != vector.HealthOK {
		t.Fatalf("health after a frame: %+v, want ok", h)
	}

	want := gotoCmd(1)
	if err := a.Execute(want); err != nil {
		t.Fatal(err)
	}
	got := command(t, v)
	if got.Vector != want.Vector || got.Seq != 1 || got.Type != want.Type || got.Lane != want.Lane ||
		got.Waypoint == nil || *got.Waypoint != *want.Waypoint {
		t.Fatalf("vehicle received %+v, want %+v", got, want)
	}

	f := frame(200)
	f.AckSeq = 1
	if err := v.Send(f); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, a); got.AckSeq != 1 {
		t.Fatalf("frame acknowledges seq %d, want 1", got.AckSeq)
	}
	// Acknowledged: a re-send of seq 1 is a no-op and never reaches the
	// vehicle, the next command does.
	if err := a.Execute(gotoCmd(1)); err != nil {
		t.Fatal(err)
	}
	if err := a.Execute(vector.Command{Vector: "DRONE-01", Seq: 2, Type: vector.CommandHold, Lane: "L0"}); err != nil {
		t.Fatal(err)
	}
	if got := command(t, v); got.Seq != 2 || got.Type != vector.CommandHold {
		t.Fatalf("vehicle received %+v, want hold seq 2", got)
	}
}

// Conformance case C8: what the vehicle did not declare is refused, never
// sent to be ignored.
func TestExecuteRefusesWhatTheHelloDidNotDeclare(t *testing.T) {
	v, _, ts := fleet(t, vector.CommandGoto, vector.CommandHold)
	a := dial(t, vectorURL(ts, v.ID()), fast())
	for _, typ := range []vector.CommandType{vector.CommandRTB, vector.CommandAbort, "orbit"} {
		if err := a.Execute(vector.Command{Vector: "DRONE-01", Seq: 1, Type: typ}); !errors.Is(err, vector.ErrUnsupported) {
			t.Errorf("%s: error %v, want ErrUnsupported", typ, err)
		}
	}
	select {
	case c := <-v.Commands():
		t.Fatalf("vehicle received %+v", c)
	case <-time.After(100 * time.Millisecond):
	}
}

// Conformance case C7: the adapter redials on its own, and frames resume on
// the channel the consumer already holds.
func TestReconnectResumesOnTheSameChannel(t *testing.T) {
	v, _, ts := fleet(t)
	a := dial(t, vectorURL(ts, v.ID()), fast())
	ch := a.Telemetry()
	if err := v.Send(frame(100)); err != nil {
		t.Fatal(err)
	}
	recv(t, a)

	v.Disconnect()
	eventually(t, "the disconnect in Health", lostWith(a, "disconnected"))

	deadline := time.After(wait)
	for ms := int64(200); ; ms += 100 {
		if err := v.Send(frame(ms)); err != nil {
			t.Fatal(err)
		}
		select {
		case s := <-ch:
			if s.LastSeenMs < 200 {
				t.Fatalf("frame at %d ms after the reconnect, want a new one", s.LastSeenMs)
			}
			if h := a.Health(); h.Kind != vector.HealthOK {
				t.Fatalf("health after reconnecting: %+v, want ok", h)
			}
			return
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("telemetry did not resume")
		}
	}
}

// Strict decoding and conformance case C2: a message the protocol does not
// allow closes the link with a status naming it, and Health says why.
func TestViolationsCloseTheLink(t *testing.T) {
	text, binary := websocket.MessageText, websocket.MessageBinary
	telemetry := func(edit func(map[string]any)) string { return message("telemetry", frameData(100, edit)) }
	cases := []struct {
		name   string
		typ    websocket.MessageType
		msg    string
		status websocket.StatusCode
		detail string
	}{
		{"unknown field", text, telemetry(func(m map[string]any) { m["wind"] = 3 }), websocket.StatusProtocolError, "telemetry.wind: unknown field"},
		{"key case", text, telemetry(func(m map[string]any) { m["ID"] = m["id"]; delete(m, "id") }), websocket.StatusProtocolError, "telemetry.ID: unknown field"},
		{"missing longitude", text, telemetry(func(m map[string]any) { delete(m["position"].(map[string]any), "lon") }), websocket.StatusProtocolError, "telemetry.position.lon: missing"},
		{"null is absent", text, telemetry(func(m map[string]any) { m["mode"] = nil }), websocket.StatusProtocolError, "telemetry.mode: missing"},
		{"unknown type", text, message("status", map[string]any{}), websocket.StatusProtocolError, `unknown message type "status"`},
		{"second hello", text, message("hello", helloData(nil)), websocket.StatusProtocolError, "hello message, want telemetry"},
		{"command from the vehicle", text, message("command", object(gotoCmd(1))), websocket.StatusProtocolError, "command message, want telemetry"},
		{"not an object", text, `[1, 2]`, websocket.StatusProtocolError, "message: want a JSON object"},
		{"binary", binary, telemetry(nil), websocket.StatusUnsupportedData, "binary message"},
		{"battery over 100", text, telemetry(func(m map[string]any) { m["battery_pct"] = 101 }), websocket.StatusPolicyViolation, "telemetry refused"},
		{"scanning", text, telemetry(func(m map[string]any) { m["mode"] = "scanning" }), websocket.StatusPolicyViolation, "telemetry refused"},
		{"other vector", text, telemetry(func(m map[string]any) { m["id"] = "DRONE-02" }), websocket.StatusPolicyViolation, "telemetry refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := make(chan websocket.StatusCode, 1)
			ts := peer(t, vehicleSide(), func(n int, c *websocket.Conn) {
				send(c, websocket.MessageText, message("hello", helloData(nil)))
				if n == 1 {
					send(c, tc.typ, tc.msg)
					status <- closed(c)
					return
				}
				closed(c)
			})
			a := dial(t, ts.URL, fast())
			select {
			case got := <-status:
				if got != tc.status {
					t.Fatalf("vehicle saw close status %d, want %d", got, tc.status)
				}
			case <-time.After(wait):
				t.Fatal("the adapter did not close the link")
			}
			eventually(t, "the violation in Health", lostWith(a, tc.detail))
		})
	}
}

func TestDialRefusesABadHello(t *testing.T) {
	onCaps := func(edit func(map[string]any)) func(map[string]any) {
		return func(h map[string]any) { edit(h["capabilities"].(map[string]any)) }
	}
	cases := []struct {
		name  string
		opts  *websocket.AcceptOptions
		first string
	}{
		{"no subprotocol", &websocket.AcceptOptions{}, message("hello", helloData(nil))},
		{"telemetry first", vehicleSide(), message("telemetry", frameData(100, nil))},
		{"supports missing", vehicleSide(), message("hello", helloData(func(h map[string]any) { delete(h, "supports") }))},
		{"supports empty", vehicleSide(), message("hello", helloData(func(h map[string]any) { h["supports"] = []string{} }))},
		{"supports unknown", vehicleSide(), message("hello", helloData(func(h map[string]any) { h["supports"] = []string{"orbit"} }))},
		{"zero cruise speed", vehicleSide(), message("hello", helloData(onCaps(func(c map[string]any) { c["cruise_speed"] = 0 })))},
		{"tags missing", vehicleSide(), message("hello", helloData(onCaps(func(c map[string]any) { delete(c, "tags") })))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := peer(t, tc.opts, func(_ int, c *websocket.Conn) {
				send(c, websocket.MessageText, tc.first)
				closed(c)
			})
			ctx, cancel := context.WithTimeout(t.Context(), wait)
			defer cancel()
			a, err := native.Dial(ctx, ts.URL, fast())
			if err == nil {
				_ = a.Close()
				t.Fatal("dialled")
			}
			if !errors.Is(err, native.ErrProtocol) {
				t.Fatalf("error %v, want ErrProtocol", err)
			}
		})
	}
}

func TestDialRefusesAgentSupports(t *testing.T) {
	opts := fast()
	opts.Agent.Supports = []vector.CommandType{vector.CommandGoto}
	if a, err := native.Dial(t.Context(), "ws://127.0.0.1:1/v1/vectors/DRONE-01", opts); err == nil {
		_ = a.Close()
		t.Fatal("dialled: the vehicle's hello, not the adapter, declares the command types")
	}
}

// Describe is stable for the adapter's lifetime: a vehicle that comes back
// declaring something else is refused, again and again.
func TestReconnectRefusesAChangedHello(t *testing.T) {
	status := make(chan websocket.StatusCode, 1)
	ts := peer(t, vehicleSide(), func(n int, c *websocket.Conn) {
		if n == 1 {
			send(c, websocket.MessageText, message("hello", helloData(nil)))
			send(c, websocket.MessageText, message("telemetry", frameData(100, nil)))
			_ = c.CloseNow()
			return
		}
		send(c, websocket.MessageText, message("hello", helloData(func(h map[string]any) { h["supports"] = []string{"goto"} })))
		s := closed(c)
		if n == 2 {
			status <- s
		}
	})
	a := dial(t, ts.URL, fast())
	recv(t, a)
	select {
	case got := <-status:
		if got != websocket.StatusPolicyViolation {
			t.Fatalf("vehicle saw close status %d, want %d", got, websocket.StatusPolicyViolation)
		}
	case <-time.After(wait):
		t.Fatal("the changed hello was not refused")
	}
	eventually(t, "the refusal in Health", lostWith(a, "hello differs"))
	if want := []vector.CommandType{vector.CommandGoto, vector.CommandHold}; !slices.Equal(a.Supports(), want) {
		t.Fatalf("supports %v after the refusal, want the first hello's %v", a.Supports(), want)
	}
}

// A half-open connection carries nothing: the idle timeout drops it and the
// adapter redials.
func TestIdleLinkIsRedialled(t *testing.T) {
	var conns atomic.Int32
	ts := peer(t, vehicleSide(), func(n int, c *websocket.Conn) {
		conns.Store(int32(n))
		send(c, websocket.MessageText, message("hello", helloData(nil)))
		closed(c)
	})
	opts := fast()
	opts.IdleTimeout = 100 * time.Millisecond
	a := dial(t, ts.URL, opts)
	eventually(t, "a redial", func() bool { return conns.Load() >= 2 })
	eventually(t, "the silence in Health", lostWith(a, "no message for 100ms"))
}

func TestNewerConnectionReplacesTheCurrent(t *testing.T) {
	v, _, ts := fleet(t)
	u := vectorURL(ts, v.ID())
	// The first adapter does not redial within the test, or the two would
	// replace each other in turn.
	first := dial(t, u, native.Options{MinBackoff: time.Minute, MaxBackoff: time.Minute})
	second := dial(t, u, fast())
	eventually(t, "the first adapter replaced", lostWith(first, "replaced by a newer connection"))
	if err := second.Execute(gotoCmd(1)); err != nil {
		t.Fatal(err)
	}
	if got := command(t, v); got.Seq != 1 {
		t.Fatalf("vehicle received %+v, want seq 1 from the newer connection", got)
	}
	if !v.Connected() {
		t.Fatal("vehicle reports no connection")
	}
}

// Conformance case C9, as far as the adapter can see it: Close ends the
// channel, refuses commands, and drops the link.
func TestCloseEndsTheAdapter(t *testing.T) {
	v, _, ts := fleet(t)
	a := dial(t, vectorURL(ts, v.ID()), fast())
	if err := v.Send(frame(100)); err != nil {
		t.Fatal(err)
	}
	recv(t, a)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	deadline := time.After(wait)
	for open := true; open; {
		select {
		case _, open = <-a.Telemetry():
		case <-deadline:
			t.Fatal("telemetry channel still open")
		}
	}
	if err := a.Execute(gotoCmd(1)); !errors.Is(err, vector.ErrClosed) {
		t.Fatalf("Execute after Close: %v, want ErrClosed", err)
	}
	if h := a.Health(); h.Kind != vector.HealthLost || h.Detail != "closed" {
		t.Fatalf("health after Close: %+v", h)
	}
	eventually(t, "the vehicle to see the link drop", func() bool { return !v.Connected() })
}

func TestServerCloseEndsEveryVehicle(t *testing.T) {
	v, srv, ts := fleet(t)
	a := dial(t, vectorURL(ts, v.ID()), fast())
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	deadline := time.After(wait)
	for open := true; open; {
		select {
		case _, open = <-v.Commands():
		case <-deadline:
			t.Fatal("command channel still open")
		}
	}
	if err := v.Send(frame(100)); !errors.Is(err, vector.ErrClosed) {
		t.Fatalf("Send after Close: %v, want ErrClosed", err)
	}
	other := caps()
	other.ID = "DRONE-02"
	if _, err := srv.Add(other, vector.CommandTypes()); !errors.Is(err, vector.ErrClosed) {
		t.Fatalf("Add after Close: %v, want ErrClosed", err)
	}
	eventually(t, "the adapter to see the link drop", lostWith(a, "disconnected"))
}

func TestServerRefusesMisbehavingAdapters(t *testing.T) {
	v, _, ts := fleet(t, vector.CommandGoto, vector.CommandHold)
	u := vectorURL(ts, v.ID())
	ctx, cancel := context.WithTimeout(t.Context(), wait)
	defer cancel()
	adapterSide := &websocket.DialOptions{Subprotocols: []string{native.Subprotocol}}

	t.Run("unknown vector", func(t *testing.T) {
		c, resp, err := websocket.Dial(ctx, vectorURL(ts, "DRONE-09"), adapterSide)
		if err == nil {
			_ = c.CloseNow()
			t.Fatal("upgraded")
		}
		if resp == nil || resp.StatusCode != http.StatusNotFound {
			t.Fatalf("response %v, want 404", resp)
		}
	})
	t.Run("no subprotocol", func(t *testing.T) {
		c, _, err := websocket.Dial(ctx, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.CloseNow() }()
		if got := closed(c); got != websocket.StatusProtocolError {
			t.Fatalf("close status %d, want %d", got, websocket.StatusProtocolError)
		}
	})

	undeclared := object(gotoCmd(1))
	undeclared["type"] = "rtb"
	delete(undeclared, "waypoint")
	misaddressed := object(gotoCmd(1))
	misaddressed["vector"] = "DRONE-02"
	noSeq := object(gotoCmd(1))
	delete(noSeq, "seq")
	cases := []struct {
		name   string
		msg    string
		status websocket.StatusCode
	}{
		{"undeclared type", message("command", undeclared), websocket.StatusPolicyViolation},
		{"other vector", message("command", misaddressed), websocket.StatusPolicyViolation},
		{"seq missing", message("command", noSeq), websocket.StatusProtocolError},
		{"telemetry from the adapter", message("telemetry", frameData(100, nil)), websocket.StatusProtocolError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, err := websocket.Dial(ctx, u, adapterSide)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.CloseNow() }()
			_, b, err := c.Read(ctx)
			if err != nil || !strings.Contains(string(b), `"type":"hello"`) {
				t.Fatalf("first message %s (%v), want the hello", b, err)
			}
			send(c, websocket.MessageText, tc.msg)
			if got := closed(c); got != tc.status {
				t.Fatalf("close status %d, want %d", got, tc.status)
			}
		})
	}
	select {
	case c := <-v.Commands():
		t.Fatalf("a refused command was delivered: %+v", c)
	default:
	}
}

func TestServerRefusesWhatCannotBeServed(t *testing.T) {
	srv, err := native.NewServer(native.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	v, err := srv.Add(caps(), vector.CommandTypes())
	if err != nil {
		t.Fatal(err)
	}
	slash, empty, unknown := caps(), caps(), caps()
	slash.ID, empty.ID, unknown.ID = "DRONE/01", "DRONE-02", "DRONE-03"
	nan := caps()
	nan.ID, nan.CruiseSpeed = "DRONE-04", math.NaN()
	adds := []struct {
		name     string
		caps     vector.Capabilities
		supports []vector.CommandType
	}{
		{"duplicate", caps(), vector.CommandTypes()},
		{"id with a slash", slash, vector.CommandTypes()},
		{"supports nothing", empty, nil},
		{"unknown type", unknown, []vector.CommandType{"orbit"}},
		{"not encodable", nan, vector.CommandTypes()},
	}
	for _, tc := range adds {
		if _, err := srv.Add(tc.caps, tc.supports); err == nil {
			t.Errorf("%s: added", tc.name)
		}
	}
	other := frame(100)
	other.ID = "DRONE-02"
	bad := frame(100)
	bad.Heading = math.Inf(1)
	for _, f := range []vector.VectorState{other, bad} {
		if err := v.Send(f); !errors.Is(err, vector.ErrInvalidFrame) {
			t.Errorf("Send %+v: %v, want ErrInvalidFrame", f, err)
		}
	}
}
