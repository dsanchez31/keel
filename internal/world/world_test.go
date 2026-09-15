package world

import (
	"errors"
	"math"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/eventlog"
)

func drone(id string, p domain.Position) Vehicle {
	return Vehicle{
		Caps: domain.Capabilities{ID: domain.VectorID(id), Domain: domain.DomainAerial, Tags: []string{"camera", "aerial"},
			CruiseSpeed: 20, MaxRangeM: 60000, SensorRadiusM: 90},
		State: domain.VectorState{ID: domain.VectorID(id), Position: p, BatteryPct: 100, Link: domain.LinkOK, Mode: domain.ModeIdle},
	}
}

func rover(id string, p domain.Position) Vehicle {
	v := drone(id, p)
	v.Caps.Domain, v.Caps.Tags, v.Caps.CruiseSpeed = domain.DomainGround, []string{"camera", "ground"}, 5
	return v
}

// perfect is a world with no loss, no jitter, no noise and no hover drain,
// so a test observes one mechanism at a time.
func perfect(t *testing.T, fleet ...Vehicle) *World {
	t.Helper()
	w, err := New(Config{Seed: 1, Physics: Physics{ClimbRateMps: 5}, Fleet: fleet,
		Stations: []domain.Station{{Name: "gcs", Position: at(0, 0, 0)}}})
	if err != nil {
		t.Fatalf("world: %v", err)
	}
	return w
}

// frameOf returns the frame of a vehicle in a tick's events.
func frameOf(evs []domain.Event, id domain.VectorID) (domain.VectorState, bool) {
	for _, ev := range evs {
		if ev.Vector == id && ev.Telemetry != nil {
			return *ev.Telemetry, true
		}
	}
	return domain.VectorState{}, false
}

func gotoCmd(id string, seq uint64, p domain.Position) domain.Command {
	return domain.Command{Vector: domain.VectorID(id), Seq: seq, Type: domain.CommandGoto, Waypoint: &p}
}

func TestJoinCarriesTheInitialState(t *testing.T) {
	w := perfect(t, drone("D-2", at(10, 0, 0)), drone("D-1", at(0, 0, 0)))
	evs := w.Join()
	if len(evs) != 2 || evs[0].Vector != "D-1" || evs[1].Vector != "D-2" {
		t.Fatalf("join events %+v, want both in id order", evs)
	}
	for _, ev := range evs {
		if ev.Kind != domain.EventVectorJoined || ev.Caps == nil || ev.Telemetry == nil || !ev.Telemetry.Position.Finite() {
			t.Fatalf("join event %+v lacks capabilities or state", ev)
		}
	}
}

func TestGotoIsFlownAndReported(t *testing.T) {
	w := perfect(t, drone("D-1", at(0, 0, 0)))
	target := at(0, 400, 120)
	w.Send([]domain.Command{gotoCmd("D-1", 1, target)})

	var last domain.VectorState
	for range 300 {
		evs := w.Step()
		f, ok := frameOf(evs, "D-1")
		if !ok {
			t.Fatalf("no frame at %d ms on a perfect link", w.NowMs())
		}
		if f.LastSeenMs != w.NowMs() || f.Link != domain.LinkOK {
			t.Fatalf("frame %+v at %d ms", f, w.NowMs())
		}
		last = f
	}
	if last.Position != target || last.Mode != domain.ModeTransit {
		t.Fatalf("last frame %+v, want the target, keeping station in transit", last)
	}
	w.Send([]domain.Command{{Vector: "D-1", Seq: 2, Type: domain.CommandHold}})
	f, _ := frameOf(w.Step(), "D-1")
	if f.Mode != domain.ModeIdle || f.Speed != 0 {
		t.Fatalf("after hold: %+v", f)
	}
}

// Idempotency and gaps (spec section 7.2): a Seq already applied changes
// nothing, a skipped Seq is tolerated, and a command overtaken in transit by
// a later one is ignored when it lands.
func TestCommandsAreIdempotentBySeq(t *testing.T) {
	w := perfect(t, drone("D-1", at(0, 0, 0)))
	a, b := at(0, 500, 100), at(500, 0, 100)
	w.Send([]domain.Command{gotoCmd("D-1", 1, a)})
	w.Step()
	w.Send([]domain.Command{gotoCmd("D-1", 1, b)})
	w.Step()
	if tr, _ := w.Truth("D-1"); *tr.Target != a || tr.LastSeq != 1 {
		t.Fatalf("a re-delivered Seq 1 changed the target to %v", *tr.Target)
	}
	w.Send([]domain.Command{gotoCmd("D-1", 3, b)})
	w.Step()
	if tr, _ := w.Truth("D-1"); *tr.Target != b || tr.LastSeq != 3 {
		t.Fatalf("Seq 3 after a gap not applied: %+v", tr)
	}
	w.Send([]domain.Command{gotoCmd("D-1", 2, a)})
	w.Step()
	if tr, _ := w.Truth("D-1"); *tr.Target != b {
		t.Fatal("an overtaken Seq 2 was applied after Seq 3")
	}
}

func TestReturnToBaseWithoutAWaypointGoesHome(t *testing.T) {
	home := at(0, 0, 0)
	w := perfect(t, drone("D-1", home))
	w.Send([]domain.Command{gotoCmd("D-1", 1, at(300, 0, 60))})
	for range 200 {
		w.Step()
	}
	w.Send([]domain.Command{{Vector: "D-1", Seq: 2, Type: domain.CommandRTB}})
	f, _ := frameOf(w.Step(), "D-1")
	if f.Mode != domain.ModeRTB {
		t.Fatalf("mode %s on the way home, want rtb", f.Mode)
	}
	for range 400 {
		f, _ = frameOf(w.Step(), "D-1")
	}
	if tr, _ := w.Truth("D-1"); tr.Position != home || f.Mode != domain.ModeIdle {
		t.Fatalf("at %v in mode %s, want home and idle", tr.Position, f.Mode)
	}
}

func TestLinkLossSilencesBothWays(t *testing.T) {
	w := perfect(t, drone("D-1", at(0, 0, 0)))
	w.Step()
	if err := w.Inject(domain.Fault{Kind: domain.FaultLinkLoss, Vector: "D-1", DurationMs: 1000}); err != nil {
		t.Fatalf("inject: %v", err)
	}
	w.Send([]domain.Command{gotoCmd("D-1", 1, at(0, 300, 50))})
	for range 9 {
		if _, ok := frameOf(w.Step(), "D-1"); ok {
			t.Fatalf("a frame got through a cut link at %d ms", w.NowMs())
		}
	}
	if tr, _ := w.Truth("D-1"); tr.Target != nil || tr.LastSeq != 0 {
		t.Fatalf("a command crossed a cut link: %+v", tr)
	}
	w.Step()
	if _, ok := frameOf(w.Step(), "D-1"); !ok {
		t.Fatal("the link did not come back after its duration")
	}
}

func TestBlackoutAndRange(t *testing.T) {
	w, err := New(Config{Seed: 1, Fleet: []Vehicle{drone("D-1", at(0, 0, 0))},
		Stations: []domain.Station{{Name: "gcs", Position: at(0, 0, 0)}},
		Physics: Physics{ClimbRateMps: 5, Comms: Comms{RangeM: 2000},
			Blackouts: []Blackout{{Name: "z", Center: at(0, 1000, 0), RadiusM: 100}}}})
	if err != nil {
		t.Fatalf("world: %v", err)
	}
	w.Send([]domain.Command{gotoCmd("D-1", 1, at(0, 3000, 50))})
	heard := map[string]bool{}
	for range 1600 {
		_, ok := frameOf(w.Step(), "D-1")
		tr, _ := w.Truth("D-1")
		_, north := enu(at(0, 0, 0), tr.Position)
		zone := "open"
		switch {
		case north > 2000:
			zone = "out of range"
		case north > 900 && north < 1100:
			zone = "blackout"
		}
		if ok {
			heard[zone] = true
		}
		if ok && zone != "open" {
			t.Fatalf("heard at %.0f m north, %s", north, zone)
		}
	}
	if !heard["open"] {
		t.Fatal("never heard in the open")
	}
}

func TestEmptyBatteryAndKillWriteOff(t *testing.T) {
	low := drone("D-1", at(0, 0, 0))
	low.State.BatteryPct = 1
	low.Caps.MaxRangeM = 1000 // 1 % is 10 m
	w := perfect(t, low, drone("D-2", at(10, 0, 0)))
	w.Send([]domain.Command{gotoCmd("D-1", 1, at(0, 500, 0))})
	for range 20 {
		w.Step()
	}
	if tr, _ := w.Truth("D-1"); !tr.Down || tr.Mode != domain.ModeDown {
		t.Fatalf("D-1 flew on an empty battery: %+v", tr)
	}
	if _, ok := frameOf(w.Step(), "D-1"); ok {
		t.Fatal("a vehicle out of battery still transmits")
	}
	if err := w.Inject(domain.Fault{Kind: domain.FaultKill, Vector: "D-2"}); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if _, ok := frameOf(w.Step(), "D-2"); ok {
		t.Fatal("a killed vehicle still transmits")
	}
	if err := w.Inject(domain.Fault{Kind: domain.FaultKill, Vector: "NOPE"}); !errors.Is(err, ErrUnknownVector) {
		t.Fatalf("a fault on an unknown vector: err %v, want ErrUnknownVector", err)
	}
}

func TestInjectHoldsTheFaultRule(t *testing.T) {
	w := perfect(t, drone("D-1", at(0, 0, 0)))
	for _, f := range []domain.Fault{
		{Kind: "meteor", Vector: "D-1"},
		{Kind: domain.FaultKill},
		{Kind: domain.FaultBatteryDrain, Vector: "D-1"},
		{Kind: domain.FaultBatteryDrain, Vector: "D-1", Magnitude: math.NaN()},
		{Kind: domain.FaultLinkLoss, Vector: "D-1", DurationMs: -100},
		{Kind: domain.FaultLinkLoss, Vector: "D-1", DurationMs: 150},
	} {
		if err := w.Inject(f); !errors.Is(err, ErrInvalidFault) {
			t.Errorf("%+v: err %v, want ErrInvalidFault", f, err)
		}
	}
	if tr, _ := w.Truth("D-1"); tr.Down || tr.BatteryPct != 100 {
		t.Fatalf("a refused fault changed the vehicle: %+v", tr)
	}
}

func TestGroundVehicleDrivesAroundBlockedTerrain(t *testing.T) {
	w, err := New(Config{Seed: 1, Fleet: []Vehicle{rover("U-1", at(0, 0, 0))},
		Physics: Physics{ClimbRateMps: 5, Terrain: TerrainSpec{CellM: 25, Blocked: []BlockedArea{
			rect("wall", -1000, 400, 550, 450), rect("wall-east", 650, 400, 1000, 450),
		}}},
		Extent: []domain.Position{at(-1000, -500, 0), at(1000, 1500, 0)}})
	if err != nil {
		t.Fatalf("world: %v", err)
	}
	dest := at(0, 900, 30)
	w.Send([]domain.Command{gotoCmd("U-1", 1, dest)})
	for range 6000 {
		w.Step()
		tr, _ := w.Truth("U-1")
		if !w.terrain.Open(tr.Position) {
			t.Fatalf("drove onto blocked ground at %v", tr.Position)
		}
		if tr.Position.AltM != 0 {
			t.Fatalf("ground vehicle at altitude %v", tr.Position.AltM)
		}
		if tr.Target == nil {
			if e, n := enu(dest, tr.Position); e*e+n*n > 1e-6 {
				t.Fatalf("stopped %v, %v m from the destination", e, n)
			}
			return
		}
	}
	t.Fatal("never arrived")
}

// The world is a function of its seed and its inputs: the same seed and the
// same commands give byte-identical telemetry, a different seed different
// noise.
func TestDeterminism(t *testing.T) {
	run := func(seed uint64) eventlog.Digest {
		fleet := []Vehicle{drone("D-1", at(0, 0, 0)), drone("D-2", at(50, 0, 0)), rover("U-1", at(-50, 0, 0))}
		w, err := New(Config{Seed: seed, Fleet: fleet,
			Stations: []domain.Station{{Name: "gcs", Position: at(0, 0, 0)}},
			Physics: Physics{ClimbRateMps: 5, GPSNoiseM: 1.5, HoverDrainPctPerMin: 0.5,
				Comms: Comms{RangeM: 5000, LossPct: 5, JitterMs: 300}}})
		if err != nil {
			t.Fatalf("world: %v", err)
		}
		log := eventlog.NewMemLog()
		var seq uint64
		for i := range 600 {
			if i%50 == 0 {
				seq++
				p := at(float64(i), float64(2*i), 80)
				w.Send([]domain.Command{gotoCmd("D-1", seq, p), gotoCmd("D-2", seq, p), gotoCmd("U-1", seq, p)})
			}
			if i == 200 {
				if err := w.Inject(domain.Fault{Kind: domain.FaultGPSDrift, Vector: "D-2", Magnitude: 3, DurationMs: 5000}); err != nil {
					t.Fatalf("inject: %v", err)
				}
			}
			for _, ev := range w.Step() {
				if _, err := log.Append(eventlog.RecordEvent, w.NowMs(), ev); err != nil {
					t.Fatalf("append: %v", err)
				}
			}
		}
		return log.Head()
	}
	want := run(42)
	for i := range 20 {
		if got := run(42); got != want {
			t.Fatalf("repetition %d: head %s, want %s", i, got, want)
		}
	}
	if run(43) == want {
		t.Fatal("two seeds gave the same telemetry, the seed is not reaching the noise")
	}
}
