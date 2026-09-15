package native

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/dsanchez31/keel/vector"
)

// DefaultCommandBuffer is a vehicle's command channel capacity.
const DefaultCommandBuffer = 16

// ServerOptions tunes a Server. The zero value is the defaults.
type ServerOptions struct {
	// WriteTimeout bounds one hello or frame write. Zero means
	// DefaultWriteTimeout.
	WriteTimeout time.Duration
	// CommandBuffer is each vehicle's command channel capacity, at least 1.
	// Zero means DefaultCommandBuffer.
	CommandBuffer int
}

// Server is the vehicle side of the native protocol: an http.Handler serving
// every registered vehicle at VectorPath, /v1/vectors/{id}. It is the peer of
// the adapter's tests and the endpoint keelsim --serve serves its simulated
// fleet through. A vehicle written in Go can use it as is; one written in
// anything else reads spec section 7.3.
//
// One connection per vehicle at a time. A newer connection replaces the
// current one, which is closed with status 1001 (going away): an adapter
// redialling after a half-open connection gets in at once, without waiting
// for the vehicle to notice the dead peer. KEEL is a single orchestrator
// (design section 10), so two adapters on one vehicle is not a case to
// arbitrate.
type Server struct {
	mux          *http.ServeMux
	writeTimeout time.Duration
	buffer       int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	vehicles map[vector.VectorID]*Vehicle
}

var _ http.Handler = (*Server)(nil)

// NewServer returns a Server with no vehicle registered.
func NewServer(opts ServerOptions) (*Server, error) {
	s := &Server{
		writeTimeout: cmp.Or(opts.WriteTimeout, DefaultWriteTimeout),
		buffer:       cmp.Or(opts.CommandBuffer, DefaultCommandBuffer),
		vehicles:     map[vector.VectorID]*Vehicle{},
	}
	if s.writeTimeout <= 0 {
		return nil, fmt.Errorf("native: write timeout %v, want positive", s.writeTimeout)
	}
	if s.buffer < 1 {
		return nil, fmt.Errorf("native: command buffer %d, want at least 1", s.buffer)
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("GET "+VectorPath, s.handle)
	return s, nil
}

// Add registers a vehicle: what its hello declares, and the handle the
// simulator or the vehicle drives it through.
//
// The capabilities are checked here only as far as the endpoint needs: an id
// that fits in one path segment, a non-empty set of known command types. The
// adapter holds the rest against conformance case C1 when it reads the hello.
func (s *Server) Add(caps vector.Capabilities, supports []vector.CommandType) (*Vehicle, error) {
	h := Hello{Capabilities: caps, Supports: supports}.normalised()
	id := h.Capabilities.ID
	switch {
	case id == "" || strings.Contains(string(id), "/"):
		return nil, fmt.Errorf("native: vector id %q, want a non-empty path segment", id)
	case len(h.Supports) == 0:
		return nil, fmt.Errorf("native: vector %s supports no command type", id)
	}
	for _, t := range h.Supports {
		if !slices.Contains(vector.CommandTypes(), t) {
			return nil, fmt.Errorf("native: vector %s supports unknown command type %q", id, t)
		}
	}
	if _, err := encode(TypeHello, h); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, vector.ErrClosed
	}
	if _, dup := s.vehicles[id]; dup {
		return nil, fmt.Errorf("native: vector %s already registered", id)
	}
	v := &Vehicle{
		server:   s,
		hello:    h,
		commands: make(chan vector.Command, s.buffer),
		notify:   make(chan struct{}, 1),
	}
	s.vehicles[id] = v
	return v, nil
}

// ServeHTTP routes VectorPath to its vehicle. Any other path is 404.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Close drops every connection, waits for their goroutines, and closes every
// vehicle's command channel. Calling it again returns nil.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	vehicles := make([]*Vehicle, 0, len(s.vehicles))
	for _, v := range s.vehicles {
		vehicles = append(vehicles, v)
	}
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
	for _, v := range vehicles {
		v.close()
	}
	return nil
}

// handle upgrades a request for one vehicle and serves the connection until
// it ends. The http.Server's handler goroutine is the connection's reader.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	id := vector.VectorID(r.PathValue("id"))
	s.mu.Lock()
	v, ok := s.vehicles[id]
	closed := s.closed
	if ok && !closed {
		s.wg.Add(1)
	}
	s.mu.Unlock()
	switch {
	case closed:
		http.Error(w, "vehicle server closed", http.StatusServiceUnavailable)
		return
	case !ok:
		http.Error(w, fmt.Sprintf("no vector %q", id), http.StatusNotFound)
		return
	}
	defer s.wg.Done()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
	if err != nil {
		// Accept has written the HTTP error.
		return
	}
	// The empty subprotocol is always negotiable: refuse it here.
	if p := conn.Subprotocol(); p != Subprotocol {
		err := fmt.Errorf("%w: subprotocol %q negotiated, want %s", ErrProtocol, p, Subprotocol)
		_ = conn.Close(websocket.StatusProtocolError, closeReason(err))
		return
	}
	conn.SetReadLimit(MaxMessageBytes)
	v.serve(s.ctx, conn)
}

// Vehicle is one registered vehicle: the frames it publishes and the commands
// it receives, whichever connection is current. Its methods are safe for
// concurrent use.
type Vehicle struct {
	server   *Server
	hello    Hello
	commands chan vector.Command

	// frame is the single-slot telemetry mailbox, latest wins, holding the
	// encoded frame. Telemetry is state: a frame not yet written is
	// superseded by the next, and the latest one published while no adapter
	// was connected is the first sent after the next hello.
	mu      sync.Mutex
	frame   []byte
	notify  chan struct{}
	current *websocket.Conn
	closed  bool
	dropped uint64
}

// ID is the vehicle's id.
func (v *Vehicle) ID() vector.VectorID { return v.hello.Capabilities.ID }

// Commands returns the channel received commands are delivered on, until the
// Server closes. It never blocks the connection: when it is full the oldest
// command is dropped for the newest, which supersedes it by Seq anyway.
// Commands are delivered as received, re-sends included; applying them
// idempotently by Seq is the vehicle's part of the contract (spec section 7.2).
func (v *Vehicle) Commands() <-chan vector.Command { return v.commands }

// Send publishes a frame to the connected adapter, if any. It never waits on
// the network. It refuses a frame for another vehicle, one that cannot be
// encoded (a value that is not finite), and any frame after the Server
// closes. The rest of conformance case C2 is the adapter's to hold.
func (v *Vehicle) Send(s vector.VectorState) error {
	if s.ID != v.ID() {
		return fmt.Errorf("%w: id %q, this vehicle is %q", vector.ErrInvalidFrame, s.ID, v.ID())
	}
	b, err := encode(TypeTelemetry, s)
	if err != nil {
		return fmt.Errorf("%w: %v", vector.ErrInvalidFrame, err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return vector.ErrClosed
	}
	v.frame = b
	select {
	case v.notify <- struct{}{}:
	default:
	}
	return nil
}

// Connected reports whether an adapter is connected.
func (v *Vehicle) Connected() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.current != nil
}

// Disconnect drops the current connection without a close handshake, as a
// link failure would. The adapter redials; nothing else changes.
func (v *Vehicle) Disconnect() {
	v.mu.Lock()
	conn := v.current
	v.mu.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
}

// Dropped is the number of commands dropped because the command channel was
// full.
func (v *Vehicle) Dropped() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.dropped
}

// serve runs one connection: hello, then the writer in its own goroutine and
// the reader in this one. A newer connection replaces this one by closing it.
func (v *Vehicle) serve(ctx context.Context, conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() { _ = conn.CloseNow() }()

	hello, err := encode(TypeHello, v.hello)
	if err != nil {
		return
	}
	wctx, wcancel := context.WithTimeout(ctx, v.server.writeTimeout)
	err = conn.Write(wctx, websocket.MessageText, hello)
	wcancel()
	if err != nil {
		return
	}

	v.mu.Lock()
	previous := v.current
	v.current = conn
	v.mu.Unlock()
	defer func() {
		v.mu.Lock()
		if v.current == conn {
			v.current = nil
		}
		v.mu.Unlock()
	}()
	if previous != nil {
		// The handshake waits on a peer that is likely dead, so it runs on
		// its own. Its reader is still blocked on that connection, and
		// Server.Close cancelling that read cuts the handshake short.
		v.server.wg.Go(func() {
			_ = previous.Close(websocket.StatusGoingAway, "replaced by a newer connection")
		})
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		v.write(ctx, conn)
	}()
	v.read(ctx, conn)
	cancel()
	<-done
}

// read delivers commands until the connection ends or a message breaks the
// protocol, which closes it with a status naming the violation.
func (v *Vehicle) read(ctx context.Context, conn *websocket.Conn) {
	for {
		typ, b, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			_ = violation(conn, websocket.StatusUnsupportedData, fmt.Errorf("%w: binary message, want JSON text", ErrProtocol))
			return
		}
		e, err := decode(b)
		if err == nil && e.Type != TypeCommand {
			err = unexpected(e, TypeCommand)
		}
		if err != nil {
			_ = violation(conn, websocket.StatusProtocolError, err)
			return
		}
		c, err := decodeData[vector.Command](e)
		if err != nil {
			_ = violation(conn, websocket.StatusProtocolError, err)
			return
		}
		if err := v.check(c); err != nil {
			_ = violation(conn, websocket.StatusPolicyViolation, err)
			return
		}
		v.deliver(c)
	}
}

// check refuses a command the adapter should never have sent: one addressed
// to another vehicle, or of a type this vehicle did not declare.
func (v *Vehicle) check(c vector.Command) error {
	if c.Vector != v.ID() {
		return fmt.Errorf("%w: command addressed to %q, this vehicle is %q", vector.ErrInvalidCommand, c.Vector, v.ID())
	}
	if _, ok := slices.BinarySearch(v.hello.Supports, c.Type); !ok {
		return fmt.Errorf("%w: %q was not declared in hello", vector.ErrUnsupported, c.Type)
	}
	return nil
}

// deliver puts a command on the channel, dropping the oldest when it is full.
func (v *Vehicle) deliver(c vector.Command) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return
	}
	for {
		select {
		case v.commands <- c:
			return
		default:
		}
		// Only deliver sends, under the lock, so after one command is taken
		// the next send has room.
		select {
		case <-v.commands:
			v.dropped++
		default:
		}
	}
}

// write sends the latest frame whenever there is one, until the connection
// ends. A failed write puts the frame back, unless a newer one has arrived,
// and closes the connection.
func (v *Vehicle) write(ctx context.Context, conn *websocket.Conn) {
	for {
		b, ok := v.take()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-v.notify:
				continue
			}
		}
		wctx, cancel := context.WithTimeout(ctx, v.server.writeTimeout)
		err := conn.Write(wctx, websocket.MessageText, b)
		cancel()
		if err != nil {
			v.putBack(b)
			_ = conn.CloseNow()
			return
		}
	}
}

func (v *Vehicle) take() ([]byte, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	b := v.frame
	v.frame = nil
	return b, b != nil
}

// putBack returns a frame whose write failed to the mailbox, unless a newer
// one has arrived since, and wakes whichever writer serves the next
// connection.
func (v *Vehicle) putBack(b []byte) {
	v.mu.Lock()
	if v.frame == nil && !v.closed {
		v.frame = b
	}
	v.mu.Unlock()
	select {
	case v.notify <- struct{}{}:
	default:
	}
}

// close ends the vehicle once the Server's goroutines are gone.
func (v *Vehicle) close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.closed {
		v.closed = true
		v.frame = nil
		close(v.commands)
	}
}
