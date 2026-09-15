// Package mavlink drives ArduPilot vehicles over MAVLink v2 as KEEL vectors:
// ArduCopter as aerial, ArduRover as ground (spec section 7.4).
//
// It is the integration layer's case against a real autopilot (design section
// 7.4). A Link owns one MAVLink node over its endpoints, a telemetry radio or
// the UDP port a SITL pushes to, and routes frames by system id to one Adapter
// per vehicle. Each Adapter is a transport plus a vector.Agent, like
// adapters/native: the Agent holds the contract's mechanics, the Adapter holds
// what MAVLink does not say for itself.
//
//   - MAVLink carries no Seq. A command is applied when every COMMAND_INT it
//     sent was answered MAV_RESULT_ACCEPTED, and the Adapter fills AckSeq with
//     it. The Adapter never retries on its own: the engine re-sends what is not
//     acknowledged (spec section 7.2).
//   - MAVLink declares no capabilities. They come from configuration, and the
//     heartbeat's vehicle type is held to the declared domain.
//   - ArduPilot speaks AMSL, KEEL ellipsoid heights (spec section 3). The
//     configured geoid separation converts both ways.
//   - Telemetry reports a physical mode, derived here from the command the
//     vehicle applied and the autopilot's own mode.
//
// This file holds the pure translations; adapter.go and link.go hold the
// state and the goroutines.
package mavlink

import (
	"math"

	"github.com/bluenviron/gomavlib/v4/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v4/pkg/dialects/common"
	"github.com/bluenviron/gomavlib/v4/pkg/dialects/minimal"

	"github.com/dsanchez31/keel/vector"
)

// kind is the ArduPilot firmware a vehicle runs, read off its heartbeat.
type kind int

const (
	kindUnknown kind = iota
	kindCopter
	kindRover
)

func (k kind) String() string {
	switch k {
	case kindCopter:
		return "ArduCopter"
	case kindRover:
		return "ArduRover"
	default:
		return "unknown vehicle"
	}
}

// domain is the medium the firmware operates in.
func (k kind) domain() vector.Domain {
	if k == kindRover {
		return vector.DomainGround
	}
	return vector.DomainAerial
}

// vehicleKind maps a heartbeat's MAV_TYPE to the firmware that reports it.
// The rotorcraft types are the frames ArduCopter builds for; a boat runs
// ArduRover too but is out of scope (spec section 1).
func vehicleKind(t minimal.MAV_TYPE) kind {
	switch t {
	case minimal.MAV_TYPE_QUADROTOR, minimal.MAV_TYPE_HEXAROTOR, minimal.MAV_TYPE_OCTOROTOR,
		minimal.MAV_TYPE_TRICOPTER, minimal.MAV_TYPE_COAXIAL, minimal.MAV_TYPE_HELICOPTER,
		minimal.MAV_TYPE_DECAROTOR, minimal.MAV_TYPE_DODECAROTOR:
		return kindCopter
	case minimal.MAV_TYPE_GROUND_ROVER:
		return kindRover
	default:
		return kindUnknown
	}
}

// heartbeat is what the adapter keeps of the latest HEARTBEAT.
type heartbeat struct {
	kind      kind
	ardupilot bool
	armed     bool
	mode      uint32
}

func readHeartbeat(m *minimal.MessageHeartbeat) heartbeat {
	return heartbeat{
		kind:      vehicleKind(m.Type),
		ardupilot: m.Autopilot == minimal.MAV_AUTOPILOT_ARDUPILOTMEGA,
		armed:     m.BaseMode&minimal.MAV_MODE_FLAG_SAFETY_ARMED != 0,
		mode:      m.CustomMode,
	}
}

// autopilotMode is what the adapter needs to know of an ArduPilot mode.
type autopilotMode int

const (
	// modeOther is any mode KEEL did not put the vehicle in: a pilot, another
	// ground station or a mission has it.
	modeOther autopilotMode = iota
	modeGuided
	// modeReturning is the autopilot bringing the vehicle home or down on its
	// own: an rtb without a station, or a failsafe.
	modeReturning
)

// classifyMode reads a heartbeat's custom mode under the firmware's table.
func classifyMode(k kind, custom uint32) autopilotMode {
	switch k {
	case kindCopter:
		switch ardupilotmega.COPTER_MODE(custom) {
		case ardupilotmega.COPTER_MODE_GUIDED:
			return modeGuided
		case ardupilotmega.COPTER_MODE_RTL, ardupilotmega.COPTER_MODE_SMART_RTL,
			ardupilotmega.COPTER_MODE_AUTO_RTL, ardupilotmega.COPTER_MODE_LAND:
			return modeReturning
		}
	case kindRover:
		switch ardupilotmega.ROVER_MODE(custom) {
		case ardupilotmega.ROVER_MODE_GUIDED:
			return modeGuided
		case ardupilotmega.ROVER_MODE_RTL, ardupilotmega.ROVER_MODE_SMART_RTL:
			return modeReturning
		}
	}
	return modeOther
}

// guidedMode is the firmware's custom mode number for GUIDED.
func guidedMode(k kind) uint32 {
	if k == kindRover {
		return uint32(ardupilotmega.ROVER_MODE_GUIDED)
	}
	return uint32(ardupilotmega.COPTER_MODE_GUIDED)
}

// fix is one GLOBAL_POSITION_INT, converted.
type fix struct {
	bootMs uint32
	// pos is the ellipsoid position. amslM and relM are the altitude as the
	// autopilot states it, above mean sea level and above home.
	pos   vector.Position
	amslM float64
	relM  float64
	speed float64
	// heading is nil when the autopilot does not know it.
	heading *float64
}

// readFix converts a GLOBAL_POSITION_INT. sepM is the geoid separation, the
// ellipsoid height of mean sea level at the operating area: ellipsoid height
// is AMSL plus it.
func readFix(m *common.MessageGlobalPositionInt, sepM float64) fix {
	amsl := float64(m.Alt) / 1000
	f := fix{
		bootMs: m.TimeBootMs,
		pos:    vector.Position{Lat: fromDegE7(m.Lat), Lon: fromDegE7(m.Lon), AltM: amsl + sepM},
		amslM:  amsl,
		relM:   float64(m.RelativeAlt) / 1000,
		speed:  math.Hypot(float64(m.Vx), float64(m.Vy)) / 100,
	}
	if m.Hdg != math.MaxUint16 {
		h := float64(m.Hdg) / 100
		f.heading = &h
	}
	return f
}

func fromDegE7(v int32) float64 { return float64(v) / 1e7 }
func toDegE7(d float64) int32   { return int32(math.Round(d * 1e7)) }

// landed reports whether a copter's EXTENDED_SYS_STATE puts it on the ground.
// UNDEFINED, a copter that never sent the message, says nothing.
func landed(s common.MAV_LANDED_STATE) bool { return s == common.MAV_LANDED_STATE_ON_GROUND }

// battery converts SYS_STATUS.battery_remaining, false when the autopilot does
// not know it (-1, no battery monitor).
func battery(remaining int8) (int, bool) {
	if remaining < 0 || remaining > 100 {
		return 0, false
	}
	return int(remaining), true
}

// positionKnown reports whether EKF_STATUS_REPORT's flags give the autopilot
// an absolute position, the rule ArduCopter navigates by
// (Copter::ekf_has_absolute_position): a predicted one is enough on the
// ground, a flying vehicle needs a measured one outside constant position
// mode. Without it, GLOBAL_POSITION_INT carries a stale position, (0, 0)
// before the first. A relative position, which Rover would accept, is not a
// WGS-84 one.
func positionKnown(flags ardupilotmega.EKF_STATUS_FLAGS, armed bool) bool {
	if !armed {
		return flags&(ardupilotmega.EKF_POS_HORIZ_ABS|ardupilotmega.EKF_PRED_POS_HORIZ_ABS) != 0
	}
	return flags&ardupilotmega.EKF_POS_HORIZ_ABS != 0 && flags&ardupilotmega.EKF_CONST_POS_MODE == 0
}

// modeInputs is what the physical mode is derived from.
type modeInputs struct {
	armed bool
	// onGround is disarmed, or a copter whose EXTENDED_SYS_STATE says landed.
	onGround bool
	mode     autopilotMode
	// applied is the type of the last command the vehicle applied, empty
	// before any.
	applied vector.CommandType
	// toStation is an applied rtb flying to its station; arrived is set once
	// the vehicle is within the station radius of it, or of home for a rover
	// the autopilot returns.
	toStation bool
	arrived   bool
	// launching is a goto whose takeoff is under way.
	launching bool
	// unconfirmed is an applied command whose steps the autopilot accepted
	// with no heartbeat heard since: armed and mode predate them, by up to
	// a second at ArduPilot's 1 Hz.
	unconfirmed bool
}

// physicalMode derives the mode a frame reports, as keelsim does (spec
// section 15.2): transit toward a commanded point, station keeping on arrival
// included; idle stopped; rtb on the way home, idle on arrival.
func physicalMode(in modeInputs) vector.VectorMode {
	switch {
	case in.launching && in.armed:
		return vector.ModeTransit
	case in.unconfirmed:
		// Every COMMAND_ACK was MAV_RESULT_ACCEPTED: the command says what the
		// vehicle does until a heartbeat says it too.
		switch {
		case in.applied == vector.CommandGoto:
			return vector.ModeTransit
		case in.applied == vector.CommandRTB && !in.arrived:
			return vector.ModeRTB
		}
		return vector.ModeIdle
	case in.mode == modeReturning:
		if in.onGround || in.arrived {
			return vector.ModeIdle
		}
		return vector.ModeRTB
	case in.onGround:
		return vector.ModeIdle
	case in.mode != modeGuided:
		// A pilot or another station has the vehicle: it is not doing KEEL's
		// work, whatever it is doing.
		return vector.ModeIdle
	}
	switch in.applied {
	case vector.CommandGoto:
		return vector.ModeTransit
	case vector.CommandRTB:
		if in.toStation && !in.arrived {
			return vector.ModeRTB
		}
	}
	return vector.ModeIdle
}

// commandInt addresses a COMMAND_INT to a vehicle's autopilot.
func commandInt(sysid uint8, cmd common.MAV_CMD) *common.MessageCommandInt {
	return &common.MessageCommandInt{
		TargetSystem:    sysid,
		TargetComponent: uint8(minimal.MAV_COMP_ID_AUTOPILOT1),
		Frame:           common.MAV_FRAME_GLOBAL,
		Command:         cmd,
	}
}

// repositionTo flies the vehicle to a point in GUIDED, switching to GUIDED if
// it is not in it. amslM is the altitude above mean sea level. speed is the
// ground speed in m/s, which ArduRover reads and ArduCopter ignores; negative
// means unchanged.
func repositionTo(sysid uint8, lat, lon, amslM, speed float64) *common.MessageCommandInt {
	m := commandInt(sysid, common.MAV_CMD_DO_REPOSITION)
	m.Param1 = float32(speed)
	m.Param2 = float32(common.MAV_DO_REPOSITION_FLAGS_CHANGE_MODE)
	// NaN is "unchanged" for the yaw of a reposition.
	m.Param4 = float32(math.NaN())
	m.X, m.Y, m.Z = toDegE7(lat), toDegE7(lon), float32(amslM)
	return m
}

// changeSpeed sets the ground speed of the current mode.
func changeSpeed(sysid uint8, speed float64) *common.MessageCommandInt {
	m := commandInt(sysid, common.MAV_CMD_DO_CHANGE_SPEED)
	m.Param1 = float32(common.SPEED_TYPE_GROUNDSPEED)
	m.Param2 = float32(speed)
	m.Param3 = -1
	return m
}

// setMode switches to a custom mode.
func setMode(sysid uint8, custom uint32) *common.MessageCommandInt {
	m := commandInt(sysid, common.MAV_CMD_DO_SET_MODE)
	m.Param1 = float32(minimal.MAV_MODE_FLAG_CUSTOM_MODE_ENABLED)
	m.Param2 = float32(custom)
	return m
}

// arm arms the motors, with the autopilot's pre-arm checks.
func arm(sysid uint8) *common.MessageCommandInt {
	m := commandInt(sysid, common.MAV_CMD_COMPONENT_ARM_DISARM)
	m.Param1 = 1
	return m
}

// takeoff climbs a copter in GUIDED to relM above home. ArduCopter accepts
// the command in the home-relative frame only.
func takeoff(sysid uint8, relM float64) *common.MessageCommandInt {
	m := commandInt(sysid, common.MAV_CMD_NAV_TAKEOFF)
	m.Frame = common.MAV_FRAME_GLOBAL_RELATIVE_ALT
	m.Z = float32(relM)
	return m
}

// land lands a copter where it is.
func land(sysid uint8) *common.MessageCommandInt {
	return commandInt(sysid, common.MAV_CMD_NAV_LAND)
}

// returnToLaunch hands the vehicle to the autopilot's own RTL.
func returnToLaunch(sysid uint8) *common.MessageCommandInt {
	return commandInt(sysid, common.MAV_CMD_NAV_RETURN_TO_LAUNCH)
}

// messageInterval asks for a message at a fixed interval.
func messageInterval(sysid uint8, id uint32, intervalUs int32) *common.MessageCommandInt {
	m := commandInt(sysid, common.MAV_CMD_SET_MESSAGE_INTERVAL)
	m.Param1 = float32(id)
	m.Param2 = float32(intervalUs)
	return m
}

// paramSet sets a REAL32 parameter. The autopilot answers with PARAM_VALUE.
func paramSet(sysid uint8, id string, v float32) *common.MessageParamSet {
	return &common.MessageParamSet{
		TargetSystem:    sysid,
		TargetComponent: uint8(minimal.MAV_COMP_ID_AUTOPILOT1),
		ParamId:         id,
		ParamValue:      v,
		ParamType:       common.MAV_PARAM_TYPE_REAL32,
	}
}

// requestMessage asks for one instance of a message.
func requestMessage(sysid uint8, id uint32) *common.MessageCommandInt {
	m := commandInt(sysid, common.MAV_CMD_REQUEST_MESSAGE)
	m.Param1 = float32(id)
	return m
}
