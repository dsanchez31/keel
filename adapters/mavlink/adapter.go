package mavlink

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gomavlib/v4/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v4/pkg/dialects/common"
	"github.com/bluenviron/gomavlib/v4/pkg/dialects/minimal"
	"github.com/bluenviron/gomavlib/v4/pkg/message"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/vector"
)

// Defaults of Config.
const (
	// DefaultAckTimeout bounds the wait for a COMMAND_ACK. ArduPilot answers
	// a command in the loop that handles it; an answer that has not come is
	// lost on the link, and the engine's re-send retries the command.
	DefaultAckTimeout = 1500 * time.Millisecond
	// DefaultStreamInterval is the rate asked for the telemetry messages,
	// 4 Hz.
	DefaultStreamInterval = 250 * time.Millisecond
	// DefaultStationRadiusM is how close to its station a vehicle on rtb
	// counts as arrived.
	DefaultStationRadiusM = 5.0
)

const (
	// minTakeoffM is the lowest takeoff: a goto at ground level still lifts a
	// landed copter clear of the ground before it repositions.
	minTakeoffM = 3.0
	// takeoffReached is the fraction of the takeoff altitude at which the
	// climb counts as done and the reposition is sent. ArduCopter hands a
	// GUIDED takeoff to waypoint control as soon as a destination arrives.
	takeoffReached = 0.95
	// streamRetry spaces the stream requests while position frames stay
	// silent.
	streamRetry = 2 * time.Second
	// tickPeriod is how often timeouts are checked.
	tickPeriod = 100 * time.Millisecond
)

// Config configures one vehicle on a Link.
type Config struct {
	// SystemID is the vehicle's MAVLink system id, ArduPilot's
	// SYSID_THISMAV. 0 is the broadcast address and is refused.
	SystemID uint8
	// Capabilities are what the vector declares: MAVLink carries none. The
	// domain is held to the heartbeat's vehicle type.
	Capabilities vector.Capabilities
	// GeoidSeparationM is the height of mean sea level above the WGS84
	// ellipsoid at the operating area (EGM96, about +50 m in the Isère).
	// ArduPilot speaks AMSL and KEEL ellipsoid heights (spec section 3):
	// ellipsoid height is AMSL plus the separation, both ways. Zero is the
	// value for a SITL whose home altitude is given as an ellipsoid height.
	GeoidSeparationM float64
	// Agent tunes the vector.Agent behind the adapter: health thresholds,
	// telemetry buffer, clock. Its Supports must be nil: the adapter
	// executes every command type. Its LostAfter is also how long the
	// vehicle's heartbeat may be silent before the link is reported down.
	Agent vector.Options
	// AckTimeout, StreamInterval and StationRadiusM: zero means the defaults
	// above.
	AckTimeout     time.Duration
	StreamInterval time.Duration
	StationRadiusM float64
	// SITL says the vehicle is an ArduPilot SITL, whose simulation runs
	// SIM_SPEEDUP times faster than real time. The adapter then holds that
	// parameter to the speed SetSimSpeed last asked for, 1 until then: set on
	// first contact and again after a reboot, so a simulator left fast by an
	// earlier orchestrator comes back to the pace of this one.
	SITL bool
}

type config struct {
	sitl      bool
	sepM      float64
	ack       time.Duration
	stream    time.Duration
	radiusM   float64
	lostAfter time.Duration
	clock     func() time.Time
}

func newConfig(c Config) (config, error) {
	cfg := config{
		sitl:      c.SITL,
		sepM:      c.GeoidSeparationM,
		ack:       cmp.Or(c.AckTimeout, DefaultAckTimeout),
		stream:    cmp.Or(c.StreamInterval, DefaultStreamInterval),
		radiusM:   cmp.Or(c.StationRadiusM, DefaultStationRadiusM),
		lostAfter: cmp.Or(c.Agent.LostAfter, vector.DefaultLostAfter),
		clock:     c.Agent.Clock,
	}
	if cfg.clock == nil {
		cfg.clock = time.Now
	}
	switch {
	case c.SystemID == 0:
		return config{}, errors.New("mavlink: system id 0 is the broadcast address")
	case c.Agent.Supports != nil:
		return config{}, errors.New("mavlink: Config.Agent.Supports is set, the adapter executes every command type")
	case math.IsNaN(cfg.sepM) || math.IsInf(cfg.sepM, 0):
		return config{}, fmt.Errorf("mavlink: geoid separation %v, want finite", cfg.sepM)
	case cfg.ack <= 0 || cfg.stream <= 0:
		return config{}, fmt.Errorf("mavlink: ack timeout %v, stream interval %v: want positive", cfg.ack, cfg.stream)
	case math.IsNaN(cfg.radiusM) || math.IsInf(cfg.radiusM, 0) || cfg.radiusM <= 0:
		return config{}, fmt.Errorf("mavlink: station radius %v, want positive and finite", cfg.radiusM)
	}
	return cfg, nil
}

// Adapter is one ArduPilot vehicle on a Link: a vector.Vector over MAVLink.
//
// It is a transport plus a vector.Agent (design section 7.2). The Agent holds
// the telemetry channel, the Seq filter, the declared command types and the
// cached health; the Adapter turns frames into telemetry and commands into
// COMMAND_INT steps, and fills AckSeq from their acknowledgements.
//
// One goroutine, stopped by Close, owns the vehicle's state: it reads the
// frames the Link routes to it, runs the command in progress one
// acknowledged step at a time, and checks the timeouts.
type Adapter struct {
	agent *vector.Agent
	link  *Link
	sysid uint8
	caps  vector.Capabilities
	cfg   config
	inbox <-chan message.Message

	// mu guards the command mailbox and the notes Health reports.
	mu sync.Mutex
	// pending is the single-slot command mailbox, latest wins, as in
	// adapters/native: a newer Seq supersedes one not yet started.
	pending *vector.Command
	notes   notes
	notify  chan struct{}
	sim     simSpeed

	closing sync.Once
	stop    chan struct{}
	done    chan struct{}

	// st is owned by the run goroutine.
	st state
}

var _ vector.Vector = (*Adapter)(nil)

// notes are what Health says beyond the Agent's own view. No package logs, so
// Health is the adapter's observable surface (design section 7.3).
type notes struct {
	// withheld is why frames are not published, empty while they are.
	withheld string
	// refused is why the last command was not applied, until the next one
	// is.
	refused string
}

// simSpeed is a SITL's SIM_SPEEDUP: the value asked for and the value the
// autopilot last confirmed, zero while unknown. changed is closed and
// replaced whenever the confirmed value changes.
type simSpeed struct {
	want, have float32
	changed    chan struct{}
}

// simParam is the SITL parameter holding the simulation's speed.
const simParam = "SIM_SPEEDUP"

// state is the adapter's view of the vehicle.
type state struct {
	hb    *heartbeat
	hbAt  time.Time
	quiet bool
	fix   *fix
	fixAt time.Time
	// heading is the last known one: a frame without it keeps it.
	heading      float64
	battery      int
	batteryKnown bool
	sysStatus    bool
	// ekf is the latest EKF_STATUS_REPORT's flags, ekfKnown whether one came
	// since the autopilot booted.
	ekf       ardupilotmega.EKF_STATUS_FLAGS
	ekfKnown  bool
	landed    common.MAV_LANDED_STATE
	home      *vector.Position
	streamsAt time.Time

	// acked is the Seq of the last command applied, the frames' AckSeq.
	acked   uint64
	applied *vector.Command
	arrived bool
	landAt  time.Time
	// simSentAt is when SIM_SPEEDUP was last set, zero for never.
	simSentAt time.Time
	// unconfirmed is set when a command that sent steps is applied, cleared
	// by the next heartbeat (modeInputs.unconfirmed).
	unconfirmed bool

	exec     *execution
	awaiting *awaiting
}

// step is one move of a command: a COMMAND_INT to be acknowledged, or the
// wait for a copter's climb.
type step struct {
	msg      *common.MessageCommandInt
	climbToM float64
}

// execution is a command under way.
type execution struct {
	cmd    vector.Command
	steps  []step
	next   int
	launch bool
	// climbSince is when the climb step began. Only a heartbeat heard after
	// it can report the copter disarmed: the one before may predate the
	// arming its COMMAND_ACK already confirmed, ArduPilot sending HEARTBEAT
	// at 1 Hz.
	climbSince time.Time
}

// awaiting is the COMMAND_INT whose COMMAND_ACK the adapter waits for. One
// at a time per vehicle: acknowledgements name the command, not the request,
// so a second DO_REPOSITION in flight would take the first one's answer.
type awaiting struct {
	cmd    common.MAV_CMD
	sentAt time.Time
	// exec is the execution the step belongs to, nil for a landing on
	// arrival.
	exec *execution
}

// NewAdapter registers a vehicle on the link and starts serving it. Frames
// are published once the vehicle has been heard: until then Health reports
// lost, waiting for its heartbeat.
func NewAdapter(link *Link, c Config) (*Adapter, error) {
	cfg, err := newConfig(c)
	if err != nil {
		return nil, err
	}
	a := &Adapter{link: link, sysid: c.SystemID, cfg: cfg, notify: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	a.sim.changed = make(chan struct{})
	if cfg.sitl {
		a.sim.want = 1
	}
	a.agent, err = vector.NewAgent(c.Capabilities, a.transmit, c.Agent)
	if err != nil {
		return nil, err
	}
	a.caps = a.agent.Describe()
	a.inbox, err = link.register(c.SystemID)
	if err != nil {
		_ = a.agent.Close()
		return nil, err
	}
	a.notes.withheld = fmt.Sprintf("waiting for a HEARTBEAT from system %d", a.sysid)
	go a.run()
	return a, nil
}

// Describe returns the configured capabilities, tags sorted.
func (a *Adapter) Describe() vector.Capabilities { return a.agent.Describe() }

// Supports returns every command type, sorted.
func (a *Adapter) Supports() []vector.CommandType { return a.agent.Supports() }

// Telemetry returns the channel frames are delivered on. It lives until
// Close, across silences of the link.
func (a *Adapter) Telemetry() <-chan vector.VectorState { return a.agent.Telemetry() }

// Execute validates a command, filters it by Seq and puts it in the mailbox.
// It never waits on the network.
func (a *Adapter) Execute(c vector.Command) error { return a.agent.Execute(c) }

// Health is the Agent's cached view, followed by the adapter's notes: why
// frames are withheld, why the last command was refused.
func (a *Adapter) Health() vector.HealthStatus {
	h := a.agent.Health()
	a.mu.Lock()
	n := a.notes
	a.mu.Unlock()
	detail := []string{}
	for _, d := range []string{h.Detail, n.withheld, n.refused} {
		if d != "" {
			detail = append(detail, d)
		}
	}
	if d := a.link.dropped(a.sysid); d > 0 {
		detail = append(detail, fmt.Sprintf("%d MAVLink messages dropped", d))
	}
	h.Detail = strings.Join(detail, "; ")
	return h
}

// Close stops the adapter's goroutine, frees its system id on the link and
// closes the telemetry channel. Calling it again returns nil.
func (a *Adapter) Close() error {
	a.closing.Do(func() {
		close(a.stop)
		<-a.done
		a.link.unregister(a.sysid)
	})
	return a.agent.Close()
}

// transmit is the Agent's Transmit: it fills the mailbox and wakes the run
// goroutine, without blocking.
func (a *Adapter) transmit(c vector.Command) error {
	a.mu.Lock()
	a.pending = &c
	a.mu.Unlock()
	a.wake()
	return nil
}

func (a *Adapter) wake() {
	select {
	case a.notify <- struct{}{}:
	default:
	}
}

// SetSimSpeed sets a SITL's SIM_SPEEDUP and returns once the autopilot has
// confirmed it, or with the context's error. The adapter keeps setting it
// until confirmed, and again after the autopilot reboots, so a context
// already done records the speed without waiting for it. A vehicle not
// configured as a SITL is refused: a real one has no such parameter.
func (a *Adapter) SetSimSpeed(ctx context.Context, speed float64) error {
	if !a.cfg.sitl {
		return fmt.Errorf("mavlink: system %d is not a SITL", a.sysid)
	}
	if !(speed > 0) || math.IsInf(speed, 0) {
		return fmt.Errorf("mavlink: simulation speed %v, want positive and finite", speed)
	}
	want := float32(speed)
	a.mu.Lock()
	a.sim.want = want
	a.mu.Unlock()
	a.wake()
	for {
		a.mu.Lock()
		have, changed := a.sim.have, a.sim.changed
		a.mu.Unlock()
		if have == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("mavlink: %s %v on system %d not confirmed: %w", simParam, speed, a.sysid, ctx.Err())
		case <-changed:
		}
	}
}

// holdSimSpeed sets SIM_SPEEDUP while the autopilot has not confirmed the
// value asked for, at most once per ack timeout.
func (a *Adapter) holdSimSpeed() {
	s := &a.st
	a.mu.Lock()
	want, have := a.sim.want, a.sim.have
	a.mu.Unlock()
	if want == 0 || want == have || s.hb == nil {
		return
	}
	now := a.cfg.clock()
	if !s.simSentAt.IsZero() && now.Sub(s.simSentAt) < a.cfg.ack {
		return
	}
	s.simSentAt = now
	_ = a.send(paramSet(a.sysid, simParam, want))
}

// simConfirmed records the SIM_SPEEDUP an autopilot reports, zero for
// unknown.
func (a *Adapter) simConfirmed(v float32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sim.have == v {
		return
	}
	a.sim.have = v
	close(a.sim.changed)
	a.sim.changed = make(chan struct{})
}

func (a *Adapter) take() *vector.Command {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.pending
	a.pending = nil
	return c
}

func (a *Adapter) setWithheld(reason string) {
	a.mu.Lock()
	a.notes.withheld = reason
	a.mu.Unlock()
}

func (a *Adapter) setRefused(reason string) {
	a.mu.Lock()
	a.notes.refused = reason
	a.mu.Unlock()
}

// run serves the vehicle until Close.
func (a *Adapter) run() {
	defer close(a.done)
	t := time.NewTicker(tickPeriod)
	defer t.Stop()
	for {
		select {
		case <-a.stop:
			return
		case m := <-a.inbox:
			a.handle(m)
		case <-a.notify:
		case <-t.C:
			a.tick()
		}
		a.advance()
		a.holdSimSpeed()
	}
}

// handle takes in one message from the vehicle's autopilot.
func (a *Adapter) handle(m message.Message) {
	now := a.cfg.clock()
	s := &a.st
	switch m := m.(type) {
	case *minimal.MessageHeartbeat:
		hb := readHeartbeat(m)
		first := s.hb == nil
		if !first && hb.armed && !s.hb.armed {
			// ArduPilot moves home to where the vehicle arms.
			_ = a.send(requestMessage(a.sysid, (&common.MessageHomePosition{}).GetID()))
		}
		s.hb, s.hbAt, s.quiet, s.unconfirmed = &hb, now, false, false
		if first || s.fix == nil || now.Sub(s.fixAt) > max(4*a.cfg.stream, time.Second) {
			// Fire and forget: asked on first contact, even when position
			// frames came first at the autopilot's own rates, then again on
			// the next heartbeat while position frames stay silent, which
			// covers a reboot and a link that came back.
			a.requestStreams(now)
		}
	case *common.MessageGlobalPositionInt:
		a.position(readFix(m, a.cfg.sepM), now)
	case *common.MessageSysStatus:
		s.sysStatus = true
		s.battery, s.batteryKnown = battery(m.BatteryRemaining)
	case *common.MessageExtendedSysState:
		s.landed = m.LandedState
	case *ardupilotmega.MessageEkfStatusReport:
		s.ekf, s.ekfKnown = m.Flags, true
	case *common.MessageHomePosition:
		s.home = &vector.Position{Lat: fromDegE7(m.Latitude), Lon: fromDegE7(m.Longitude), AltM: float64(m.Altitude)/1000 + a.cfg.sepM}
	case *common.MessageCommandAck:
		a.acknowledge(m)
	case *common.MessageParamValue:
		if strings.TrimRight(m.ParamId, "\x00") == simParam {
			a.simConfirmed(m.ParamValue)
		}
	}
}

// requestStreams asks for the telemetry messages at the stream interval, and
// for home while it is unknown, at most every streamRetry.
func (a *Adapter) requestStreams(now time.Time) {
	if !a.st.streamsAt.IsZero() && now.Sub(a.st.streamsAt) < streamRetry {
		return
	}
	a.st.streamsAt = now
	us := int32(a.cfg.stream / time.Microsecond)
	for _, m := range []message.Message{&common.MessageGlobalPositionInt{}, &common.MessageSysStatus{}, &common.MessageExtendedSysState{}, &ardupilotmega.MessageEkfStatusReport{}} {
		_ = a.send(messageInterval(a.sysid, m.GetID(), us))
	}
	if a.st.home == nil {
		_ = a.send(requestMessage(a.sysid, (&common.MessageHomePosition{}).GetID()))
	}
}

// send writes a message to the vehicle.
func (a *Adapter) send(m message.Message) error { return a.link.write(a.sysid, m) }

// position takes in a GLOBAL_POSITION_INT: reboot detection, rtb arrival,
// and the frame.
func (a *Adapter) position(f fix, now time.Time) {
	s := &a.st
	// time_boot_ms going backwards is an autopilot that restarted: it has
	// forgotten every command, and the frames say so (spec section 4.2).
	// The margin absorbs datagrams arriving out of order.
	if s.fix != nil && f.bootMs+1000 < s.fix.bootMs {
		a.reboot()
		// The intervals went with the reboot; frames at the autopilot's
		// default rates would keep the silence check from noticing.
		a.requestStreams(now)
	}
	s.fix, s.fixAt = &f, now
	if f.heading != nil {
		s.heading = *f.heading
	}
	a.arrival(now)
	a.publish()
}

// reboot forgets everything the autopilot forgot.
func (a *Adapter) reboot() {
	s := &a.st
	s.acked, s.applied, s.arrived, s.landAt, s.unconfirmed = 0, nil, false, time.Time{}, false
	s.exec, s.awaiting = nil, nil
	s.home, s.landed, s.streamsAt = nil, common.MAV_LANDED_STATE_UNDEFINED, time.Time{}
	// The EKF restarts with it, and the frames carry (0, 0) until it knows
	// where it is again.
	s.ekf, s.ekfKnown = 0, false
	// A SITL restarts at the speed of its command line.
	s.simSentAt = time.Time{}
	a.simConfirmed(0)
}

// arrival watches an applied rtb: arrived within the station radius of its
// station, or of home for the autopilot's own return. A copter arrived at its
// station lands.
func (a *Adapter) arrival(now time.Time) {
	s := &a.st
	if s.applied == nil || s.applied.Type != vector.CommandRTB || s.hb == nil {
		return
	}
	target := s.applied.Waypoint
	if target == nil {
		target = s.home
	}
	if target == nil || domain.HaversineM(s.fix.pos, *target) > a.cfg.radiusM {
		return
	}
	s.arrived = true
	if s.applied.Waypoint == nil || s.hb.kind != kindCopter || a.onGround() || classifyMode(s.hb.kind, s.hb.mode) == modeReturning {
		return
	}
	if s.awaiting != nil || (!s.landAt.IsZero() && now.Sub(s.landAt) < a.cfg.ack) {
		return
	}
	s.landAt = now
	if err := a.send(land(a.sysid)); err != nil {
		a.setRefused(fmt.Sprintf("landing at the station not sent: %v", err))
		return
	}
	s.awaiting = &awaiting{cmd: common.MAV_CMD_NAV_LAND, sentAt: now}
}

// onGround is disarmed, or a copter that says it is landed.
func (a *Adapter) onGround() bool {
	s := &a.st
	if s.hb == nil || !s.hb.armed {
		return true
	}
	return s.hb.kind == kindCopter && landed(s.landed)
}

// withheld says why no frame can be published yet, empty when one can.
func (a *Adapter) withheld() string {
	s := &a.st
	switch {
	case s.hb == nil:
		return fmt.Sprintf("waiting for a HEARTBEAT from system %d", a.sysid)
	case !s.hb.ardupilot:
		return fmt.Sprintf("system %d is not an ArduPilot autopilot", a.sysid)
	case s.hb.kind == kindUnknown:
		return fmt.Sprintf("system %d is neither ArduCopter nor ArduRover", a.sysid)
	case s.hb.kind.domain() != a.caps.Domain:
		return fmt.Sprintf("system %d runs %s (%s), capabilities declare %s", a.sysid, s.hb.kind, s.hb.kind.domain(), a.caps.Domain)
	case !s.sysStatus:
		return fmt.Sprintf("waiting for SYS_STATUS from system %d", a.sysid)
	case !s.batteryKnown:
		return fmt.Sprintf("battery of system %d unknown (SYS_STATUS.battery_remaining -1)", a.sysid)
	case !s.ekfKnown:
		return fmt.Sprintf("waiting for EKF_STATUS_REPORT from system %d", a.sysid)
	case !positionKnown(s.ekf, s.hb.armed):
		return fmt.Sprintf("system %d has no absolute position estimate (EKF_STATUS_REPORT flags %#x)", a.sysid, uint16(s.ekf))
	}
	return ""
}

// publish hands the Agent the frame the latest fix makes.
func (a *Adapter) publish() {
	s := &a.st
	reason := a.withheld()
	if reason == "" {
		frame := vector.VectorState{
			ID:         a.caps.ID,
			Position:   s.fix.pos,
			Heading:    s.heading,
			Speed:      s.fix.speed,
			BatteryPct: s.battery,
			Link:       vector.LinkOK,
			Mode:       physicalMode(a.modeInputs()),
			// The autopilot knows no mission time: the engine stamps the
			// tick it accepts the frame on (spec section 4.2).
			LastSeenMs: 0,
			AckSeq:     s.acked,
		}
		if err := a.agent.Publish(frame); err != nil && !errors.Is(err, vector.ErrClosed) {
			reason = err.Error()
		}
	}
	a.setWithheld(reason)
}

func (a *Adapter) modeInputs() modeInputs {
	s := &a.st
	in := modeInputs{
		armed:       s.hb.armed,
		onGround:    a.onGround(),
		mode:        classifyMode(s.hb.kind, s.hb.mode),
		arrived:     s.arrived,
		launching:   s.exec != nil && s.exec.launch,
		unconfirmed: s.unconfirmed,
	}
	if s.applied != nil {
		in.applied = s.applied.Type
		in.toStation = s.applied.Type == vector.CommandRTB && s.applied.Waypoint != nil
	}
	return in
}

// acknowledge takes a COMMAND_ACK. One addressed to another ground station,
// or for no command this adapter waits on, is not its business.
func (a *Adapter) acknowledge(m *common.MessageCommandAck) {
	s := &a.st
	w := s.awaiting
	if w == nil || m.Command != w.cmd || (m.TargetSystem != 0 && m.TargetSystem != a.link.sysid) {
		return
	}
	switch m.Result {
	case common.MAV_RESULT_IN_PROGRESS:
		w.sentAt = a.cfg.clock()
		return
	case common.MAV_RESULT_ACCEPTED:
		s.awaiting = nil
		if w.exec != nil && w.exec == s.exec {
			w.exec.next++
		}
		return
	}
	s.awaiting = nil
	switch w.exec {
	case nil:
		a.setRefused(fmt.Sprintf("landing at the station refused: %s", m.Result))
	case s.exec:
		a.fail(fmt.Sprintf("%s refused: %s", w.cmd, m.Result))
	}
}

// fail abandons the command under way. It stays unacknowledged, and the
// engine's re-send starts it again.
func (a *Adapter) fail(reason string) {
	s := &a.st
	a.setRefused(fmt.Sprintf("%s seq %d not applied: %s", s.exec.cmd.Type, s.exec.cmd.Seq, reason))
	s.exec = nil
}

// tick checks the timeouts: a COMMAND_ACK that did not come, a heartbeat
// gone silent.
func (a *Adapter) tick() {
	now := a.cfg.clock()
	s := &a.st
	if w := s.awaiting; w != nil && now.Sub(w.sentAt) >= a.cfg.ack {
		s.awaiting = nil
		if w.exec != nil && w.exec == s.exec {
			a.fail(fmt.Sprintf("no COMMAND_ACK to %s within %v", w.cmd, a.cfg.ack))
		}
	}
	if s.hb != nil && !s.quiet && now.Sub(s.hbAt) >= a.cfg.lostAfter {
		s.quiet = true
		a.agent.Disconnected(fmt.Sprintf("no HEARTBEAT from system %d for %v", a.sysid, a.cfg.lostAfter))
	}
}

// advance moves the command under way as far as it can go: it starts the
// latest command once no acknowledgement is outstanding, and sends each step
// once the previous one is accepted.
func (a *Adapter) advance() {
	s := &a.st
	// A vehicle whose frames are withheld is not commanded: moving a vehicle
	// KEEL cannot see is worse than leaving the command waiting.
	if s.awaiting != nil || s.fix == nil || a.withheld() != "" {
		return
	}
	if c := a.take(); c != nil {
		switch {
		case s.exec != nil && c.Seq == s.exec.cmd.Seq, c.Seq <= s.acked:
			// The engine re-sending the command under way, or one applied
			// before its frame reached the Agent.
		default:
			s.exec = a.plan(*c)
		}
	}
	for s.exec != nil {
		e := s.exec
		if e.next == len(e.steps) {
			a.complete()
			return
		}
		st := e.steps[e.next]
		if st.msg == nil {
			if e.climbSince.IsZero() {
				e.climbSince = a.cfg.clock()
			}
			switch {
			case !s.hb.armed && s.hbAt.After(e.climbSince):
				a.fail("disarmed during the takeoff")
			case s.fix.relM >= st.climbToM:
				e.next++
				continue
			}
			return
		}
		if err := a.send(st.msg); err != nil {
			a.fail(fmt.Sprintf("%s not sent: %v", st.msg.Command, err))
			return
		}
		s.awaiting = &awaiting{cmd: st.msg.Command, sentAt: a.cfg.clock(), exec: e}
		return
	}
}

// complete records the command under way as applied: the frames carry its
// Seq from now on.
func (a *Adapter) complete() {
	s := &a.st
	c := s.exec.cmd
	s.unconfirmed = len(s.exec.steps) > 0
	s.acked, s.applied, s.exec = c.Seq, &c, nil
	s.arrived, s.landAt = false, time.Time{}
	a.setRefused("")
}

// plan turns a command into its steps, under the vehicle's state now
func (a *Adapter) plan(c vector.Command) *execution {
	s := &a.st
	k := s.hb.kind
	e := &execution{cmd: c}
	// ArduRover reads the ground speed off the reposition; ArduCopter ignores
	// it and is sent DO_CHANGE_SPEED once in GUIDED, entering the mode having
	// reset its speed.
	speed := -1.0
	if k == kindRover {
		speed = a.caps.CruiseSpeed
	}
	grounded := a.onGround()
	switch c.Type {
	case vector.CommandGoto:
		wp := *c.Waypoint
		amsl := wp.AltM - a.cfg.sepM
		if grounded {
			// Auto-launch without the operator's consent, by decision
			// (design section 10): the durable fix is an opt-in option.
			switch k {
			case kindCopter:
				climb := max(amsl-s.fix.amslM, minTakeoffM)
				e.launch = true
				e.steps = append(e.steps,
					step{msg: setMode(a.sysid, guidedMode(k))},
					step{msg: arm(a.sysid)},
					step{msg: takeoff(a.sysid, climb)},
					step{climbToM: takeoffReached * climb},
				)
			case kindRover:
				e.steps = append(e.steps, step{msg: arm(a.sysid)})
			}
		}
		e.steps = append(e.steps, step{msg: repositionTo(a.sysid, wp.Lat, wp.Lon, amsl, speed)})
		if k == kindCopter {
			e.steps = append(e.steps, step{msg: changeSpeed(a.sysid, a.caps.CruiseSpeed)})
		}
	case vector.CommandHold, vector.CommandAbort:
		// A vehicle on the ground already holds.
		if !grounded {
			e.steps = append(e.steps, step{msg: repositionTo(a.sysid, s.fix.pos.Lat, s.fix.pos.Lon, s.fix.amslM, -1)})
		}
	case vector.CommandRTB:
		// A vehicle on the ground is not launched to come back.
		switch {
		case grounded:
		case c.Waypoint != nil:
			wp := *c.Waypoint
			alt := max(s.fix.amslM, wp.AltM-a.cfg.sepM)
			e.steps = append(e.steps, step{msg: repositionTo(a.sysid, wp.Lat, wp.Lon, alt, speed)})
		default:
			e.steps = append(e.steps, step{msg: returnToLaunch(a.sysid)})
		}
	}
	return e
}
