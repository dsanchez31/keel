package native

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/dsanchez31/keel/vector"
)

// Defaults of the adapter's Options.
const (
	// DefaultHelloTimeout bounds a connection attempt: the handshake and the
	// wait for the vehicle's hello.
	DefaultHelloTimeout = 5 * time.Second
	// DefaultWriteTimeout bounds one command write.
	DefaultWriteTimeout = 5 * time.Second
	// DefaultMinBackoff and DefaultMaxBackoff bound the wait before each
	// reconnect attempt: exponential from the minimum, capped at the maximum,
	// with full jitter. Well inside the 30 s conformance case C7 allows.
	DefaultMinBackoff = 250 * time.Millisecond
	DefaultMaxBackoff = 5 * time.Second
)

// Options tunes an Adapter. The zero value is the defaults.
type Options struct {
	// Agent tunes the vector.Agent behind the adapter: health thresholds,
	// telemetry buffer, clock. Its Supports must be nil: a native vehicle
	// declares the command types it executes in its hello.
	Agent vector.Options
	// IdleTimeout is how long a connection may carry no message before the
	// adapter drops it and redials: a half-open connection is otherwise never
	// noticed. Zero means the Agent's LostAfter, so a silent link is redialled
	// when Health reports it lost.
	IdleTimeout time.Duration
	// HelloTimeout, WriteTimeout, MinBackoff and MaxBackoff: zero means the
	// defaults above.
	HelloTimeout time.Duration
	WriteTimeout time.Duration
	MinBackoff   time.Duration
	MaxBackoff   time.Duration
	// HTTPClient and HTTPHeader are passed to the WebSocket handshake. Nil
	// means http.DefaultClient and no extra header.
	HTTPClient *http.Client
	HTTPHeader http.Header
}

type config struct {
	idle, hello, write     time.Duration
	minBackoff, maxBackoff time.Duration
	client                 *http.Client
	header                 http.Header
}

func newConfig(opts Options) (config, error) {
	if opts.Agent.Supports != nil {
		return config{}, errors.New("native: Options.Agent.Supports is set, the vehicle declares its command types in hello")
	}
	c := config{
		idle:       cmp.Or(opts.IdleTimeout, opts.Agent.LostAfter, vector.DefaultLostAfter),
		hello:      cmp.Or(opts.HelloTimeout, DefaultHelloTimeout),
		write:      cmp.Or(opts.WriteTimeout, DefaultWriteTimeout),
		minBackoff: cmp.Or(opts.MinBackoff, DefaultMinBackoff),
		maxBackoff: cmp.Or(opts.MaxBackoff, DefaultMaxBackoff),
		client:     opts.HTTPClient,
		header:     opts.HTTPHeader,
	}
	switch {
	case c.idle <= 0 || c.hello <= 0 || c.write <= 0:
		return config{}, fmt.Errorf("native: timeouts idle %v, hello %v, write %v: want positive", c.idle, c.hello, c.write)
	case c.minBackoff <= 0 || c.maxBackoff < c.minBackoff:
		return config{}, fmt.Errorf("native: backoff %v to %v: want 0 < min <= max", c.minBackoff, c.maxBackoff)
	}
	// The Agent is the one authority on its own options. Probing them with
	// capabilities known to be valid reports a bad option as the caller's
	// before any connection, rather than blaming it on the vehicle's hello.
	probe := vector.Capabilities{ID: "probe", Domain: vector.DomainAerial, CruiseSpeed: 1, MaxRangeM: 1}
	if _, err := vector.NewAgent(probe, func(vector.Command) error { return nil }, opts.Agent); err != nil {
		return config{}, fmt.Errorf("native: Options.Agent: %w", err)
	}
	return c, nil
}

// Adapter is the orchestrator's end of one native connection: a
// vector.Vector over a WebSocket to one vehicle.
//
// It is a transport plus a vector.Agent (design section 7.2). The Agent holds
// the contract's mechanics: the telemetry channel, the Seq filter, the
// declared command types, the cached health. The adapter holds the link: it
// feeds every frame to the Agent, keeps the latest command for the writer,
// and redials when the connection drops. The Agent is wrapped rather than
// embedded, so its Publish and Disconnected stay the adapter's to call.
//
// Two goroutines, both stopped by Close: the connection loop, which is also
// the current connection's reader, and that connection's writer.
type Adapter struct {
	agent *vector.Agent
	url   string
	cfg   config
	// hello is the first connection's, normalised. Every reconnect must
	// declare the same.
	hello Hello

	// pending is the single-slot command mailbox, latest wins. A newer
	// command replaces one not yet written: Seq supersedes, and the vehicle
	// would drop the older as superseded anyway. It survives a disconnect,
	// so the command standing when the link dropped leaves on the next one.
	mu      sync.Mutex
	pending *vector.Command
	notify  chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

var _ vector.Vector = (*Adapter)(nil)

// Dial connects to the vehicle at url, a ws:// or wss:// URL naming one
// vector (spec section 7.3), and waits for its hello. The hello fixes what
// Describe and the declared command types report for the adapter's lifetime.
//
// ctx bounds this first connection only: one attempt, no retry, so a caller
// learns at once that the vehicle is not there. Once Dial returns, the
// adapter lives until Close and redials on its own.
func Dial(ctx context.Context, url string, opts Options) (*Adapter, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	conn, hello, err := connect(ctx, url, cfg)
	if err != nil {
		return nil, err
	}
	a := &Adapter{url: url, cfg: cfg, hello: hello, notify: make(chan struct{}, 1), done: make(chan struct{})}
	agentOpts := opts.Agent
	agentOpts.Supports = hello.Supports
	a.agent, err = vector.NewAgent(hello.Capabilities, a.transmit, agentOpts)
	if err != nil {
		err = fmt.Errorf("%w: hello: %v", ErrProtocol, err)
		_ = conn.Close(websocket.StatusPolicyViolation, closeReason(err))
		return nil, err
	}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	go a.run(conn)
	return a, nil
}

// Describe returns the capabilities the vehicle declared in its hello.
func (a *Adapter) Describe() vector.Capabilities { return a.agent.Describe() }

// Supports returns the command types the vehicle declared, sorted.
func (a *Adapter) Supports() []vector.CommandType { return a.agent.Supports() }

// Telemetry returns the channel frames are delivered on. It lives until
// Close, across reconnects.
func (a *Adapter) Telemetry() <-chan vector.VectorState { return a.agent.Telemetry() }

// Execute validates a command, filters it by Seq and puts it in the mailbox.
// It never waits on the network: the command leaves when the writer takes it.
func (a *Adapter) Execute(c vector.Command) error { return a.agent.Execute(c) }

// Health is the Agent's cached view. After a protocol violation or a refused
// frame it reports lost with the reason, until the next accepted frame.
func (a *Adapter) Health() vector.HealthStatus { return a.agent.Health() }

// Close drops the connection, stops every goroutine the adapter started and
// closes the telemetry channel. Calling it again returns nil.
func (a *Adapter) Close() error {
	a.cancel()
	<-a.done
	return a.agent.Close()
}

// transmit is the Agent's Transmit: it fills the mailbox and wakes the
// writer, without blocking.
func (a *Adapter) transmit(c vector.Command) error {
	a.mu.Lock()
	a.pending = &c
	a.mu.Unlock()
	select {
	case a.notify <- struct{}{}:
	default:
	}
	return nil
}

// take empties the mailbox.
func (a *Adapter) take() (vector.Command, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending == nil {
		return vector.Command{}, false
	}
	c := *a.pending
	a.pending = nil
	return c, true
}

// putBack returns a command whose write failed to the mailbox, unless a newer
// one has arrived since, and wakes whichever writer serves the next
// connection. Commands are idempotent by Seq, so a copy that did leave before
// the failure is harmless.
func (a *Adapter) putBack(c vector.Command) {
	a.mu.Lock()
	if a.pending == nil {
		a.pending = &c
	}
	a.mu.Unlock()
	select {
	case a.notify <- struct{}{}:
	default:
	}
}

// run is the connection loop: serve a connection until it ends, report the
// link down with the reason, redial, until Close.
func (a *Adapter) run(conn *websocket.Conn) {
	defer close(a.done)
	for conn != nil {
		reason := a.serve(conn)
		if a.ctx.Err() != nil {
			return
		}
		a.agent.Disconnected(reason)
		conn = a.reconnect()
	}
}

// reconnect redials with exponential backoff and full jitter until a
// connection declares the first hello, or Close. It returns nil on Close.
func (a *Adapter) reconnect() *websocket.Conn {
	for attempt := 0; ; attempt++ {
		if !a.sleep(a.backoff(attempt)) {
			return nil
		}
		conn, hello, err := connect(a.ctx, a.url, a.cfg)
		if err == nil && !hello.equal(a.hello) {
			err = fmt.Errorf("%w: hello differs from the one the adapter was dialled with", ErrProtocol)
			_ = conn.Close(websocket.StatusPolicyViolation, closeReason(err))
		}
		if err == nil {
			return conn
		}
		if a.ctx.Err() != nil {
			return nil
		}
		a.agent.Disconnected(err.Error())
	}
}

// backoff is the wait before a reconnect attempt: uniform in [0, d), d
// doubling from the minimum to the maximum ("full jitter"), so a fleet of
// adapters dropped at once does not redial in lockstep.
func (a *Adapter) backoff(attempt int) time.Duration {
	d := a.cfg.minBackoff
	for range attempt {
		if d > a.cfg.maxBackoff/2 {
			d = a.cfg.maxBackoff
			break
		}
		d *= 2
	}
	return rand.N(min(d, a.cfg.maxBackoff)) + 1
}

// sleep waits d, returning false if Close came first.
func (a *Adapter) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-a.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// serve runs one connection: the writer in its own goroutine, the reader in
// this one. It returns why the connection ended once both have stopped.
func (a *Adapter) serve(conn *websocket.Conn) string {
	ctx, cancel := context.WithCancel(a.ctx)
	werr := make(chan error, 1)
	go func() { werr <- a.write(ctx, conn) }()
	reason, failed := a.read(ctx, conn)
	cancel()
	_ = conn.CloseNow()
	if err := <-werr; err != nil && failed {
		// A failed write closes the connection, and the reader then reports
		// only that it was closed: the write is the cause. When the reader
		// ended the connection itself, a write failing on the way out is not.
		reason = err.Error()
	}
	return reason
}

// read delivers telemetry to the Agent until the connection fails, a message
// breaks the protocol, a frame breaks C2, or no message arrives for the idle
// timeout. A violation closes the connection with a status and the reason,
// so the vehicle is told, and the reason becomes the adapter's Health. failed
// reports a connection that failed under the reader rather than one the
// reader ended.
func (a *Adapter) read(ctx context.Context, conn *websocket.Conn) (reason string, failed bool) {
	for {
		rctx, cancel := context.WithTimeout(ctx, a.cfg.idle)
		typ, b, err := conn.Read(rctx)
		idle := errors.Is(rctx.Err(), context.DeadlineExceeded)
		cancel()
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return "closed", false
		case idle:
			return fmt.Sprintf("no message for %v", a.cfg.idle), false
		default:
			return readFailure(err), true
		}
		if typ != websocket.MessageText {
			return violation(conn, websocket.StatusUnsupportedData, fmt.Errorf("%w: binary message, want JSON text", ErrProtocol)), false
		}
		e, err := decode(b)
		if err == nil && e.Type != TypeTelemetry {
			err = unexpected(e, TypeTelemetry)
		}
		if err != nil {
			return violation(conn, websocket.StatusProtocolError, err), false
		}
		s, err := decodeData[vector.VectorState](e)
		if err != nil {
			return violation(conn, websocket.StatusProtocolError, err), false
		}
		if err := a.agent.Publish(s); err != nil {
			if errors.Is(err, vector.ErrClosed) {
				return "closed", false
			}
			return violation(conn, websocket.StatusPolicyViolation, fmt.Errorf("telemetry refused: %w", err)), false
		}
	}
}

// write sends the mailbox's command whenever there is one, until the
// connection ends. It returns an error only for a failed write, after putting
// the command back for the next connection.
func (a *Adapter) write(ctx context.Context, conn *websocket.Conn) error {
	for {
		c, ok := a.take()
		if !ok {
			select {
			case <-ctx.Done():
				return nil
			case <-a.notify:
				continue
			}
		}
		b, err := encode(TypeCommand, c)
		if err != nil {
			// The Agent refused every command that could fail to encode, a
			// non-finite waypoint included, so this is a bug, not a link
			// failure: drop the command rather than retry it forever.
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, a.cfg.write)
		err = conn.Write(wctx, websocket.MessageText, b)
		cancel()
		if err != nil {
			a.putBack(c)
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("writing command seq %d: %w", c.Seq, err)
		}
	}
}

// connect opens one connection and reads the vehicle's hello, within the
// hello timeout.
func connect(ctx context.Context, url string, cfg config) (*websocket.Conn, Hello, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.hello)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient:   cfg.client,
		HTTPHeader:   cfg.header,
		Subprotocols: []string{Subprotocol},
	})
	if err != nil {
		return nil, Hello{}, fmt.Errorf("dialling %s: %w", url, err)
	}
	// The handshake accepts a server that negotiated no subprotocol at all.
	if p := conn.Subprotocol(); p != Subprotocol {
		err := fmt.Errorf("%w: subprotocol %q negotiated, want %s", ErrProtocol, p, Subprotocol)
		_ = conn.Close(websocket.StatusProtocolError, closeReason(err))
		return nil, Hello{}, err
	}
	conn.SetReadLimit(MaxMessageBytes)
	typ, b, err := conn.Read(ctx)
	if err != nil {
		_ = conn.CloseNow()
		return nil, Hello{}, fmt.Errorf("waiting for hello: %s", readFailure(err))
	}
	if typ != websocket.MessageText {
		err := fmt.Errorf("%w: binary message, want JSON text", ErrProtocol)
		_ = conn.Close(websocket.StatusUnsupportedData, closeReason(err))
		return nil, Hello{}, err
	}
	e, err := decode(b)
	if err == nil && e.Type != TypeHello {
		err = unexpected(e, TypeHello)
	}
	var hello Hello
	if err == nil {
		hello, err = decodeHello(e)
	}
	if err != nil {
		_ = conn.Close(websocket.StatusProtocolError, closeReason(err))
		return nil, Hello{}, err
	}
	return conn, hello, nil
}

// violation closes the connection with a status naming the error, and
// returns the error as the reason the connection ended.
func violation(conn *websocket.Conn, code websocket.StatusCode, err error) string {
	_ = conn.Close(code, closeReason(err))
	return err.Error()
}

// readFailure describes a failed read: the peer's close status and reason
// when it sent one, the transport's error otherwise.
func readFailure(err error) string {
	if ce, ok := errors.AsType[websocket.CloseError](err); ok {
		if ce.Reason != "" {
			return fmt.Sprintf("peer closed the connection (%d): %s", int(ce.Code), ce.Reason)
		}
		return fmt.Sprintf("peer closed the connection (%d)", int(ce.Code))
	}
	return err.Error()
}
