package mavlink

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bluenviron/gomavlib/v4"
	"github.com/bluenviron/gomavlib/v4/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v4/pkg/dialects/minimal"
	"github.com/bluenviron/gomavlib/v4/pkg/message"
)

// Defaults of LinkOptions: the identity MAVLink ground stations use by
// convention, and the heartbeat rate of the heartbeat microservice.
const (
	DefaultSystemID        = 255
	DefaultComponentID     = uint8(minimal.MAV_COMP_ID_MISSIONPLANNER)
	DefaultHeartbeatPeriod = time.Second
)

// inboxSize bounds the messages queued for one adapter. The link's reader
// must never wait on an adapter: gomavlib delivers every channel's frames
// through one event stream, so one stalled adapter would stall the whole
// link.
const inboxSize = 256

// Errors of the link.
var (
	// ErrLinkClosed is returned once the link is closed.
	ErrLinkClosed = errors.New("mavlink: link closed")
	// ErrNoRoute is returned when writing to a system the link has not heard
	// on any channel, or whose channel has closed since.
	ErrNoRoute = errors.New("mavlink: no channel to the system")
)

// LinkOptions configures a Link.
type LinkOptions struct {
	// Endpoints are where the vehicles are: a UDP server on the port a SITL
	// or MAVProxy pushes to, a serial telemetry radio, a TCP client. At least
	// one.
	Endpoints []gomavlib.Endpoint
	// SystemID and ComponentID are the orchestrator's identity on the link.
	// Zero means the defaults above.
	SystemID    uint8
	ComponentID uint8
	// HeartbeatPeriod is the rate of the orchestrator's own heartbeat, which
	// ArduPilot's ground station failsafe watches. Zero means the default.
	HeartbeatPeriod time.Duration
}

// Link is one MAVLink node shared by every vehicle behind its endpoints. It
// routes each frame from a vehicle's autopilot (component 1) to the Adapter
// registered for the frame's system id, and writes an adapter's messages to
// the channel its vehicle was last heard on.
//
// One goroutine, stopped by Close: the reader of the node's events.
type Link struct {
	node *gomavlib.Node
	// sysid is the orchestrator's own system id, the one COMMAND_ACKs for
	// its commands are addressed to.
	sysid uint8

	mu     sync.Mutex
	routes map[uint8]*route
	closed bool

	done chan struct{}
}

// route is one registered vehicle.
type route struct {
	inbox chan message.Message
	// channel is where the vehicle was last heard, nil before or after the
	// channel closes.
	channel *gomavlib.Channel
	dropped uint64
}

// NewLink opens the endpoints and starts routing.
func NewLink(opts LinkOptions) (*Link, error) {
	if len(opts.Endpoints) == 0 {
		return nil, errors.New("mavlink: link without an endpoint")
	}
	if opts.SystemID == 0 {
		opts.SystemID = DefaultSystemID
	}
	if opts.ComponentID == 0 {
		opts.ComponentID = DefaultComponentID
	}
	if opts.HeartbeatPeriod == 0 {
		opts.HeartbeatPeriod = DefaultHeartbeatPeriod
	}
	if opts.HeartbeatPeriod < 0 {
		return nil, fmt.Errorf("mavlink: heartbeat period %v, want positive", opts.HeartbeatPeriod)
	}
	node := &gomavlib.Node{
		Endpoints:           opts.Endpoints,
		Dialect:             ardupilotmega.Dialect,
		OutVersion:          gomavlib.V2,
		OutSystemID:         opts.SystemID,
		OutComponentID:      opts.ComponentID,
		HeartbeatPeriod:     opts.HeartbeatPeriod,
		HeartbeatSystemType: int(minimal.MAV_TYPE_GCS),
		// Streams are asked for per message with SET_MESSAGE_INTERVAL by each
		// adapter; gomavlib's automatic requests use the deprecated
		// REQUEST_DATA_STREAM.
		StreamRequestEnable: false,
	}
	if err := node.Initialize(); err != nil {
		return nil, fmt.Errorf("mavlink: opening the link: %w", err)
	}
	l := &Link{node: node, sysid: opts.SystemID, routes: map[uint8]*route{}, done: make(chan struct{})}
	go l.run()
	return l, nil
}

// Close closes the endpoints and stops the reader. Adapters still registered
// stop hearing their vehicles, and their writes return ErrLinkClosed. Calling
// it again returns nil.
func (l *Link) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	l.node.Close()
	<-l.done
	return nil
}

// register reserves a system id and returns the inbox its frames arrive on.
func (l *Link) register(sysid uint8) (<-chan message.Message, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.closed:
		return nil, ErrLinkClosed
	case l.routes[sysid] != nil:
		return nil, fmt.Errorf("mavlink: system %d already has an adapter on this link", sysid)
	}
	r := &route{inbox: make(chan message.Message, inboxSize)}
	l.routes[sysid] = r
	return r.inbox, nil
}

// unregister frees a system id. Frames for it are ignored from then on.
func (l *Link) unregister(sysid uint8) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.routes, sysid)
}

// dropped is how many messages for the system were dropped on a full inbox.
func (l *Link) dropped(sysid uint8) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r := l.routes[sysid]; r != nil {
		return r.dropped
	}
	return 0
}

// write sends a message to a system on the channel it was last heard on. It
// does not wait on the network: gomavlib queues the message for the channel's
// writer.
func (l *Link) write(sysid uint8, m message.Message) error {
	l.mu.Lock()
	var ch *gomavlib.Channel
	if r := l.routes[sysid]; r != nil {
		ch = r.channel
	}
	closed := l.closed
	l.mu.Unlock()
	switch {
	case closed:
		return ErrLinkClosed
	case ch == nil:
		return fmt.Errorf("%w %d", ErrNoRoute, sysid)
	}
	return l.node.WriteMessageTo(ch, m)
}

// run reads the node's events until Close. gomavlib closes the event channel
// once the node has stopped.
func (l *Link) run() {
	defer close(l.done)
	for evt := range l.node.Events() {
		switch e := evt.(type) {
		case *gomavlib.EventFrame:
			l.route(e)
		case *gomavlib.EventChannelClose:
			l.forget(e.Channel)
		}
	}
}

// route delivers a frame from a vehicle's autopilot to its adapter, never
// waiting: a full inbox drops the frame and counts it. Other components of a
// vehicle (a gimbal, a companion computer) and systems without an adapter
// (another ground station) are ignored.
func (l *Link) route(e *gomavlib.EventFrame) {
	if e.ComponentID() != uint8(minimal.MAV_COMP_ID_AUTOPILOT1) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.routes[e.SystemID()]
	if r == nil {
		return
	}
	r.channel = e.Channel
	select {
	case r.inbox <- e.Message():
	default:
		r.dropped++
	}
}

// forget drops a closed channel from every route that used it.
func (l *Link) forget(ch *gomavlib.Channel) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.routes {
		if r.channel == ch {
			r.channel = nil
		}
	}
}
