package vector

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dsanchez31/keel/internal/domain"
)

// Health thresholds by default: the engine's link aging read in wall clock.
// The engine degrades a link after StaleTelemetryTicks without a frame, 50
// ticks or 5 s by default, and loses it at twice that.
const (
	DefaultDegradedAfter = 5 * time.Second
	DefaultLostAfter     = 10 * time.Second
	// DefaultBuffer is the telemetry channel capacity.
	DefaultBuffer = 16
)

// Transmit hands one command to the adapter's transport. It must not block,
// and it must not call back into the Agent: Execute holds the Agent's lock
// while it runs, so that commands leave in the order their Seq was checked.
type Transmit func(Command) error

// Options tunes an Agent. The zero value is the defaults.
type Options struct {
	// Supports is the set of command types the adapter executes. Nil means
	// all of CommandTypes. A type outside it is refused with ErrUnsupported.
	Supports []CommandType
	// DegradedAfter and LostAfter are how long without an accepted frame
	// before Health reports degraded, then lost. Zero means the default.
	DegradedAfter time.Duration
	LostAfter     time.Duration
	// Buffer is the telemetry channel capacity, at least 1. Zero means
	// DefaultBuffer.
	Buffer int
	// Clock reads the wall clock Health ages frames against. Nil means
	// time.Now. Adapters are outside the decision path and may read a clock.
	Clock func() time.Time
}

// Agent implements the mechanics of the Vector contract once, so that every
// adapter is a transport plus an Agent: it owns the telemetry channel, filters
// commands by Seq, refuses what the adapter does not declare, and keeps the
// cached health view.
//
// An adapter embeds *Agent, or wraps it to keep Publish and Disconnected its
// own (adapters/native does), passes it a non-blocking Transmit, and feeds it
// from its transport: Publish for every frame, with AckSeq set to the last
// command Seq the vehicle applied, and Disconnected when the link drops. The
// adapter overrides Close to stop its own goroutines first, then calls
// Agent.Close.
//
// The Agent starts no goroutine. Its methods are safe for concurrent use.
type Agent struct {
	caps          Capabilities
	supports      []CommandType
	transmit      Transmit
	degradedAfter time.Duration
	lostAfter     time.Duration
	clock         func() time.Time

	mu     sync.Mutex
	out    chan VectorState
	closed bool
	// lostReason, when set, is why Health reports lost whatever the age of
	// the last frame: no frame yet, or a disconnect. The next accepted frame
	// clears it.
	lostReason string
	lastFrame  time.Time
	lastSeenMs int64
	lastSent   uint64
	// acked is the AckSeq of the last accepted frame.
	acked   uint64
	dropped uint64
}

var _ Vector = (*Agent)(nil)

// NewAgent returns an Agent for one vehicle. Capabilities are validated
// against conformance case C1 and their tags sorted; an Agent that cannot be
// conformant is refused here rather than at the first command.
func NewAgent(caps Capabilities, transmit Transmit, opts Options) (*Agent, error) {
	caps = caps.Clone()
	caps.SortTags()
	if err := checkCapabilities(caps); err != nil {
		return nil, err
	}
	if transmit == nil {
		return nil, errors.New("vector: nil transmit")
	}
	supports, err := supportedSet(opts.Supports)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		caps:          caps,
		supports:      supports,
		transmit:      transmit,
		degradedAfter: cmp.Or(opts.DegradedAfter, DefaultDegradedAfter),
		lostAfter:     cmp.Or(opts.LostAfter, DefaultLostAfter),
		clock:         opts.Clock,
		lostReason:    "no frame yet",
	}
	if a.degradedAfter < 0 || a.lostAfter <= a.degradedAfter {
		return nil, fmt.Errorf("vector: thresholds degraded %v, lost %v: want 0 <= degraded < lost", a.degradedAfter, a.lostAfter)
	}
	if a.clock == nil {
		a.clock = time.Now
	}
	buffer := cmp.Or(opts.Buffer, DefaultBuffer)
	if buffer < 1 {
		return nil, fmt.Errorf("vector: telemetry buffer %d, want at least 1", buffer)
	}
	a.out = make(chan VectorState, buffer)
	return a, nil
}

func checkCapabilities(c Capabilities) error {
	switch {
	case c.ID == "":
		return errors.New("vector: capabilities without an id")
	case !domain.ValidDomain(c.Domain):
		return fmt.Errorf("vector %s: domain %q, want aerial or ground", c.ID, c.Domain)
	case !positiveFinite(c.CruiseSpeed):
		return fmt.Errorf("vector %s: cruise speed %v, want positive and finite", c.ID, c.CruiseSpeed)
	case !positiveFinite(c.MaxRangeM):
		return fmt.Errorf("vector %s: max range %v, want positive and finite", c.ID, c.MaxRangeM)
	case !finite(c.SensorRadiusM) || c.SensorRadiusM < 0:
		return fmt.Errorf("vector %s: sensor radius %v, want finite and not negative", c.ID, c.SensorRadiusM)
	}
	return nil
}

// supportedSet sorts and deduplicates the declared command types, refusing
// one the contract does not define and a declaration that supports nothing.
func supportedSet(in []CommandType) ([]CommandType, error) {
	if in == nil {
		return CommandTypes(), nil
	}
	known := CommandTypes()
	out := slices.Clone(in)
	slices.Sort(out)
	out = slices.Compact(out)
	for _, t := range out {
		if _, ok := slices.BinarySearch(known, t); !ok {
			return nil, fmt.Errorf("vector: supports unknown command type %q", t)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("vector: supports no command type")
	}
	return out, nil
}

func finite(f float64) bool         { return !math.IsNaN(f) && !math.IsInf(f, 0) }
func positiveFinite(f float64) bool { return finite(f) && f > 0 }

// Describe returns the declared capabilities, tags sorted. The copy keeps a
// caller from editing the Agent's set through the slice.
func (a *Agent) Describe() Capabilities { return a.caps.Clone() }

// Supports returns the declared command types, sorted.
func (a *Agent) Supports() []CommandType { return slices.Clone(a.supports) }

// Telemetry returns the channel frames are delivered on. It lives until
// Close, across disconnects.
func (a *Agent) Telemetry() <-chan VectorState { return a.out }

// Execute validates a command and hands it to Transmit unless it is already
// known to be applied or superseded.
//
// The filter is on what the vehicle acknowledged, not on what was sent. The
// engine re-sends a standing command with its original Seq precisely because
// the first copy may have been lost on the link (spec section 7.2), so a
// re-send of the latest Seq is forwarded until a frame's AckSeq reports it
// applied. A Seq at or below the acknowledged one, or below the latest sent,
// is a no-op and returns nil. A Seq above the latest sent is forwarded
// whatever the gap.
func (a *Agent) Execute(c Command) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrClosed
	}
	if err := a.checkCommand(c); err != nil {
		return err
	}
	if c.Seq <= a.acked || c.Seq < a.lastSent {
		return nil
	}
	if err := a.transmit(c); err != nil {
		return fmt.Errorf("vector %s: transmit %s seq %d: %w", a.caps.ID, c.Type, c.Seq, err)
	}
	a.lastSent = c.Seq
	return nil
}

func (a *Agent) checkCommand(c Command) error {
	if c.Vector != a.caps.ID {
		return fmt.Errorf("%w: addressed to %q, this vector is %q", ErrInvalidCommand, c.Vector, a.caps.ID)
	}
	if _, ok := slices.BinarySearch(a.supports, c.Type); !ok {
		return fmt.Errorf("%w: %q by vector %s", ErrUnsupported, c.Type, a.caps.ID)
	}
	switch {
	case c.Seq == 0:
		return fmt.Errorf("%w: seq 0, sequences start at 1", ErrInvalidCommand)
	case c.RequiresWaypoint() && c.Waypoint == nil:
		return fmt.Errorf("%w: %s seq %d without a waypoint", ErrInvalidCommand, c.Type, c.Seq)
	case c.Waypoint != nil && !c.Waypoint.Finite():
		return fmt.Errorf("%w: %s seq %d waypoint is not finite", ErrInvalidCommand, c.Type, c.Seq)
	}
	return nil
}

// Publish validates a frame against conformance case C2 and delivers it.
//
// The frame's AckSeq becomes the acknowledgement Execute filters on. It is
// taken as the vehicle states it, lower included: a vehicle that restarts
// reports 0 again, and the re-send of its standing command must then reach it.
//
// Delivery never blocks: telemetry is state, not a log, so when the channel is
// full the oldest frame is dropped for the newest, and the count shows in
// Health. A refused frame is not delivered and does not refresh Health.
func (a *Agent) Publish(s VectorState) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrClosed
	}
	if err := a.checkFrame(s); err != nil {
		return err
	}
	for {
		select {
		case a.out <- s:
			a.lostReason = ""
			a.lastFrame = a.clock()
			a.lastSeenMs = s.LastSeenMs
			a.acked = s.AckSeq
			return nil
		default:
		}
		// Only Publish sends, under the lock, so after one frame is taken
		// the next send has room.
		select {
		case <-a.out:
			a.dropped++
		default:
		}
	}
}

func (a *Agent) checkFrame(s VectorState) error {
	switch {
	case s.ID != a.caps.ID:
		return fmt.Errorf("%w: id %q, this vector is %q", ErrInvalidFrame, s.ID, a.caps.ID)
	case !s.Position.Finite():
		return fmt.Errorf("%w: position is not finite", ErrInvalidFrame)
	case !finite(s.Heading):
		return fmt.Errorf("%w: heading %v is not finite", ErrInvalidFrame, s.Heading)
	case !finite(s.Speed) || s.Speed < 0:
		return fmt.Errorf("%w: speed %v, want finite and not negative", ErrInvalidFrame, s.Speed)
	case s.BatteryPct < 0 || s.BatteryPct > 100:
		return fmt.Errorf("%w: battery %d%%, want [0, 100]", ErrInvalidFrame, s.BatteryPct)
	case !PhysicalMode(s.Mode):
		return fmt.Errorf("%w: mode %q, telemetry reports idle, transit or rtb", ErrInvalidFrame, s.Mode)
	case s.LastSeenMs < 0:
		return fmt.Errorf("%w: mission time %d ms is negative", ErrInvalidFrame, s.LastSeenMs)
	case s.LastSeenMs < a.lastSeenMs:
		return fmt.Errorf("%w: mission time %d ms, before the last frame at %d ms", ErrInvalidFrame, s.LastSeenMs, a.lastSeenMs)
	}
	return nil
}

// Disconnected reports the link down. Health reports lost at once with the
// reason, until the next accepted frame. After Close it does nothing.
func (a *Agent) Disconnected(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closed {
		a.lostReason = "disconnected: " + reason
	}
}

// Health is the cached view: lost when closed, disconnected or before any
// frame, otherwise aged by the time since the last accepted frame. It reads
// the clock and nothing else.
func (a *Agent) Health() HealthStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	h := HealthStatus{Kind: HealthOK, LastFrameMs: a.lastSeenMs}
	var detail []string
	switch {
	case a.closed:
		h.Kind, detail = HealthLost, append(detail, "closed")
	case a.lostReason != "":
		h.Kind, detail = HealthLost, append(detail, a.lostReason)
	default:
		age := a.clock().Sub(a.lastFrame)
		switch {
		case age >= a.lostAfter:
			h.Kind = HealthLost
		case age >= a.degradedAfter:
			h.Kind = HealthDegraded
		}
		if h.Kind != HealthOK {
			detail = append(detail, fmt.Sprintf("no frame for %v", age.Round(100*time.Millisecond)))
		}
	}
	if a.dropped > 0 {
		detail = append(detail, fmt.Sprintf("%d telemetry frames dropped", a.dropped))
	}
	h.Detail = strings.Join(detail, "; ")
	return h
}

// Close closes the telemetry channel. Frames already buffered are still
// received; every later Execute and Publish returns ErrClosed. Calling it
// again returns nil.
func (a *Agent) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closed {
		a.closed = true
		close(a.out)
	}
	return nil
}
