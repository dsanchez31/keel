// Package mavlinktest provides an in-process ArduPilot for testing code that
// drives vehicles over MAVLink, the way net/http/httptest provides servers.
//
// The Autopilot answers COMMAND_INT the way ArduCopter and ArduRover do in the
// handlers the adapter relies on (spec section 7.4): mode changes, arming,
// GUIDED takeoff, DO_REPOSITION, DO_CHANGE_SPEED, LAND, RTL, stream intervals
// and HOME_POSITION requests. It flies a crude point mass, fast so that a test
// flies in well under a second, and streams HEARTBEAT, GLOBAL_POSITION_INT,
// SYS_STATUS, EXTENDED_SYS_STATE and EKF_STATUS_REPORT every 20 ms. As a SITL
// it also has SIM_SPEEDUP, set by PARAM_SET. Tests steer it: refuse or mute a
// command, fall silent, lose its position estimate, reboot.
package mavlinktest

import (
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gomavlib/v4"
	"github.com/bluenviron/gomavlib/v4/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v4/pkg/dialects/common"
	"github.com/bluenviron/gomavlib/v4/pkg/dialects/minimal"
	"github.com/bluenviron/gomavlib/v4/pkg/message"

	"github.com/dsanchez31/keel/internal/domain"
)

// The pad an Autopilot boots on: the gcs-west station of the reference world,
// ground at 380 m AMSL.
const (
	PadLat  = 45.02683
	PadLon  = 5.00526
	PadAMSL = 380.0
)

// Where an airborne Autopilot starts: 30 m over a field east of the pad.
const (
	FieldLat = 45.03000
	FieldLon = 5.01000
)

// The kinematics.
const (
	tick     = 20 * time.Millisecond
	speed    = 500.0 // m/s
	climb    = 60.0  // m/s
	bootBase = 60000 // ms, so that a reboot is seen going backwards
)

// Options configures an Autopilot.
type Options struct {
	// SystemID is SYSID_THISMAV. Required.
	SystemID uint8
	// Type is the heartbeat's vehicle type: zero means MAV_TYPE_QUADROTOR.
	// MAV_TYPE_GROUND_ROVER behaves as ArduRover.
	Type minimal.MAV_TYPE
	// Autopilot is the heartbeat's autopilot: zero means ArduPilot.
	Autopilot minimal.MAV_AUTOPILOT
	// Airborne starts armed in GUIDED, 30 m over the field.
	Airborne bool
	// NoBattery reports battery_remaining -1, a vehicle without a monitor.
	NoBattery bool
	// NoPosition boots without a position estimate, as before a GPS lock:
	// EKF_STATUS_REPORT without a horizontal absolute flag, GLOBAL_POSITION_INT
	// at (0, 0) until SetNoPosition(false).
	NoPosition bool
	// SITL gives the autopilot SITL's SIM_SPEEDUP parameter, 1 at boot, set by
	// PARAM_SET and answered with PARAM_VALUE. Without it a PARAM_SET of it
	// goes unanswered, as ArduPilot ignores a parameter it does not have.
	SITL bool
	// Endpoint is how the autopilot reaches the ground, as SITL's SERIAL0
	// does: a client, a UDP one pushing to a ground station say, whose
	// channel New waits for. Nil means a TCP server on 127.0.0.1, whose
	// address Addr returns.
	Endpoint gomavlib.Endpoint
}

// openWithin bounds New's wait for the channel of a caller's endpoint.
const openWithin = 5 * time.Second

// Autopilot is one simulated ArduPilot vehicle. Its methods are safe for
// concurrent use.
type Autopilot struct {
	opts Options
	addr string
	node *gomavlib.Node

	mu        sync.Mutex
	booted    time.Time
	bootBase  uint32
	armed     bool
	mode      uint32
	lat, lon  float64
	amsl      float64
	homeLat   float64
	homeLon   float64
	homeAMSL  float64
	target    *[3]float64
	climbTo   *float64
	vn, ve    float64
	silent    bool
	speedSet  float64
	speedup   float32
	paramSets int
	intervals map[uint32]int32
	homeAsked int
	cmds      []common.MessageCommandInt
	refuse    map[common.MAV_CMD]common.MAV_RESULT
	mute      map[common.MAV_CMD]bool

	// noFix is the position estimate missing; sentLat and sentLon are the
	// last position known, (0, 0) before the first.
	noFix            bool
	sentLat, sentLon float64

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// New starts an Autopilot. Close stops it.
func New(o Options) (*Autopilot, error) {
	if o.SystemID == 0 {
		return nil, errors.New("mavlinktest: system id 0")
	}
	if o.Type == 0 {
		o.Type = minimal.MAV_TYPE_QUADROTOR
	}
	if o.Autopilot == 0 {
		o.Autopilot = minimal.MAV_AUTOPILOT_ARDUPILOTMEGA
	}
	a := &Autopilot{
		opts:      o,
		booted:    time.Now(),
		bootBase:  bootBase,
		lat:       PadLat,
		lon:       PadLon,
		amsl:      PadAMSL,
		homeLat:   PadLat,
		homeLon:   PadLon,
		homeAMSL:  PadAMSL,
		noFix:     o.NoPosition,
		speedup:   1,
		intervals: map[uint32]int32{},
		refuse:    map[common.MAV_CMD]common.MAV_RESULT{},
		mute:      map[common.MAV_CMD]bool{},
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	if a.rover() {
		a.mode = uint32(ardupilotmega.ROVER_MODE_HOLD)
	} else {
		a.mode = uint32(ardupilotmega.COPTER_MODE_STABILIZE)
	}
	if o.Airborne {
		a.armed, a.lat, a.lon, a.amsl = true, FieldLat, FieldLon, PadAMSL+30
		a.mode = a.guided()
	}
	ep := o.Endpoint
	if ep == nil {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		a.addr = ln.Addr().String()
		ep = &gomavlib.EndpointCustomServer{Listen: func() (net.Listener, error) { return ln, nil }, Label: "mavlinktest"}
	}
	a.node = &gomavlib.Node{
		Endpoints:      []gomavlib.Endpoint{ep},
		Dialect:        ardupilotmega.Dialect,
		OutVersion:     gomavlib.V2,
		OutSystemID:    o.SystemID,
		OutComponentID: uint8(minimal.MAV_COMP_ID_AUTOPILOT1),
		// The autopilot sends its own heartbeat: gomavlib's announces a GCS.
		HeartbeatDisable: true,
	}
	if err := a.node.Initialize(); err != nil {
		return nil, err
	}
	if o.Endpoint != nil {
		// gomavlib opens a client's channel in the background. Returned
		// before it, the autopilot would start its goroutines under a test
		// already counting them, and its first messages would be late.
		if err := a.awaitChannel(); err != nil {
			a.node.Close()
			return nil, err
		}
	}
	go a.run()
	return a, nil
}

// awaitChannel reads the node's events until its first channel opens. Frames
// cannot arrive before it.
func (a *Autopilot) awaitChannel() error {
	timeout := time.After(openWithin)
	for {
		select {
		case evt, ok := <-a.node.Events():
			if !ok {
				return errors.New("mavlinktest: node closed before its channel opened")
			}
			if _, open := evt.(*gomavlib.EventChannelOpen); open {
				return nil
			}
		case <-timeout:
			return fmt.Errorf("mavlinktest: endpoint channel not open within %v", openWithin)
		}
	}
}

// Addr is the TCP address a ground station connects to, empty when the
// caller chose the endpoint.
func (a *Autopilot) Addr() string { return a.addr }

// Close stops the autopilot. Calling it again does nothing.
func (a *Autopilot) Close() {
	a.once.Do(func() {
		close(a.stop)
		<-a.done
		a.node.Close()
	})
}

func (a *Autopilot) rover() bool { return a.opts.Type == minimal.MAV_TYPE_GROUND_ROVER }

func (a *Autopilot) guided() uint32 {
	if a.rover() {
		return uint32(ardupilotmega.ROVER_MODE_GUIDED)
	}
	return uint32(ardupilotmega.COPTER_MODE_GUIDED)
}

func (a *Autopilot) onGround() bool { return a.rover() || a.amsl <= a.homeAMSL+0.05 }

// run answers commands and streams telemetry until Close. The node's events
// are drained in the same loop: gomavlib stalls every channel while they are
// not.
func (a *Autopilot) run() {
	defer close(a.done)
	t := time.NewTicker(tick)
	defer t.Stop()
	last := time.Now()
	for n := 0; ; {
		select {
		case <-a.stop:
			return
		case evt := <-a.node.Events():
			if e, ok := evt.(*gomavlib.EventFrame); ok {
				switch m := e.Message().(type) {
				case *common.MessageCommandInt:
					if m.TargetSystem == a.opts.SystemID {
						a.command(m, e.SystemID(), e.ComponentID())
					}
				case *common.MessageParamSet:
					if m.TargetSystem == a.opts.SystemID {
						a.param(m)
					}
				}
			}
		case now := <-t.C:
			a.fly(now.Sub(last).Seconds())
			last = now
			a.emit(n%5 == 0)
			n++
		}
	}
}

// command applies one COMMAND_INT and answers it, unless the test refused or
// muted the command.
func (a *Autopilot) command(m *common.MessageCommandInt, from, comp uint8) {
	a.mu.Lock()
	a.cmds = append(a.cmds, *m)
	res, refused := a.refuse[m.Command]
	delete(a.refuse, m.Command)
	var reply message.Message
	if !refused {
		res, reply = a.apply(m)
	}
	mute := a.mute[m.Command]
	a.mu.Unlock()
	if !mute {
		_ = a.node.WriteMessageAll(&common.MessageCommandAck{Command: m.Command, Result: res, TargetSystem: from, TargetComponent: comp})
	}
	if reply != nil {
		_ = a.node.WriteMessageAll(reply)
	}
}

// param applies a PARAM_SET of SIM_SPEEDUP on a SITL and answers it with the
// value now held. Any other parameter is not modelled and goes unanswered.
func (a *Autopilot) param(m *common.MessageParamSet) {
	a.mu.Lock()
	a.paramSets++
	if !a.opts.SITL || strings.TrimRight(m.ParamId, "\x00") != "SIM_SPEEDUP" {
		a.mu.Unlock()
		return
	}
	a.speedup = m.ParamValue
	v := a.speedup
	a.mu.Unlock()
	_ = a.node.WriteMessageAll(&common.MessageParamValue{ParamId: "SIM_SPEEDUP", ParamValue: v, ParamType: common.MAV_PARAM_TYPE_REAL32, ParamCount: 1})
}

// apply performs a command, returning its result and the message it asked
// for, if any.
func (a *Autopilot) apply(m *common.MessageCommandInt) (common.MAV_RESULT, message.Message) {
	switch m.Command {
	case common.MAV_CMD_SET_MESSAGE_INTERVAL:
		if m.Param3 != 0 {
			return common.MAV_RESULT_DENIED, nil
		}
		a.intervals[uint32(m.Param1)] = int32(m.Param2)
	case common.MAV_CMD_REQUEST_MESSAGE:
		if uint32(m.Param1) != (&common.MessageHomePosition{}).GetID() {
			return common.MAV_RESULT_DENIED, nil
		}
		a.homeAsked++
		return common.MAV_RESULT_ACCEPTED, &common.MessageHomePosition{Latitude: E7(a.homeLat), Longitude: E7(a.homeLon), Altitude: int32(math.Round(a.homeAMSL * 1000))}
	case common.MAV_CMD_DO_SET_MODE:
		a.mode = uint32(m.Param2)
	case common.MAV_CMD_COMPONENT_ARM_DISARM:
		a.armed = m.Param1 == 1
		if a.armed {
			// ArduPilot moves home to where the vehicle arms.
			a.homeLat, a.homeLon, a.homeAMSL = a.lat, a.lon, a.amsl
		}
	case common.MAV_CMD_NAV_TAKEOFF:
		if m.Frame != common.MAV_FRAME_GLOBAL_RELATIVE_ALT {
			return common.MAV_RESULT_DENIED, nil
		}
		if a.rover() || !a.armed || !a.onGround() || a.mode != a.guided() {
			return common.MAV_RESULT_FAILED, nil
		}
		to := a.homeAMSL + float64(m.Z)
		a.climbTo = &to
	case common.MAV_CMD_DO_REPOSITION:
		if a.mode != a.guided() && int(m.Param2)&int(common.MAV_DO_REPOSITION_FLAGS_CHANGE_MODE) == 0 {
			return common.MAV_RESULT_DENIED, nil
		}
		a.mode = a.guided()
		a.target = &[3]float64{float64(m.X) / 1e7, float64(m.Y) / 1e7, float64(m.Z)}
		a.climbTo = nil
		if m.Param1 > 0 {
			a.speedSet = float64(m.Param1)
		}
	case common.MAV_CMD_DO_CHANGE_SPEED:
		a.speedSet = float64(m.Param2)
	case common.MAV_CMD_NAV_LAND:
		if a.rover() {
			return common.MAV_RESULT_UNSUPPORTED, nil
		}
		a.mode = uint32(ardupilotmega.COPTER_MODE_LAND)
	case common.MAV_CMD_NAV_RETURN_TO_LAUNCH:
		if a.rover() {
			a.mode = uint32(ardupilotmega.ROVER_MODE_RTL)
		} else {
			a.mode = uint32(ardupilotmega.COPTER_MODE_RTL)
		}
		a.target = nil
	default:
		return common.MAV_RESULT_UNSUPPORTED, nil
	}
	return common.MAV_RESULT_ACCEPTED, nil
}

// fly moves the point mass for dt seconds.
func (a *Autopilot) fly(dt float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.vn, a.ve = 0, 0
	if !a.armed {
		return
	}
	switch {
	case a.mode == a.guided() && a.climbTo != nil:
		a.amsl = math.Min(a.amsl+climb*dt, *a.climbTo)
		if a.amsl == *a.climbTo {
			a.climbTo = nil
		}
	case a.mode == a.guided() && a.target != nil && (a.rover() || !a.onGround()):
		a.toward(a.target[0], a.target[1], a.target[2], dt)
	case a.mode == uint32(ardupilotmega.COPTER_MODE_LAND) && !a.rover():
		a.descend(dt)
	case a.mode == uint32(ardupilotmega.COPTER_MODE_RTL) && !a.rover(),
		a.mode == uint32(ardupilotmega.ROVER_MODE_RTL) && a.rover():
		if a.lat != a.homeLat || a.lon != a.homeLon {
			a.toward(a.homeLat, a.homeLon, a.amsl, dt)
		} else if !a.rover() {
			a.descend(dt)
		}
	}
}

func (a *Autopilot) descend(dt float64) {
	a.amsl = math.Max(a.amsl-climb*dt, a.homeAMSL)
	if a.amsl == a.homeAMSL {
		a.armed = false
	}
}

func (a *Autopilot) toward(lat, lon, amsl, dt float64) {
	mLat := domain.MetresPerDegreeLat()
	mLon := domain.MetresPerDegreeLon(a.lat)
	dn, de, dz := (lat-a.lat)*mLat, (lon-a.lon)*mLon, amsl-a.amsl
	d := math.Sqrt(dn*dn + de*de + dz*dz)
	step := speed * dt
	if d <= step {
		a.lat, a.lon, a.amsl = lat, lon, amsl
		return
	}
	k := step / d
	a.lat += dn * k / mLat
	a.lon += de * k / mLon
	a.amsl += dz * k
	a.vn, a.ve = speed*dn/d, speed*de/d
}

// emit streams one round of telemetry, the heartbeat when asked.
func (a *Autopilot) emit(heartbeat bool) {
	a.mu.Lock()
	if a.silent {
		a.mu.Unlock()
		return
	}
	var msgs []message.Message
	if heartbeat {
		base := minimal.MAV_MODE_FLAG_CUSTOM_MODE_ENABLED
		if a.armed {
			base |= minimal.MAV_MODE_FLAG_SAFETY_ARMED
		}
		msgs = append(msgs, &minimal.MessageHeartbeat{Type: a.opts.Type, Autopilot: a.opts.Autopilot, BaseMode: base, CustomMode: a.mode, SystemStatus: minimal.MAV_STATE_ACTIVE, MavlinkVersion: 3})
	}
	// Without an estimate ArduPilot sends the Location it last had, (0, 0)
	// before the first (GCS_MAVLINK::send_global_position_int).
	ekf := ardupilotmega.EKF_ATTITUDE | ardupilotmega.EKF_VELOCITY_HORIZ | ardupilotmega.EKF_VELOCITY_VERT |
		ardupilotmega.EKF_POS_VERT_ABS | ardupilotmega.EKF_POS_HORIZ_ABS | ardupilotmega.EKF_PRED_POS_HORIZ_ABS
	if a.noFix {
		ekf = ardupilotmega.EKF_ATTITUDE | ardupilotmega.EKF_CONST_POS_MODE
	} else {
		a.sentLat, a.sentLon = a.lat, a.lon
	}
	msgs = append(msgs, &common.MessageGlobalPositionInt{
		TimeBootMs:  a.bootBase + uint32(time.Since(a.booted).Milliseconds()),
		Lat:         E7(a.sentLat),
		Lon:         E7(a.sentLon),
		Alt:         int32(math.Round(a.amsl * 1000)),
		RelativeAlt: int32(math.Round((a.amsl - a.homeAMSL) * 1000)),
		Vx:          int16(a.vn * 100),
		Vy:          int16(a.ve * 100),
		Hdg:         9000,
	})
	remaining := int8(80)
	if a.opts.NoBattery {
		remaining = -1
	}
	msgs = append(msgs, &common.MessageSysStatus{BatteryRemaining: remaining})
	if !a.rover() {
		landed := common.MAV_LANDED_STATE_IN_AIR
		if a.onGround() {
			landed = common.MAV_LANDED_STATE_ON_GROUND
		}
		msgs = append(msgs, &common.MessageExtendedSysState{LandedState: landed})
	}
	msgs = append(msgs, &ardupilotmega.MessageEkfStatusReport{Flags: ekf})
	a.mu.Unlock()
	for _, m := range msgs {
		_ = a.node.WriteMessageAll(m)
	}
}

// E7 is a coordinate as MAVLink carries it, degrees times 10^7.
func E7(d float64) int32 { return int32(math.Round(d * 1e7)) }

// SetSilent stops or resumes every message the autopilot sends.
func (a *Autopilot) SetSilent(silent bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.silent = silent
}

// SetNoPosition drops or restores the position estimate, a GPS lost and found
// again: the vehicle keeps flying, its reports repeat where it last knew it
// was.
func (a *Autopilot) SetNoPosition(lost bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.noFix = lost
}

// Reboot restarts the autopilot where it stands: boot time from zero,
// disarmed, every command and stream interval forgotten, SIM_SPEEDUP back to
// 1 as a SITL restarted with --wipe --speedup 1.
func (a *Autopilot) Reboot() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.booted, a.bootBase = time.Now(), 0
	a.armed, a.target, a.climbTo = false, nil, nil
	a.intervals = map[uint32]int32{}
	a.speedup = 1
}

// SimSpeed is SIM_SPEEDUP and how many PARAM_SET the autopilot received.
func (a *Autopilot) SimSpeed() (speedup float32, paramSets int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.speedup, a.paramSets
}

// RefuseNext answers the next cmd with res, without applying it.
func (a *Autopilot) RefuseNext(cmd common.MAV_CMD, res common.MAV_RESULT) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refuse[cmd] = res
}

// MuteAcks applies cmd from now on without answering it.
func (a *Autopilot) MuteAcks(cmd common.MAV_CMD) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mute[cmd] = true
}

// FlightCommands is every COMMAND_INT received, in order, the stream and home
// requests left out.
func (a *Autopilot) FlightCommands() []common.MessageCommandInt {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []common.MessageCommandInt
	for _, c := range a.cmds {
		if c.Command != common.MAV_CMD_SET_MESSAGE_INTERVAL && c.Command != common.MAV_CMD_REQUEST_MESSAGE {
			out = append(out, c)
		}
	}
	return out
}

// FlightCommandIDs is FlightCommands, the command ids only.
func (a *Autopilot) FlightCommandIDs() []common.MAV_CMD {
	var out []common.MAV_CMD
	for _, c := range a.FlightCommands() {
		out = append(out, c.Command)
	}
	return out
}

// Last is the latest received cmd, the zero message when none was.
func (a *Autopilot) Last(cmd common.MAV_CMD) common.MessageCommandInt {
	cmds := a.FlightCommands()
	for i := len(cmds) - 1; i >= 0; i-- {
		if cmds[i].Command == cmd {
			return cmds[i]
		}
	}
	return common.MessageCommandInt{}
}

// Interval is the interval, in microseconds, last asked for a message id.
func (a *Autopilot) Interval(id uint32) int32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.intervals[id]
}

// State reports whether the motors are armed, how many times home was asked
// for, and the last ground speed set.
func (a *Autopilot) State() (armed bool, homeAsked int, speedSet float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.armed, a.homeAsked, a.speedSet
}
