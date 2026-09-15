package transport

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/eventlog"
)

// streamServer serves a hub over a real listener, as the daemon will.
func streamServer(t *testing.T, opts HubOptions, trusted ...string) (*Hub, string) {
	t.Helper()
	hub, err := NewHub(opts)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(&fakeService{}, hub, Options{TrustedOrigins: trusted})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		_ = hub.Close()
		srv.Close()
	})
	return hub, "ws" + strings.TrimPrefix(srv.URL, "http") + StreamPath
}

func dial(t *testing.T, url string, header http.Header) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func readFrame(t *testing.T, conn *websocket.Conn) Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, b, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("message type %v, want text", typ)
	}
	var f Frame
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("frame %s: %v", b, err)
	}
	if !json.Valid(f.Data) || len(f.Data) == 0 {
		t.Fatalf("frame without data: %s", b)
	}
	return f
}

// eventually polls cond until it holds, for what a goroutine does in its own
// time.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func view(tick int64) MissionView {
	return MissionView{Tick: tick, TickMs: tick * 100, Head: eventlog.MustHashOf(tick).String(), Mission: domain.Mission{State: domain.MissionRunning}}
}

func tickMsgs(tick int64) []Message {
	return []Message{
		{Type: FrameDecision, Data: domain.Decision{TickMs: tick * 100, Kind: domain.DecisionOperatorNotice, Rationale: "note"}},
		{Type: FrameTelemetry, Data: []domain.VectorState{{ID: "DRONE-01", BatteryPct: 90, Link: domain.LinkOK, Mode: domain.ModeTransit}}},
		{Type: FrameTick, Data: TickSummary{Tick: tick, State: domain.MissionRunning, Cursors: []LaneCursor{}}},
	}
}

func publish(t *testing.T, hub *Hub, tick int64) {
	t.Helper()
	if err := hub.Publish(tick*100, view(tick), tickMsgs(tick)...); err != nil {
		t.Fatal(err)
	}
}

func TestStreamStartsWithTheSnapshot(t *testing.T) {
	hub, url := streamServer(t, HubOptions{})
	publish(t, hub, 1)
	publish(t, hub, 2)

	conn := dial(t, url, nil)
	f := readFrame(t, conn)
	if f.Seq != 1 || f.Type != FrameMission || f.T != 200 {
		t.Fatalf("first frame seq %d type %s t %d, want the mission snapshot of tick 2", f.Seq, f.Type, f.T)
	}
	var v MissionView
	if err := json.Unmarshal(f.Data, &v); err != nil || v.Tick != 2 {
		t.Fatalf("snapshot %s: %v", f.Data, err)
	}

	eventually(t, "the subscription", func() bool { return hub.Clients() == 1 })
	publish(t, hub, 3)
	for i, want := range []FrameType{FrameDecision, FrameTelemetry, FrameTick} {
		f := readFrame(t, conn)
		if f.Seq != uint64(i+2) || f.Type != want || f.T != 300 {
			t.Fatalf("frame %d: seq %d type %s t %d, want seq %d %s at 300", i, f.Seq, f.Type, f.T, i+2, want)
		}
	}
}

func TestStreamJoiningBeforeTheFirstTick(t *testing.T) {
	hub, url := streamServer(t, HubOptions{})
	conn := dial(t, url, nil)
	eventually(t, "the subscription", func() bool { return hub.Clients() == 1 })

	publish(t, hub, 1)
	f := readFrame(t, conn)
	if f.Seq != 1 || f.Type != FrameMission || f.T != 100 {
		t.Fatalf("first frame seq %d type %s t %d, want tick 1's snapshot alone", f.Seq, f.Type, f.T)
	}
	publish(t, hub, 2)
	if f := readFrame(t, conn); f.Seq != 2 || f.Type != FrameDecision || f.T != 200 {
		t.Fatalf("second frame seq %d type %s t %d, want tick 2's first frame", f.Seq, f.Type, f.T)
	}
}

func TestSlowClientSeesTheGap(t *testing.T) {
	c := &client{out: make(chan EncodedFrame, 2)}
	for range 5 {
		c.send(EncodedFrame{Type: FrameTick})
	}
	if c.dropped != 3 {
		t.Fatalf("%d frames dropped, want 3", c.dropped)
	}
	var seqs []uint64
	for range 2 {
		seqs = append(seqs, (<-c.out).Seq)
	}
	c.send(EncodedFrame{Type: FrameTick})
	seqs = append(seqs, (<-c.out).Seq)
	if !slices.Equal(seqs, []uint64{1, 2, 6}) {
		t.Fatalf("seqs %v, want [1 2 6]: a dropped frame consumes its seq", seqs)
	}
}

func TestPublishEncodingFailurePublishesNothing(t *testing.T) {
	hub, url := streamServer(t, HubOptions{})
	publish(t, hub, 1)
	conn := dial(t, url, nil)
	readFrame(t, conn)
	eventually(t, "the subscription", func() bool { return hub.Clients() == 1 })

	bad := []Message{
		{Type: FrameTick, Data: TickSummary{Tick: 2}},
		{Type: FrameTelemetry, Data: []domain.VectorState{{Speed: math.NaN()}}},
	}
	var nf *eventlog.NonFiniteError
	if err := hub.Publish(200, view(2), bad...); !errors.As(err, &nf) {
		t.Fatalf("Publish = %v, want a non-finite encoding error", err)
	}
	publish(t, hub, 3)
	if f := readFrame(t, conn); f.Seq != 2 || f.T != 300 {
		t.Fatalf("frame seq %d at %d, want tick 3's first as seq 2: the failed tick sent nothing", f.Seq, f.T)
	}
}

func TestStreamIsServerToClient(t *testing.T) {
	hub, url := streamServer(t, HubOptions{})
	conn := dial(t, url, nil)
	eventually(t, "the subscription", func() bool { return hub.Clients() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"hello":1}`)); err != nil {
		t.Fatal(err)
	}
	_, _, err := conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Fatalf("close status %v (%v), want policy violation", got, err)
	}
	eventually(t, "the unsubscription", func() bool { return hub.Clients() == 0 })
}

func TestHubCloseGoesAway(t *testing.T) {
	hub, url := streamServer(t, HubOptions{})
	publish(t, hub, 1)
	conn := dial(t, url, nil)
	readFrame(t, conn)
	eventually(t, "the subscription", func() bool { return hub.Clients() == 1 })

	closed := make(chan error, 1)
	go func() { closed <- hub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
		t.Fatalf("close status %v (%v), want going away", got, err)
	}
	if err := <-closed; err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if err := hub.Publish(200, view(2)); !errors.Is(err, ErrHubClosed) {
		t.Fatalf("Publish after Close = %v, want ErrHubClosed", err)
	}
	if n := hub.Clients(); n != 0 {
		t.Fatalf("%d connections left after Close", n)
	}
}

func TestStreamOrigin(t *testing.T) {
	_, url := streamServer(t, HubOptions{}, "http://localhost:5173")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a foreign origin opened the stream: %v", err)
	}
	dial(t, url, http.Header{"Origin": {"http://localhost:5173"}})
}
