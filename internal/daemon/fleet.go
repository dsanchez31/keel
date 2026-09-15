package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/bluenviron/gomavlib/v4"

	"github.com/dsanchez31/keel/adapters/mavlink"
	"github.com/dsanchez31/keel/adapters/native"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/httpjson"
	"github.com/dsanchez31/keel/internal/pacer"
	"github.com/dsanchez31/keel/internal/planir"
	"github.com/dsanchez31/keel/vector"
)

// faultTimeout bounds one fault forwarded to a simulator.
const faultTimeout = 5 * time.Second

// clockTimeout bounds one simulator's change of pace.
const clockTimeout = 5 * time.Second

// The refusals of Fleet.InjectFault.
var (
	// ErrUnbound: the vector has no binding.
	ErrUnbound = errors.New("daemon: vector not bound")
	// ErrNotSimulated: the vector is bound to no simulator, so nothing can
	// make it undergo a fault (spec section 8.1).
	ErrNotSimulated = errors.New("daemon: vector not simulated")
	// ErrFaultRefused: the simulator refused the fault.
	ErrFaultRefused = errors.New("daemon: fault refused by the simulator")
	// ErrSimulator: the simulator could not be reached, or failed, or did not
	// take the pace it was given.
	ErrSimulator = errors.New("daemon: simulator unavailable")
)

// FleetOptions tunes a Fleet. The zero value is the defaults.
type FleetOptions struct {
	// Agent tunes every adapter's vector.Agent: health thresholds, telemetry
	// buffer, clock. Its Supports must be nil. keeld hands it its pacer's
	// scaled clock, so the thresholds count in the fleet's time at any pace.
	Agent vector.Options
	// Logger receives what an operator would want to know and no endpoint
	// answers: a dial failing, a command an adapter refused. Nil discards it.
	Logger *slog.Logger
	// HTTPClient forwards faults and paces. Nil means a client with no
	// timeout of its own: each request is bounded by its context and
	// faultTimeout or clockTimeout.
	HTTPClient *http.Client
}

// Fleet is every bound vector, each through its adapter: what the daemon
// drains into events at each tick and dispatches commands to.
//
// A native vehicle is dialled in the background until it answers, since the
// simulator serving it may start after keeld; its adapter then redials on its
// own. The MAVLink vehicles share one link and are served from the start, each
// adapter publishing once its autopilot is heard.
type Fleet struct {
	log    *slog.Logger
	client *http.Client
	link   *mavlink.Link

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// members are sorted by id.
	members []*member

	// clockMu serialises the simulators' pace: a change, and the pace pushed
	// again to a simulator that reconnected.
	clockMu sync.Mutex
	// speed is the simulators' pace, 1 until SetSimSpeed.
	speed int
}

// simSpeeder is an adapter whose vehicle's simulation keeld paces: a SITL
// behind the MAVLink adapter.
type simSpeeder interface {
	SetSimSpeed(ctx context.Context, speed float64) error
}

// member is one bound vector.
type member struct {
	binding Binding

	mu sync.Mutex
	// v is nil until a native vehicle's first connection.
	v      vector.Vector
	dialed string // the last dial failure, until connected
	// frame is the latest frame since the last Drain.
	frame *domain.VectorState
	// link is the verdict of the last Drain.
	link domain.LinkState
}

// Report is what the fleet knows of one connected vector at a Drain.
type Report struct {
	Vector domain.VectorID
	// Caps is what the vector declares.
	Caps domain.Capabilities
	// Frame is the latest frame since the last Drain, nil when none came.
	// Frames between two ticks are state, latest wins, as on the adapter's
	// channel.
	Frame *domain.VectorState
	// Link is the adapter's verdict now, and LinkChanged whether it differs
	// from the previous Drain's.
	Link        domain.LinkState
	LinkChanged bool
}

// NewFleet binds the configured vectors, taking the capabilities of a
// MAVLink vehicle from its entry in the world's fleet, and starts serving
// them. Close stops it.
func NewFleet(cfg *Config, w *planir.World, opts FleetOptions) (*Fleet, error) {
	if err := cfg.CheckFleet(w); err != nil {
		return nil, err
	}
	f := &Fleet{log: opts.Logger, client: opts.HTTPClient, speed: 1}
	if f.log == nil {
		f.log = slog.New(slog.DiscardHandler)
	}
	if f.client == nil {
		f.client = &http.Client{}
	}
	f.ctx, f.cancel = context.WithCancel(context.Background())

	var fail error
	for _, b := range cfg.Vectors {
		m := &member{binding: b, link: domain.LinkLost}
		f.members = append(f.members, m)
		if b.MAVLink == nil {
			continue
		}
		if f.link == nil {
			if f.link, fail = mavlink.NewLink(mavlink.LinkOptions{
				Endpoints: []gomavlib.Endpoint{&gomavlib.EndpointUDPServer{Address: cfg.MAVLink.Listen}},
			}); fail != nil {
				fail = fmt.Errorf("daemon: mavlink link on %s: %w", cfg.MAVLink.Listen, fail)
				break
			}
		}
		fv, _ := fleetVector(w, b.ID)
		a, err := mavlink.NewAdapter(f.link, mavlink.Config{
			SystemID:         b.MAVLink.SystemID,
			Capabilities:     fv.Caps,
			GeoidSeparationM: cfg.MAVLink.GeoidSeparationM,
			Agent:            opts.Agent,
			SITL:             b.MAVLink.SITL,
		})
		if err != nil {
			fail = fmt.Errorf("daemon: %s: %w", b.ID, err)
			break
		}
		m.v = a
	}
	if fail != nil {
		_ = f.Close()
		return nil, fail
	}
	for _, m := range f.members {
		f.wg.Go(func() { f.serve(m, opts.Agent) })
	}
	return f, nil
}

// serve dials a native member until it connects, then pumps its telemetry
// into the member until the adapter closes.
func (f *Fleet) serve(m *member, agent vector.Options) {
	v := m.vector()
	for attempt := 0; v == nil; attempt++ {
		a, err := native.Dial(f.ctx, m.binding.Native.URL, native.Options{Agent: agent})
		if err == nil && a.Describe().ID != m.binding.ID {
			// A misrouted vehicle would be flown under another's name.
			err = fmt.Errorf("its hello declares %s", a.Describe().ID)
			_ = a.Close()
		}
		if err != nil {
			if f.ctx.Err() != nil {
				return
			}
			m.mu.Lock()
			first := m.dialed == ""
			m.dialed = err.Error()
			m.mu.Unlock()
			if first {
				f.log.Warn("vector not reachable yet, redialling", "vector", m.binding.ID, "url", m.binding.Native.URL, "error", err)
			}
			if !f.sleep(backoff(attempt)) {
				return
			}
			continue
		}
		m.mu.Lock()
		if f.ctx.Err() != nil {
			m.mu.Unlock()
			_ = a.Close()
			return
		}
		m.v, m.dialed = a, ""
		m.mu.Unlock()
		f.log.Info("vector connected", "vector", m.binding.ID, "url", m.binding.Native.URL)
		v = a
	}
	for s := range v.Telemetry() {
		m.mu.Lock()
		m.frame = &s
		m.mu.Unlock()
	}
}

// backoff is the wait before a dial attempt: exponential from
// native.DefaultMinBackoff to native.DefaultMaxBackoff with full jitter, as
// the adapter's own reconnection, so a fleet restarting together does not
// dial in lockstep.
func backoff(attempt int) time.Duration {
	d := native.DefaultMinBackoff << min(attempt, 8)
	return rand.N(min(d, native.DefaultMaxBackoff)) + 1
}

func (f *Fleet) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-f.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (m *member) vector() vector.Vector {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.v
}

// Drain reports every connected vector, in id order: its capabilities, the
// latest frame since the last Drain, and its adapter's verdict on the link.
func (f *Fleet) Drain() []Report {
	var out []Report
	for _, m := range f.members {
		m.mu.Lock()
		v, frame := m.v, m.frame
		m.frame = nil
		m.mu.Unlock()
		if v == nil {
			continue
		}
		link := domain.LinkState(v.Health().Kind)
		r := Report{Vector: m.binding.ID, Caps: v.Describe(), Frame: frame, Link: link}
		m.mu.Lock()
		r.LinkChanged, m.link = m.link != link, link
		m.mu.Unlock()
		if r.LinkChanged && link == domain.LinkOK && m.binding.Native != nil && m.binding.Native.Clock != "" {
			// A vehicle heard again may be a simulator that restarted, in
			// real time: it is brought back to the fleet's pace. A SITL's
			// adapter does the same on its own.
			url := m.binding.Native.Clock
			f.wg.Go(func() { f.repace(url) })
		}
		out = append(out, r)
	}
	return out
}

// SetSimSpeed brings every simulator of the fleet to a pace, each keelsim
// clock endpoint once then each SITL, and returns once each has taken it. A
// simulator failing brings back the ones already changed, itself included,
// to the previous pace, and its error is returned.
func (f *Fleet) SetSimSpeed(ctx context.Context, speed int) error {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	from := f.speed
	clocks := f.simClocks()
	for i, c := range clocks {
		cctx, cancel := context.WithTimeout(ctx, clockTimeout)
		err := f.setClock(cctx, c, speed)
		cancel()
		if err != nil {
			for _, back := range clocks[:i+1] {
				f.rollback(back, from)
			}
			return fmt.Errorf("%w: %s: %v", ErrSimulator, c.name, err)
		}
	}
	f.speed = speed
	return nil
}

// simClock is one simulator keeld paces: a clock endpoint, or a SITL.
type simClock struct {
	name string
	url  string
	sitl simSpeeder
}

// simClocks lists the fleet's simulators, each clock endpoint once in the
// order of its first vector, then each SITL by id.
func (f *Fleet) simClocks() []simClock {
	var out []simClock
	seen := map[string]bool{}
	for _, m := range f.members {
		if n := m.binding.Native; n != nil && n.Clock != "" && !seen[n.Clock] {
			seen[n.Clock] = true
			out = append(out, simClock{name: n.Clock, url: n.Clock})
		}
	}
	for _, m := range f.members {
		if b := m.binding.MAVLink; b != nil && b.SITL {
			if s, ok := m.vector().(simSpeeder); ok {
				out = append(out, simClock{name: "SITL " + string(m.binding.ID), sitl: s})
			}
		}
	}
	return out
}

func (f *Fleet) setClock(ctx context.Context, c simClock, speed int) error {
	if c.sitl != nil {
		return c.sitl.SetSimSpeed(ctx, float64(speed))
	}
	return f.putClock(ctx, c.url, speed)
}

// rollback brings a simulator back to a pace, as far as it can. A SITL
// records it without waiting, set on its next contact if not now.
func (f *Fleet) rollback(c simClock, speed int) {
	var err error
	if c.sitl != nil {
		done, cancel := context.WithCancel(context.Background())
		cancel()
		_ = c.sitl.SetSimSpeed(done, float64(speed))
	} else {
		ctx, cancel := context.WithTimeout(f.ctx, clockTimeout)
		err = f.putClock(ctx, c.url, speed)
		cancel()
	}
	if err != nil {
		f.log.Error("simulator not brought back to its pace", "simulator", c.name, "speed", speed, "error", err)
	}
}

// repace puts the fleet's pace to a clock endpoint again.
func (f *Fleet) repace(url string) {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	ctx, cancel := context.WithTimeout(f.ctx, clockTimeout)
	defer cancel()
	if err := f.putClock(ctx, url, f.speed); err != nil && f.ctx.Err() == nil {
		f.log.Warn("simulator not brought to the fleet's pace", "clock", url, "speed", f.speed, "error", err)
	}
}

// putClock puts a pace to a keelsim clock endpoint (spec section 15.4).
func (f *Fleet) putClock(ctx context.Context, url string, speed int) error {
	body, err := eventlog.Canonical(pacer.Setting{Speed: speed})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("answered %s: %s", resp.Status, problemDetail(resp.Body))
	}
	return nil
}

// Health is every bound vector's health, by id: the adapter's once
// connected, the last dial failure before.
func (f *Fleet) Health() map[domain.VectorID]domain.HealthStatus {
	out := make(map[domain.VectorID]domain.HealthStatus, len(f.members))
	for _, m := range f.members {
		m.mu.Lock()
		v, dialed := m.v, m.dialed
		m.mu.Unlock()
		if v != nil {
			out[m.binding.ID] = v.Health()
			continue
		}
		detail := "not connected yet"
		if dialed != "" {
			detail += ": " + dialed
		}
		out[m.binding.ID] = domain.HealthStatus{Kind: domain.HealthLost, Detail: detail}
	}
	return out
}

// Dispatch hands each command to its vector's adapter, which never blocks. A
// command for a vector not connected yet is dropped: the engine re-sends a
// command it sees unacknowledged (spec section 7.2).
func (f *Fleet) Dispatch(cmds []domain.Command) {
	for _, c := range cmds {
		m := f.member(c.Vector)
		if m == nil {
			f.log.Error("command for an unbound vector", "vector", c.Vector, "seq", c.Seq)
			continue
		}
		v := m.vector()
		if v == nil {
			continue
		}
		if err := v.Execute(c); err != nil {
			f.log.Warn("command refused by the adapter", "vector", c.Vector, "seq", c.Seq, "type", c.Type, "error", err)
		}
	}
}

// InjectFault forwards a fault to the simulator serving its vector, and
// returns once the simulator has accepted it. The caller records the fault
// only then: the log never claims a fault the vehicle did not undergo.
func (f *Fleet) InjectFault(ctx context.Context, fault domain.Fault) error {
	m := f.member(fault.Vector)
	switch {
	case m == nil:
		return fmt.Errorf("%w: %s", ErrUnbound, fault.Vector)
	case !m.binding.Simulated():
		return fmt.Errorf("%w: %s is not bound to a simulator, and faults are simulated", ErrNotSimulated, fault.Vector)
	}
	body, err := eventlog.Canonical(fault)
	if err != nil {
		return fmt.Errorf("encoding the fault: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, faultTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.binding.Native.Faults, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSimulator, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSimulator, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusAccepted {
		return nil
	}
	detail := problemDetail(resp.Body)
	if resp.StatusCode == http.StatusUnprocessableEntity {
		return fmt.Errorf("%w: %s", ErrFaultRefused, detail)
	}
	return fmt.Errorf("%w: %s answered %s: %s", ErrSimulator, m.binding.Native.Faults, resp.Status, detail)
}

// problemDetail reads the detail of a problem answer, or says there is none.
func problemDetail(r io.Reader) string {
	var p httpjson.Problem
	if err := json.NewDecoder(io.LimitReader(r, httpjson.MaxBodyBytes)).Decode(&p); err != nil || p.Detail == "" {
		return "no detail"
	}
	return p.Detail
}

func (f *Fleet) member(id domain.VectorID) *member {
	for _, m := range f.members {
		if m.binding.ID == id {
			return m
		}
	}
	return nil
}

// Close stops dialling, closes every adapter, then the MAVLink link, and
// waits for the fleet's goroutines.
func (f *Fleet) Close() error {
	f.cancel()
	var errs []error
	for _, m := range f.members {
		m.mu.Lock()
		v := m.v
		m.mu.Unlock()
		if v != nil {
			errs = append(errs, v.Close())
		}
	}
	if f.link != nil {
		errs = append(errs, f.link.Close())
	}
	f.wg.Wait()
	return errors.Join(errs...)
}
