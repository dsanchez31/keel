package mavlink

import (
	"math"
	"testing"

	"github.com/bluenviron/gomavlib/v4/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v4/pkg/dialects/common"
	"github.com/bluenviron/gomavlib/v4/pkg/dialects/minimal"

	"github.com/dsanchez31/keel/vector"
)

func TestVehicleKind(t *testing.T) {
	cases := []struct {
		typ    minimal.MAV_TYPE
		want   kind
		domain vector.Domain
	}{
		{minimal.MAV_TYPE_QUADROTOR, kindCopter, vector.DomainAerial},
		{minimal.MAV_TYPE_HEXAROTOR, kindCopter, vector.DomainAerial},
		{minimal.MAV_TYPE_HELICOPTER, kindCopter, vector.DomainAerial},
		{minimal.MAV_TYPE_GROUND_ROVER, kindRover, vector.DomainGround},
		{minimal.MAV_TYPE_FIXED_WING, kindUnknown, vector.DomainAerial},
		{minimal.MAV_TYPE_SURFACE_BOAT, kindUnknown, vector.DomainAerial},
		{minimal.MAV_TYPE_GCS, kindUnknown, vector.DomainAerial},
	}
	for _, c := range cases {
		k := vehicleKind(c.typ)
		if k != c.want {
			t.Errorf("%v: kind %v, want %v", c.typ, k, c.want)
		}
		if k != kindUnknown && k.domain() != c.domain {
			t.Errorf("%v: domain %v, want %v", c.typ, k.domain(), c.domain)
		}
	}
}

func TestReadHeartbeat(t *testing.T) {
	hb := readHeartbeat(&minimal.MessageHeartbeat{
		Type:       minimal.MAV_TYPE_QUADROTOR,
		Autopilot:  minimal.MAV_AUTOPILOT_ARDUPILOTMEGA,
		BaseMode:   minimal.MAV_MODE_FLAG_SAFETY_ARMED | minimal.MAV_MODE_FLAG_CUSTOM_MODE_ENABLED,
		CustomMode: uint32(ardupilotmega.COPTER_MODE_GUIDED),
	})
	want := heartbeat{kind: kindCopter, ardupilot: true, armed: true, mode: uint32(ardupilotmega.COPTER_MODE_GUIDED)}
	if hb != want {
		t.Fatalf("heartbeat %+v, want %+v", hb, want)
	}
	px4 := readHeartbeat(&minimal.MessageHeartbeat{Type: minimal.MAV_TYPE_QUADROTOR, Autopilot: minimal.MAV_AUTOPILOT_PX4})
	if px4.ardupilot || px4.armed {
		t.Fatalf("PX4 disarmed heartbeat read as %+v", px4)
	}
}

func TestReadFixAppliesTheDatum(t *testing.T) {
	m := &common.MessageGlobalPositionInt{
		TimeBootMs:  42000,
		Lat:         450268300,
		Lon:         50050600,
		Alt:         412500, // mm AMSL
		RelativeAlt: 30000,
		Vx:          300, // cm/s north
		Vy:          400, // cm/s east
		Hdg:         9050,
	}
	f := readFix(m, 49.5)
	if f.bootMs != 42000 {
		t.Errorf("boot %d ms", f.bootMs)
	}
	if f.pos.Lat != 45.02683 || f.pos.Lon != 5.00506 {
		t.Errorf("position %v, %v", f.pos.Lat, f.pos.Lon)
	}
	if f.amslM != 412.5 || f.pos.AltM != 462 || f.relM != 30 {
		t.Errorf("altitudes: amsl %v, ellipsoid %v, relative %v", f.amslM, f.pos.AltM, f.relM)
	}
	if f.speed != 5 {
		t.Errorf("speed %v, want 5 m/s", f.speed)
	}
	if f.heading == nil || *f.heading != 90.5 {
		t.Errorf("heading %v, want 90.5", f.heading)
	}

	m.Hdg = math.MaxUint16
	if f := readFix(m, 0); f.heading != nil {
		t.Errorf("unknown heading read as %v", *f.heading)
	}
}

func TestDegE7RoundTrip(t *testing.T) {
	for _, d := range []float64{45.02683, -33.8688197, 5.00506, 179.9999999, -180} {
		if got := fromDegE7(toDegE7(d)); math.Abs(got-d) > 5e-8 {
			t.Errorf("%v round trips to %v", d, got)
		}
	}
}

func TestBattery(t *testing.T) {
	cases := []struct {
		in    int8
		want  int
		known bool
	}{{80, 80, true}, {0, 0, true}, {100, 100, true}, {-1, 0, false}, {101, 0, false}}
	for _, c := range cases {
		got, known := battery(c.in)
		if got != c.want || known != c.known {
			t.Errorf("battery(%d) = %d, %v; want %d, %v", c.in, got, known, c.want, c.known)
		}
	}
}

func TestPositionKnown(t *testing.T) {
	const (
		abs       = ardupilotmega.EKF_POS_HORIZ_ABS
		predicted = ardupilotmega.EKF_PRED_POS_HORIZ_ABS
		relative  = ardupilotmega.EKF_POS_HORIZ_REL
		constPos  = ardupilotmega.EKF_CONST_POS_MODE
		attitude  = ardupilotmega.EKF_ATTITUDE
	)
	cases := []struct {
		name  string
		flags ardupilotmega.EKF_STATUS_FLAGS
		armed bool
		want  bool
	}{
		{"on the ground, absolute", attitude | abs, false, true},
		{"on the ground, predicted only", attitude | predicted, false, true},
		{"on the ground, relative only", attitude | relative, false, false},
		{"on the ground, no estimate", attitude, false, false},
		{"flying, absolute", attitude | abs, true, true},
		{"flying, absolute in constant position mode", attitude | abs | constPos, true, false},
		{"flying, predicted only", attitude | predicted, true, false},
		{"flying, relative only", attitude | relative, true, false},
	}
	for _, c := range cases {
		if got := positionKnown(c.flags, c.armed); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestClassifyMode(t *testing.T) {
	cases := []struct {
		k    kind
		mode uint32
		want autopilotMode
	}{
		{kindCopter, uint32(ardupilotmega.COPTER_MODE_GUIDED), modeGuided},
		{kindCopter, uint32(ardupilotmega.COPTER_MODE_RTL), modeReturning},
		{kindCopter, uint32(ardupilotmega.COPTER_MODE_SMART_RTL), modeReturning},
		{kindCopter, uint32(ardupilotmega.COPTER_MODE_AUTO_RTL), modeReturning},
		{kindCopter, uint32(ardupilotmega.COPTER_MODE_LAND), modeReturning},
		{kindCopter, uint32(ardupilotmega.COPTER_MODE_LOITER), modeOther},
		{kindCopter, uint32(ardupilotmega.COPTER_MODE_STABILIZE), modeOther},
		{kindRover, uint32(ardupilotmega.ROVER_MODE_GUIDED), modeGuided},
		{kindRover, uint32(ardupilotmega.ROVER_MODE_RTL), modeReturning},
		{kindRover, uint32(ardupilotmega.ROVER_MODE_SMART_RTL), modeReturning},
		{kindRover, uint32(ardupilotmega.ROVER_MODE_HOLD), modeOther},
		// The numbers differ between firmwares: copter GUIDED is rover HOLD.
		{kindRover, uint32(ardupilotmega.COPTER_MODE_GUIDED), modeOther},
		{kindUnknown, uint32(ardupilotmega.COPTER_MODE_GUIDED), modeOther},
	}
	for _, c := range cases {
		if got := classifyMode(c.k, c.mode); got != c.want {
			t.Errorf("%v mode %d: %v, want %v", c.k, c.mode, got, c.want)
		}
	}
	if guidedMode(kindCopter) != 4 || guidedMode(kindRover) != 15 {
		t.Errorf("GUIDED numbers: copter %d, rover %d", guidedMode(kindCopter), guidedMode(kindRover))
	}
}

func TestPhysicalMode(t *testing.T) {
	flying := modeInputs{armed: true, mode: modeGuided}
	with := func(edit func(*modeInputs)) modeInputs {
		in := flying
		edit(&in)
		return in
	}
	cases := []struct {
		name string
		in   modeInputs
		want vector.VectorMode
	}{
		{"guided before any command", flying, vector.ModeIdle},
		{"goto applied", with(func(in *modeInputs) { in.applied = vector.CommandGoto }), vector.ModeTransit},
		{"hold applied", with(func(in *modeInputs) { in.applied = vector.CommandHold }), vector.ModeIdle},
		{"abort applied", with(func(in *modeInputs) { in.applied = vector.CommandAbort }), vector.ModeIdle},
		{"rtb to a station", with(func(in *modeInputs) { in.applied, in.toStation = vector.CommandRTB, true }), vector.ModeRTB},
		{"rtb arrived at its station", with(func(in *modeInputs) { in.applied, in.toStation, in.arrived = vector.CommandRTB, true, true }), vector.ModeIdle},
		{"autopilot returning", with(func(in *modeInputs) { in.mode = modeReturning }), vector.ModeRTB},
		{"failsafe during a goto", with(func(in *modeInputs) { in.mode, in.applied = modeReturning, vector.CommandGoto }), vector.ModeRTB},
		{"rover home by RTL", with(func(in *modeInputs) { in.mode, in.arrived = modeReturning, true }), vector.ModeIdle},
		{"landed by RTL", with(func(in *modeInputs) { in.mode, in.onGround, in.armed = modeReturning, true, false }), vector.ModeIdle},
		{"pilot took over", with(func(in *modeInputs) { in.mode, in.applied = modeOther, vector.CommandGoto }), vector.ModeIdle},
		{"disarmed after a goto", with(func(in *modeInputs) { in.armed, in.onGround, in.applied = false, true, vector.CommandGoto }), vector.ModeIdle},
		{"launch armed", with(func(in *modeInputs) { in.onGround, in.launching = true, true }), vector.ModeTransit},
		{"launch not armed yet", with(func(in *modeInputs) { in.armed, in.onGround, in.launching = false, true, true }), vector.ModeIdle},
		{"rover goto before the heartbeat", modeInputs{onGround: true, mode: modeOther, applied: vector.CommandGoto, unconfirmed: true}, vector.ModeTransit},
		{"goto from another mode before the heartbeat", with(func(in *modeInputs) { in.mode, in.applied, in.unconfirmed = modeOther, vector.CommandGoto, true }), vector.ModeTransit},
		{"rtb to a station before the heartbeat", with(func(in *modeInputs) { in.applied, in.toStation, in.unconfirmed = vector.CommandRTB, true, true }), vector.ModeRTB},
		{"rtl before the heartbeat", with(func(in *modeInputs) { in.applied, in.unconfirmed = vector.CommandRTB, true }), vector.ModeRTB},
		{"hold before the heartbeat", with(func(in *modeInputs) { in.mode, in.applied, in.unconfirmed = modeReturning, vector.CommandHold, true }), vector.ModeIdle},
	}
	for _, c := range cases {
		got := physicalMode(c.in)
		if got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
		if !vector.PhysicalMode(got) {
			t.Errorf("%s: %v is not a physical mode", c.name, got)
		}
	}
}

func TestCommandBuilders(t *testing.T) {
	r := repositionTo(7, 45.02683, 5.00506, 170.25, 4)
	switch {
	case r.TargetSystem != 7 || r.TargetComponent != uint8(minimal.MAV_COMP_ID_AUTOPILOT1):
		t.Errorf("reposition addressed to %d/%d", r.TargetSystem, r.TargetComponent)
	case r.Command != common.MAV_CMD_DO_REPOSITION || r.Frame != common.MAV_FRAME_GLOBAL:
		t.Errorf("reposition is %v in frame %v", r.Command, r.Frame)
	case r.X != 450268300 || r.Y != 50050600 || r.Z != 170.25:
		t.Errorf("reposition to %d, %d, %v", r.X, r.Y, r.Z)
	case r.Param1 != 4 || r.Param2 != float32(common.MAV_DO_REPOSITION_FLAGS_CHANGE_MODE):
		t.Errorf("reposition speed %v, flags %v", r.Param1, r.Param2)
	case !math.IsNaN(float64(r.Param4)):
		t.Errorf("reposition yaw %v, want NaN (unchanged)", r.Param4)
	}
	if tk := takeoff(7, 30); tk.Command != common.MAV_CMD_NAV_TAKEOFF || tk.Frame != common.MAV_FRAME_GLOBAL_RELATIVE_ALT || tk.Z != 30 {
		t.Errorf("takeoff %+v", tk)
	}
	if sm := setMode(7, 4); sm.Command != common.MAV_CMD_DO_SET_MODE || sm.Param1 != 1 || sm.Param2 != 4 {
		t.Errorf("set mode %+v", sm)
	}
	if ar := arm(7); ar.Command != common.MAV_CMD_COMPONENT_ARM_DISARM || ar.Param1 != 1 || ar.Param2 != 0 {
		t.Errorf("arm %+v, want param1 1 and the pre-arm checks kept", ar)
	}
	if cs := changeSpeed(7, 18); cs.Command != common.MAV_CMD_DO_CHANGE_SPEED || cs.Param1 != float32(common.SPEED_TYPE_GROUNDSPEED) || cs.Param2 != 18 {
		t.Errorf("change speed %+v", cs)
	}
	// ArduPilot denies SET_MESSAGE_INTERVAL with a non-zero param3.
	if mi := messageInterval(7, 33, 250000); mi.Command != common.MAV_CMD_SET_MESSAGE_INTERVAL || mi.Param1 != 33 || mi.Param2 != 250000 || mi.Param3 != 0 {
		t.Errorf("message interval %+v", mi)
	}
	if rm := requestMessage(7, 242); rm.Command != common.MAV_CMD_REQUEST_MESSAGE || rm.Param1 != 242 {
		t.Errorf("request message %+v", rm)
	}
	if l := land(7); l.Command != common.MAV_CMD_NAV_LAND {
		t.Errorf("land %+v", l)
	}
	if rtl := returnToLaunch(7); rtl.Command != common.MAV_CMD_NAV_RETURN_TO_LAUNCH {
		t.Errorf("return to launch %+v", rtl)
	}
}
