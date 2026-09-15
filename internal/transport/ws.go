package transport

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/httpjson"
)

// StreamPath is the stream's endpoint (spec section 8.2).
const StreamPath = "/api/v1/stream"

// FrameType discriminates the frames of the stream.
type FrameType string

const (
	// FrameMission carries a MissionView: every connection's first frame,
	// and again whenever a tick reshapes the mission.
	FrameMission FrameType = "mission"
	// FrameEvent carries one domain.Event the engine applied.
	FrameEvent FrameType = "event"
	// FrameDecision carries one domain.Decision with its full trace.
	FrameDecision FrameType = "decision"
	// FrameDoctrine carries the DoctrineInfo of a pack that became active.
	FrameDoctrine FrameType = "doctrine"
	// FrameTelemetry carries every vector's domain.VectorState, in id order.
	FrameTelemetry FrameType = "telemetry"
	// FrameCoverage carries a CoverageDelta.
	FrameCoverage FrameType = "coverage"
	// FrameTick carries a TickSummary and closes a tick.
	FrameTick FrameType = "tick"
)

// Frame is one message of the stream, as a client reads it.
//
// T is the mission time of the tick the frame belongs to. Seq numbers the
// frames of one connection from 1 without a gap: a gap is a frame the server
// dropped because the client fell behind, and the client resynchronises.
type Frame struct {
	T    int64           `json:"t"`
	Seq  uint64          `json:"seq"`
	Type FrameType       `json:"type"`
	Data json.RawMessage `json:"data"`
}

// EncodedFrame is a Frame as the server writes it, the data spliced in
// already canonical: the hub encodes it once per tick rather than once per
// connection, and a replay window (ReplayWindow) carries its frames so,
// through the one canonical encoder rather than around it.
type EncodedFrame struct {
	Data eventlog.RawJSON `json:"data"`
	Seq  uint64           `json:"seq"`
	T    int64            `json:"t"`
	Type FrameType        `json:"type"`
}

// Hub defaults.
const (
	// DefaultStreamBuffer is how many frames a connection may fall behind
	// before the hub drops. A tick is about ten frames, so this is some ten
	// seconds of mission.
	DefaultStreamBuffer = 1024
	// DefaultWriteTimeout bounds one frame write. A client that cannot take
	// one frame in that time is dropped.
	DefaultWriteTimeout = 10 * time.Second
	// DefaultPingInterval is how often an idle-looking connection is
	// pinged, which is what notices a peer that vanished without closing.
	DefaultPingInterval = 30 * time.Second
)

// HubOptions tunes a Hub. The zero value is the defaults.
type HubOptions struct {
	Buffer       int
	WriteTimeout time.Duration
	PingInterval time.Duration
}

// Hub fans the daemon's ticks out to every stream connection.
//
// The daemon calls Publish once per tick with the view after the tick and
// the tick's messages; the hub keeps the view as the snapshot the next
// connection starts from. Both happen under one lock, so a connection joins
// between two ticks, never inside one: its first frame is the state after
// tick N and its next frames are tick N+1's, nothing missing and nothing
// applied twice.
//
// Delivery never blocks the daemon. Each connection has a bounded buffer
// drained by its own writer; when it is full the frame is dropped and its
// seq consumed anyway, so the client sees the gap spec section 8.2 tells it
// to resynchronise on. Blocking would let one slow browser stall the tick
// loop; dropping silently would leave it drawing a state that never was.
type Hub struct {
	buffer       int
	writeTimeout time.Duration
	pingInterval time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	clients map[*client]struct{}
	// snap is the view of the last Publish and snapT its mission time. It is
	// encoded on the first subscription after that Publish and cached in
	// snapData, since most ticks see no one join.
	snap     *MissionView
	snapT    int64
	snapData eventlog.RawJSON
}

// client is one connection's queue. seq and dropped are guarded by the
// hub's lock.
type client struct {
	out     chan EncodedFrame
	seq     uint64
	dropped uint64
	// waiting marks a connection that joined before the first Publish: it
	// is sent that Publish's view as its snapshot, not the tick's frames the
	// view already reflects.
	waiting bool
}

// NewHub returns a hub with no connection and no snapshot.
func NewHub(opts HubOptions) (*Hub, error) {
	h := &Hub{
		buffer:       cmp.Or(opts.Buffer, DefaultStreamBuffer),
		writeTimeout: cmp.Or(opts.WriteTimeout, DefaultWriteTimeout),
		pingInterval: cmp.Or(opts.PingInterval, DefaultPingInterval),
		clients:      map[*client]struct{}{},
	}
	switch {
	case h.buffer < 1:
		return nil, fmt.Errorf("transport: stream buffer %d, want at least 1", h.buffer)
	case h.writeTimeout <= 0:
		return nil, fmt.Errorf("transport: write timeout %v, want positive", h.writeTimeout)
	case h.pingInterval <= 0:
		return nil, fmt.Errorf("transport: ping interval %v, want positive", h.pingInterval)
	}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	return h, nil
}

// ErrHubClosed reports a Publish after Close.
var ErrHubClosed = errors.New("transport: hub closed")

// Publish sends one tick to every connection: view is the state after the
// tick at mission time t, msgs are the tick's messages (TickMessages).
//
// Every frame is encoded before anything is sent, and an encoding failure (a
// NaN in a view is a bug upstream) publishes nothing: a tick reaches every
// connection whole or not at all.
func (h *Hub) Publish(t int64, view MissionView, msgs ...Message) error {
	data := make([]eventlog.RawJSON, len(msgs))
	for i, m := range msgs {
		b, err := eventlog.Canonical(m.Data)
		if err != nil {
			return fmt.Errorf("transport: encoding a %s frame: %w", m.Type, err)
		}
		data[i] = b
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrHubClosed
	}
	// A connection waiting for its first snapshot gets this one, encoded now
	// so that a failure leaves the hub as it was.
	var snapData eventlog.RawJSON
	for c := range h.clients {
		if c.waiting {
			b, err := eventlog.Canonical(view)
			if err != nil {
				return fmt.Errorf("transport: encoding the mission snapshot: %w", err)
			}
			snapData = b
			break
		}
	}
	h.snap, h.snapT, h.snapData = &view, t, snapData
	for c := range h.clients {
		if c.waiting {
			c.waiting = false
			c.send(EncodedFrame{Data: snapData, T: t, Type: FrameMission})
			continue
		}
		for i, m := range msgs {
			c.send(EncodedFrame{Data: data[i], T: t, Type: m.Type})
		}
	}
	return nil
}

// send numbers a frame for this connection and queues it, dropping it when
// the queue is full. The seq is consumed either way.
func (c *client) send(e EncodedFrame) {
	c.seq++
	e.Seq = c.seq
	select {
	case c.out <- e:
	default:
		c.dropped++
	}
}

// snapshotLocked is the mission frame of the current snapshot, encoded once
// per Publish.
func (h *Hub) snapshotLocked() (EncodedFrame, error) {
	if h.snapData == nil {
		b, err := eventlog.Canonical(*h.snap)
		if err != nil {
			return EncodedFrame{}, fmt.Errorf("transport: encoding the mission snapshot: %w", err)
		}
		h.snapData = b
	}
	return EncodedFrame{Data: h.snapData, T: h.snapT, Type: FrameMission}, nil
}

// subscribe registers a connection, its snapshot queued as its first frame,
// or held for the first Publish when there is none yet.
func (h *Hub) subscribe() (*client, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrHubClosed
	}
	c := &client{out: make(chan EncodedFrame, h.buffer)}
	if h.snap == nil {
		c.waiting = true
	} else {
		snap, err := h.snapshotLocked()
		if err != nil {
			return nil, err
		}
		c.send(snap)
	}
	h.clients[c] = struct{}{}
	return c, nil
}

func (h *Hub) unsubscribe(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
}

// Clients is the number of open stream connections.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Close closes every stream connection with status 1001 (going away) and
// waits for their goroutines; Publish fails after it. Calling it again
// returns nil.
//
// Stream connections are hijacked from the http.Server, which no longer
// tracks them: the daemon closes the hub before shutting the server down.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.mu.Unlock()
	h.cancel()
	h.wg.Wait()
	return nil
}

// serveStream upgrades a request to a stream connection and writes to it
// until the connection or the hub closes. The stream runs server to client:
// the read side is closed, and a data message from the client closes the
// connection with status 1008.
func (h *Hub) serveStream(w http.ResponseWriter, r *http.Request, accept *websocket.AcceptOptions) {
	h.mu.Lock()
	closed := h.closed
	if !closed {
		h.wg.Add(1)
	}
	h.mu.Unlock()
	if closed {
		httpjson.WriteProblem(w, http.StatusServiceUnavailable, "the stream is shutting down")
		return
	}
	defer h.wg.Done()

	conn, err := websocket.Accept(w, r, accept)
	if err != nil {
		// Accept has written the HTTP error.
		return
	}
	c, err := h.subscribe()
	if err != nil {
		_ = conn.Close(websocket.StatusGoingAway, "stream shutting down")
		return
	}
	defer h.unsubscribe(c)

	// The read side lives as long as the connection, not the hub: a read
	// whose context ends closes the connection outright, and the hub closing
	// must leave it open long enough for the 1001 handshake below.
	//
	// It is read here rather than through conn.CloseRead. On a data message,
	// CloseRead's goroutine closes the connection and then waits for itself to
	// exit, 15 s before its context ends (coder/websocket v1.8.15,
	// Conn.waitGoroutines), holding the client in the hub all that time. The
	// durable fix is upstream; this loop can return to CloseRead once a release
	// carries it.
	peer := make(chan error, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		// Reader answers ping and close frames itself and returns on the
		// first data message, or when the connection closes.
		_, _, err := conn.Reader(context.Background())
		peer <- err
	}()
	// A closed connection ends the read: the hub's Close waits for it as for
	// this function.
	defer func() {
		_ = conn.CloseNow()
		<-readDone
	}()
	ping := time.NewTicker(h.pingInterval)
	defer ping.Stop()
	for {
		select {
		case err := <-peer:
			if err == nil {
				_ = conn.Close(websocket.StatusPolicyViolation, "the stream is server to client")
				return
			}
			_ = conn.CloseNow()
			return
		case <-h.ctx.Done():
			_ = conn.Close(websocket.StatusGoingAway, "stream shutting down")
			return
		case e := <-c.out:
			if err := h.write(conn, e); err != nil {
				_ = conn.CloseNow()
				return
			}
		case <-ping.C:
			ctx, cancel := context.WithTimeout(h.ctx, h.writeTimeout)
			err := conn.Ping(ctx)
			cancel()
			if err != nil {
				_ = conn.CloseNow()
				return
			}
		}
	}
}

// write sends one frame within the write timeout.
func (h *Hub) write(conn *websocket.Conn, e EncodedFrame) error {
	b, err := eventlog.Canonical(e)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(h.ctx, h.writeTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, b)
}
