package mavlink_test

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v4"
	"github.com/bluenviron/gomavlib/v4/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v4/pkg/dialects/common"
	"github.com/bluenviron/gomavlib/v4/pkg/dialects/minimal"

	"github.com/dsanchez31/keel/adapters/mavlink"
	"github.com/dsanchez31/keel/adapters/mavlink/mavlinktest"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/vector"
)

// wait bounds every wait on the network. Generous: a slow CI machine under
// -race is not a failure.
const wait = 5 * time.Second

// sep is the geoid separation the tests configure: ellipsoid heights are
// AMSL plus it.
const sep = 49.5

// The positions the fake ArduPilot starts from.
const (
	padLat   = mavlinktest.PadLat
	padLon   = mavlinktest.PadLon
	padAMSL  = mavlinktest.PadAMSL
	fieldLat = mavlinktest.FieldLat
	fieldLon = mavlinktest.FieldLon
)

var e7 = mavlinktest.E7

// newFake starts a fake ArduPilot behind a TCP listener until the test ends.
func newFake(t *testing.T, o mavlinktest.Options) *mavlinktest.Autopilot {
	t.Helper()
	ap, err := mavlinktest.New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ap.Close)
	return ap
}

// newLink connects one link to every fake, one TCP client endpoint each.
func newLink(t *testing.T, fakes ...*mavlinktest.Autopilot) *mavlink.Link {
	t.Helper()
	var eps []gomavlib.Endpoint
	for _, f := range fakes {
		eps = append(eps, &gomavlib.EndpointTCPClient{Address: f.Addr()})
	}
	l, err := mavlink.NewLink(mavlink.LinkOptions{Endpoints: eps})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func copterCaps() vector.Capabilities {
	return vector.Capabilities{ID: "DRONE-01", Domain: vector.DomainAerial, Tags: []string{"gps", "camera"}, CruiseSpeed: 18, MaxRangeM: 60000, SensorRadiusM: 90}
}

func roverCaps() vector.Capabilities {
	return vector.Capabilities{ID: "UGV-01", Domain: vector.DomainGround, Tags: []string{"gps"}, CruiseSpeed: 6, MaxRangeM: 20000, SensorRadiusM: 30}
}

func newAdapter(t *testing.T, l *mavlink.Link, c mavlink.Config) *mavlink.Adapter {
	t.Helper()
	if c.GeoidSeparationM == 0 {
		c.GeoidSeparationM = sep
	}
	a, err := mavlink.NewAdapter(l, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// copter is a link to one fake copter and its adapter.
func copter(t *testing.T, o mavlinktest.Options, edit func(*mavlink.Config)) (*mavlinktest.Autopilot, *mavlink.Adapter) {
	t.Helper()
	o.SystemID = 1
	f := newFake(t, o)
	c := mavlink.Config{SystemID: 1, Capabilities: copterCaps()}
	if edit != nil {
		edit(&c)
	}
	return f, newAdapter(t, newLink(t, f), c)
}

// until reads frames until one satisfies cond.
func until(t *testing.T, a *mavlink.Adapter, what string, cond func(vector.VectorState) bool) vector.VectorState {
	t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case s, ok := <-a.Telemetry():
			if !ok {
				t.Fatalf("telemetry channel closed waiting for %s", what)
			}
			if cond(s) {
				return s
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func healthSays(a *mavlink.Adapter, kind vector.HealthKind, detail string) func() bool {
	return func() bool {
		h := a.Health()
		return h.Kind == kind && strings.Contains(h.Detail, detail)
	}
}

func cmd(id vector.VectorID, seq uint64, typ vector.CommandType, wp *vector.Position) vector.Command {
	return vector.Command{Vector: id, Seq: seq, Type: typ, Waypoint: wp}
}

func execute(t *testing.T, a *mavlink.Adapter, c vector.Command) {
	t.Helper()
	if err := a.Execute(c); err != nil {
		t.Fatalf("Execute %s seq %d: %v", c.Type, c.Seq, err)
	}
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestNewAdapterRefusesBadConfig(t *testing.T) {
	f := newFake(t, mavlinktest.Options{SystemID: 1})
	l := newLink(t, f)
	newAdapter(t, l, mavlink.Config{SystemID: 1, Capabilities: copterCaps()})
	cases := map[string]mavlink.Config{
		"broadcast system id":        {SystemID: 0, Capabilities: copterCaps()},
		"supports declared":          {SystemID: 2, Capabilities: copterCaps(), Agent: vector.Options{Supports: []vector.CommandType{vector.CommandGoto}}},
		"separation not finite":      {SystemID: 2, Capabilities: copterCaps(), GeoidSeparationM: math.NaN()},
		"capabilities without an id": {SystemID: 2, Capabilities: vector.Capabilities{Domain: vector.DomainAerial, CruiseSpeed: 1, MaxRangeM: 1}},
		"system id taken":            {SystemID: 1, Capabilities: copterCaps()},
	}
	for name, c := range cases {
		if a, err := mavlink.NewAdapter(l, c); err == nil {
			_ = a.Close()
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := mavlink.NewLink(mavlink.LinkOptions{}); err == nil {
		t.Error("link without an endpoint accepted")
	}
}

func TestFramesCarryTheVehicle(t *testing.T) {
	f, a := copter(t, mavlinktest.Options{}, nil)
	s := until(t, a, "a frame", func(vector.VectorState) bool { return true })
	switch {
	case s.ID != "DRONE-01" || s.Link != vector.LinkOK:
		t.Errorf("frame of %q, link %q", s.ID, s.Link)
	case !near(s.Position.Lat, padLat, 1e-7) || !near(s.Position.Lon, padLon, 1e-7):
		t.Errorf("position %v, %v", s.Position.Lat, s.Position.Lon)
	case !near(s.Position.AltM, padAMSL+sep, 1e-3):
		t.Errorf("altitude %v, want the ellipsoid height %v", s.Position.AltM, padAMSL+sep)
	case s.BatteryPct != 80 || s.Heading != 90:
		t.Errorf("battery %d, heading %v", s.BatteryPct, s.Heading)
	case s.Mode != vector.ModeIdle:
		t.Errorf("mode %q on the pad, want idle", s.Mode)
	case s.LastSeenMs != 0 || s.AckSeq != 0:
		t.Errorf("last seen %d, ack %d: want both 0", s.LastSeenMs, s.AckSeq)
	}
	for _, m := range []interface{ GetID() uint32 }{&common.MessageGlobalPositionInt{}, &common.MessageSysStatus{}, &common.MessageExtendedSysState{}, &ardupilotmega.MessageEkfStatusReport{}} {
		id := m.GetID()
		eventually(t, "the streams at 4 Hz", func() bool { return f.Interval(id) == 250000 })
	}
	eventually(t, "home to be asked for", func() bool { _, asked, _ := f.State(); return asked > 0 })
	if h := a.Health(); h.Kind != vector.HealthOK || h.Detail != "" {
		t.Errorf("health %+v", h)
	}
}

func TestGotoLaunchesALandedCopter(t *testing.T) {
	f, a := copter(t, mavlinktest.Options{}, nil)
	until(t, a, "a frame", func(vector.VectorState) bool { return true })
	wp := vector.Position{Lat: fieldLat, Lon: fieldLon, AltM: padAMSL + sep + 30}
	execute(t, a, cmd("DRONE-01", 1, vector.CommandGoto, &wp))

	s := until(t, a, "the goto acknowledged", func(s vector.VectorState) bool { return s.AckSeq == 1 })
	if s.Mode != vector.ModeTransit {
		t.Errorf("mode %q once the goto is applied, want transit", s.Mode)
	}
	want := []common.MAV_CMD{
		common.MAV_CMD_DO_SET_MODE, common.MAV_CMD_COMPONENT_ARM_DISARM, common.MAV_CMD_NAV_TAKEOFF,
		common.MAV_CMD_DO_REPOSITION, common.MAV_CMD_DO_CHANGE_SPEED,
	}
	if got := f.FlightCommandIDs(); !slices.Equal(got, want) {
		t.Fatalf("commands %v, want %v", got, want)
	}
	if sm := f.Last(common.MAV_CMD_DO_SET_MODE); sm.Param2 != float32(ardupilotmega.COPTER_MODE_GUIDED) {
		t.Errorf("set mode %v, want GUIDED", sm.Param2)
	}
	if tk := f.Last(common.MAV_CMD_NAV_TAKEOFF); !near(float64(tk.Z), 30, 0.01) {
		t.Errorf("takeoff to %v m above home, want 30", tk.Z)
	}
	rp := f.Last(common.MAV_CMD_DO_REPOSITION)
	if rp.X != e7(fieldLat) || rp.Y != e7(fieldLon) || !near(float64(rp.Z), padAMSL+30, 0.01) {
		t.Errorf("reposition to %d, %d, %v m AMSL", rp.X, rp.Y, rp.Z)
	}
	if _, _, speed := f.State(); speed != 18 {
		t.Errorf("speed set to %v, want the cruise speed 18", speed)
	}
	s = until(t, a, "the copter over the field", func(s vector.VectorState) bool {
		return domain.HaversineM(s.Position, wp) < 1 && near(s.Position.AltM, wp.AltM, 0.01)
	})
	if s.Mode != vector.ModeTransit {
		t.Errorf("mode %q keeping station at the waypoint, want transit", s.Mode)
	}
}

func TestRefusedCommandStaysUnacknowledged(t *testing.T) {
	f, a := copter(t, mavlinktest.Options{Airborne: true}, nil)
	until(t, a, "a frame", func(vector.VectorState) bool { return true })
	f.RefuseNext(common.MAV_CMD_DO_REPOSITION, common.MAV_RESULT_DENIED)
	wp := vector.Position{Lat: padLat, Lon: padLon, AltM: padAMSL + sep + 30}
	execute(t, a, cmd("DRONE-01", 1, vector.CommandGoto, &wp))
	eventually(t, "the refusal in Health", healthSays(a, vector.HealthOK, "goto seq 1 not applied: MAV_CMD_DO_REPOSITION refused: MAV_RESULT_DENIED"))
	if s := until(t, a, "a frame", func(vector.VectorState) bool { return true }); s.AckSeq != 0 {
		t.Fatalf("refused goto acknowledged: AckSeq %d", s.AckSeq)
	}

	// The engine re-sends what is not acknowledged, with the same Seq.
	execute(t, a, cmd("DRONE-01", 1, vector.CommandGoto, &wp))
	until(t, a, "the re-sent goto acknowledged", func(s vector.VectorState) bool { return s.AckSeq == 1 })
	eventually(t, "the refusal cleared", func() bool { return !strings.Contains(a.Health().Detail, "refused") })
}

func TestUnansweredCommandStaysUnacknowledged(t *testing.T) {
	f, a := copter(t, mavlinktest.Options{Airborne: true}, func(c *mavlink.Config) { c.AckTimeout = 200 * time.Millisecond })
	until(t, a, "a frame", func(vector.VectorState) bool { return true })
	f.MuteAcks(common.MAV_CMD_DO_REPOSITION)
	wp := vector.Position{Lat: padLat, Lon: padLon, AltM: padAMSL + sep + 30}
	execute(t, a, cmd("DRONE-01", 1, vector.CommandGoto, &wp))
	eventually(t, "the timeout in Health", healthSays(a, vector.HealthOK, "no COMMAND_ACK to MAV_CMD_DO_REPOSITION within 200ms"))
	if s := until(t, a, "a frame", func(vector.VectorState) bool { return true }); s.AckSeq != 0 {
		t.Fatalf("unanswered goto acknowledged: AckSeq %d", s.AckSeq)
	}
}

func TestHoldOnTheGroundSendsNothing(t *testing.T) {
	f, a := copter(t, mavlinktest.Options{}, nil)
	until(t, a, "a frame", func(vector.VectorState) bool { return true })
	execute(t, a, cmd("DRONE-01", 1, vector.CommandHold, nil))
	s := until(t, a, "the hold acknowledged", func(s vector.VectorState) bool { return s.AckSeq == 1 })
	if s.Mode != vector.ModeIdle {
		t.Errorf("mode %q, want idle", s.Mode)
	}
	if got := f.FlightCommandIDs(); len(got) != 0 {
		t.Errorf("a landed copter was sent %v to hold", got)
	}
}

func TestHoldStopsWhereTheVehicleIs(t *testing.T) {
	f, a := copter(t, mavlinktest.Options{Airborne: true}, nil)
	until(t, a, "a frame", func(vector.VectorState) bool { return true })
	far := vector.Position{Lat: fieldLat + 0.2, Lon: fieldLon, AltM: padAMSL + sep + 30}
	execute(t, a, cmd("DRONE-01", 1, vector.CommandGoto, &far))
	until(t, a, "the copter on its way", func(s vector.VectorState) bool { return s.AckSeq == 1 && s.Speed > 0 })

	execute(t, a, cmd("DRONE-01", 2, vector.CommandHold, nil))
	s := until(t, a, "the copter stopped", func(s vector.VectorState) bool { return s.AckSeq == 2 && s.Speed == 0 })
	if s.Mode != vector.ModeIdle {
		t.Errorf("mode %q holding, want idle", s.Mode)
	}
	if domain.HaversineM(s.Position, far) < 1000 {
		t.Errorf("the copter went on to the goto's waypoint")
	}
	if rp := f.Last(common.MAV_CMD_DO_REPOSITION); rp.X == e7(far.Lat) {
		t.Errorf("hold repositioned to the goto's waypoint")
	}
}

func TestRTBToAStationLandsOnArrival(t *testing.T) {
	f, a := copter(t, mavlinktest.Options{Airborne: true}, nil)
	until(t, a, "a frame", func(vector.VectorState) bool { return true })
	station := vector.Position{Lat: padLat, Lon: padLon, AltM: padAMSL + sep}
	execute(t, a, cmd("DRONE-01", 1, vector.CommandRTB, &station))

	until(t, a, "the copter returning", func(s vector.VectorState) bool { return s.AckSeq == 1 && s.Mode == vector.ModeRTB })
	if rp := f.Last(common.MAV_CMD_DO_REPOSITION); !near(float64(rp.Z), padAMSL+30, 0.01) {
		t.Errorf("returned at %v m AMSL, want the current 410 rather than the station's ground", rp.Z)
	}
	// The landing starts within the station radius, where the copter is:
	// short of the station itself (spec section 7.4).
	until(t, a, "the copter landed at the station", func(s vector.VectorState) bool {
		return s.Mode == vector.ModeIdle && domain.HaversineM(s.Position, station) <= mavlink.DefaultStationRadiusM && near(s.Position.AltM, station.AltM, 0.01)
	})
	eventually(t, "the copter disarmed", func() bool { armed, _, _ := f.State(); return !armed })
	want := []common.MAV_CMD{common.MAV_CMD_DO_REPOSITION, common.MAV_CMD_NAV_LAND}
	if got := f.FlightCommandIDs(); !slices.Equal(got, want) {
		t.Errorf("commands %v, want %v", got, want)
	}
}

func TestRTBWithoutAStationHandsOverToTheAutopilot(t *testing.T) {
	f, a := copter(t, mavlinktest.Options{Airborne: true}, nil)
	until(t, a, "a frame", func(vector.VectorState) bool { return true })
	execute(t, a, cmd("DRONE-01", 1, vector.CommandRTB, nil))
	until(t, a, "the autopilot returning", func(s vector.VectorState) bool { return s.AckSeq == 1 && s.Mode == vector.ModeRTB })
	until(t, a, "the copter landed at home", func(s vector.VectorState) bool {
		return s.Mode == vector.ModeIdle && near(s.Position.Lat, padLat, 1e-7) && near(s.Position.Lon, padLon, 1e-7)
	})
	if got, want := f.FlightCommandIDs(), []common.MAV_CMD{common.MAV_CMD_NAV_RETURN_TO_LAUNCH}; !slices.Equal(got, want) {
		t.Errorf("commands %v, want %v", got, want)
	}
}

func TestRoverArmsAndDrives(t *testing.T) {
	f := newFake(t, mavlinktest.Options{SystemID: 2, Type: minimal.MAV_TYPE_GROUND_ROVER})
	a := newAdapter(t, newLink(t, f), mavlink.Config{SystemID: 2, Capabilities: roverCaps()})
	until(t, a, "a frame", func(vector.VectorState) bool { return true })
	wp := vector.Position{Lat: fieldLat, Lon: fieldLon, AltM: padAMSL + sep}
	execute(t, a, cmd("UGV-01", 1, vector.CommandGoto, &wp))
	s := until(t, a, "the goto acknowledged", func(s vector.VectorState) bool { return s.AckSeq == 1 })
	if s.Mode != vector.ModeTransit {
		t.Errorf("mode %q, want transit", s.Mode)
	}
	want := []common.MAV_CMD{common.MAV_CMD_COMPONENT_ARM_DISARM, common.MAV_CMD_DO_REPOSITION}
	if got := f.FlightCommandIDs(); !slices.Equal(got, want) {
		t.Fatalf("commands %v, want %v", got, want)
	}
	if rp := f.Last(common.MAV_CMD_DO_REPOSITION); rp.Param1 != 6 {
		t.Errorf("reposition speed %v, want the cruise speed 6", rp.Param1)
	}
	until(t, a, "the rover at the waypoint", func(s vector.VectorState) bool { return domain.HaversineM(s.Position, wp) < 1 })
}

func TestRebootResetsAckSeq(t *testing.T) {
	f, a := copter(t, mavlinktest.Options{}, nil)
	until(t, a, "a frame", func(vector.VectorState) bool { return true })
	execute(t, a, cmd("DRONE-01", 1, vector.CommandHold, nil))
	until(t, a, "the hold acknowledged", func(s vector.VectorState) bool { return s.AckSeq == 1 })
	f.Reboot()
	until(t, a, "AckSeq back to 0", func(s vector.VectorState) bool { return s.AckSeq == 0 })
	for _, m := range []interface{ GetID() uint32 }{&common.MessageGlobalPositionInt{}, &common.MessageSysStatus{}, &ardupilotmega.MessageEkfStatusReport{}} {
		id := m.GetID()
		eventually(t, "the streams asked for again", func() bool { return f.Interval(id) == 250000 })
	}
	// The command standing when the autopilot restarted reaches it again.
	execute(t, a, cmd("DRONE-01", 1, vector.CommandHold, nil))
	until(t, a, "the re-sent hold acknowledged", func(s vector.VectorState) bool { return s.AckSeq == 1 })
}

func TestFramesAreWithheld(t *testing.T) {
	cases := []struct {
		name   string
		opts   mavlinktest.Options
		detail string
	}{
		{"battery unknown", mavlinktest.Options{NoBattery: true}, "battery of system 1 unknown"},
		{"another domain", mavlinktest.Options{Type: minimal.MAV_TYPE_GROUND_ROVER}, "system 1 runs ArduRover (ground), capabilities declare aerial"},
		{"not ArduPilot", mavlinktest.Options{Autopilot: minimal.MAV_AUTOPILOT_PX4}, "system 1 is not an ArduPilot autopilot"},
		{"no position estimate", mavlinktest.Options{NoPosition: true}, "system 1 has no absolute position estimate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, a := copter(t, c.opts, nil)
			eventually(t, "the reason in Health", healthSays(a, vector.HealthLost, c.detail))
			wp := vector.Position{Lat: fieldLat, Lon: fieldLon, AltM: padAMSL + sep + 30}
			execute(t, a, cmd("DRONE-01", 1, vector.CommandGoto, &wp))
			select {
			case s := <-a.Telemetry():
				t.Fatalf("frame published: %+v", s)
			case <-time.After(300 * time.Millisecond):
			}
			if got := f.FlightCommandIDs(); len(got) != 0 {
				t.Errorf("a vehicle KEEL cannot see was sent %v", got)
			}
		})
	}
}

// ArduPilot sends GLOBAL_POSITION_INT whether or not it knows where it is, at
// (0, 0) before its first estimate: a frame from one is a vector teleported.
func TestFramesWaitForAPositionEstimate(t *testing.T) {
	f, a := copter(t, mavlinktest.Options{NoPosition: true}, func(c *mavlink.Config) {
		c.Agent = vector.Options{DegradedAfter: 150 * time.Millisecond, LostAfter: 400 * time.Millisecond}
	})
	// Every frame read, the buffered ones included, must be on the pad.
	onThePad := func(s vector.VectorState) bool {
		if !near(s.Position.Lat, padLat, 1e-7) || !near(s.Position.Lon, padLon, 1e-7) {
			t.Fatalf("frame at %v, %v published, want the pad", s.Position.Lat, s.Position.Lon)
		}
		return true
	}
	const reason = "system 1 has no absolute position estimate"
	eventually(t, "no estimate reported", healthSays(a, vector.HealthLost, reason))
	f.SetNoPosition(false)
	until(t, a, "a frame once the autopilot knows where it is", onThePad)
	eventually(t, "health ok", func() bool { return a.Health().Kind == vector.HealthOK })

	f.SetNoPosition(true)
	eventually(t, "the loss reported", healthSays(a, vector.HealthLost, reason))
	f.SetNoPosition(false)
	until(t, a, "frames resuming", onThePad)
	eventually(t, "health back to ok", func() bool { return a.Health().Kind == vector.HealthOK })
}

func TestSilentHeartbeatReportsLostThenResumes(t *testing.T) {
	f, a := copter(t, mavlinktest.Options{}, func(c *mavlink.Config) {
		c.Agent = vector.Options{DegradedAfter: 150 * time.Millisecond, LostAfter: 400 * time.Millisecond}
	})
	until(t, a, "a frame", func(vector.VectorState) bool { return true })
	f.SetSilent(true)
	eventually(t, "the silence reported", healthSays(a, vector.HealthLost, "no HEARTBEAT from system 1 for 400ms"))
	f.SetSilent(false)
	until(t, a, "frames resuming on the same channel", func(vector.VectorState) bool { return true })
	eventually(t, "health back to ok", func() bool { return a.Health().Kind == vector.HealthOK })
}

func TestLinkRoutesBySystemID(t *testing.T) {
	fc := newFake(t, mavlinktest.Options{SystemID: 1})
	fr := newFake(t, mavlinktest.Options{SystemID: 2, Type: minimal.MAV_TYPE_GROUND_ROVER})
	l := newLink(t, fc, fr)
	ac := newAdapter(t, l, mavlink.Config{SystemID: 1, Capabilities: copterCaps()})
	ar := newAdapter(t, l, mavlink.Config{SystemID: 2, Capabilities: roverCaps()})
	until(t, ac, "the copter's frame", func(s vector.VectorState) bool { return s.ID == "DRONE-01" })
	until(t, ar, "the rover's frame", func(s vector.VectorState) bool { return s.ID == "UGV-01" })

	wp := vector.Position{Lat: fieldLat, Lon: fieldLon, AltM: padAMSL + sep}
	execute(t, ar, cmd("UGV-01", 1, vector.CommandGoto, &wp))
	until(t, ar, "the rover's goto acknowledged", func(s vector.VectorState) bool { return s.AckSeq == 1 })
	if got := fc.FlightCommandIDs(); len(got) != 0 {
		t.Errorf("the copter received the rover's commands: %v", got)
	}
	if s := until(t, ac, "a copter frame", func(vector.VectorState) bool { return true }); s.AckSeq != 0 || s.Mode != vector.ModeIdle {
		t.Errorf("copter frame %+v after the rover's goto", s)
	}
}

// Conformance case C9, as far as the adapter can see it: Close ends the
// channel, refuses commands and frees the system id.
func TestCloseEndsTheAdapter(t *testing.T) {
	f := newFake(t, mavlinktest.Options{SystemID: 1})
	l := newLink(t, f)
	a, err := mavlink.NewAdapter(l, mavlink.Config{SystemID: 1, Capabilities: copterCaps(), GeoidSeparationM: sep})
	if err != nil {
		t.Fatal(err)
	}
	until(t, a, "a frame", func(vector.VectorState) bool { return true })
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	deadline := time.After(wait)
	for open := true; open; {
		select {
		case _, open = <-a.Telemetry():
		case <-deadline:
			t.Fatal("telemetry channel still open")
		}
	}
	if err := a.Execute(cmd("DRONE-01", 1, vector.CommandHold, nil)); !errors.Is(err, vector.ErrClosed) {
		t.Fatalf("Execute after Close: %v, want ErrClosed", err)
	}
	if h := a.Health(); h.Kind != vector.HealthLost || !strings.HasPrefix(h.Detail, "closed") {
		t.Fatalf("health after Close: %+v", h)
	}
	b := newAdapter(t, l, mavlink.Config{SystemID: 1, Capabilities: copterCaps()})
	until(t, b, "a frame for the system id freed", func(vector.VectorState) bool { return true })
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second link Close: %v", err)
	}
}

// A SITL is brought to ×1 on first contact, set faster on demand, and set
// again after it restarts.
func TestSetSimSpeedHoldsSITLSpeedup(t *testing.T) {
	f, a := copter(t, mavlinktest.Options{SITL: true}, func(c *mavlink.Config) { c.SITL = true })
	until(t, a, "a frame", func(vector.VectorState) bool { return true })
	eventually(t, "SIM_SPEEDUP set on first contact", func() bool { _, n := f.SimSpeed(); return n > 0 })
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if err := a.SetSimSpeed(ctx, 20); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.SimSpeed(); got != 20 {
		t.Fatalf("SIM_SPEEDUP %v after SetSimSpeed(20)", got)
	}
	f.Reboot()
	eventually(t, "SIM_SPEEDUP set again after the reboot", func() bool { got, _ := f.SimSpeed(); return got == 20 })
	if err := a.SetSimSpeed(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.SimSpeed(); got != 1 {
		t.Fatalf("SIM_SPEEDUP %v after SetSimSpeed(1)", got)
	}
}

// An autopilot without SIM_SPEEDUP never confirms it, and a vehicle not
// configured as a SITL is never asked.
func TestSetSimSpeedRefusals(t *testing.T) {
	f, a := copter(t, mavlinktest.Options{}, func(c *mavlink.Config) { c.SITL = true })
	until(t, a, "a frame", func(vector.VectorState) bool { return true })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := a.SetSimSpeed(ctx, 5); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SetSimSpeed on an autopilot without SIM_SPEEDUP: %v", err)
	}
	if _, n := f.SimSpeed(); n == 0 {
		t.Fatal("SIM_SPEEDUP never sent")
	}

	f2, real := copter(t, mavlinktest.Options{SITL: true}, nil)
	until(t, real, "a frame", func(vector.VectorState) bool { return true })
	if err := real.SetSimSpeed(context.Background(), 5); err == nil || !strings.Contains(err.Error(), "not a SITL") {
		t.Fatalf("SetSimSpeed on a vehicle not configured as a SITL: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, n := f2.SimSpeed(); n != 0 {
		t.Fatalf("a vehicle not configured as a SITL was sent %d PARAM_SET", n)
	}
}
