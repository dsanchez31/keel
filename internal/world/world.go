package world

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"

	"github.com/dsanchez31/keel/internal/domain"
)

// Vehicle is one simulated vector as a run starts it: what it declares and
// its initial state.
type Vehicle struct {
	Caps  domain.Capabilities
	State domain.VectorState
}

// Config is everything a world is built from.
type Config struct {
	Seed     uint64
	Physics  Physics
	Stations []domain.Station
	Fleet    []Vehicle
	// Extent are positions the terrain grid must cover beyond the fleet and
	// the stations, typically the AO polygons.
	Extent []domain.Position
}

// ErrInvalidConfig reports a world that cannot be built.
var ErrInvalidConfig = errors.New("world: invalid config")

// tickS is one tick in seconds.
const tickS = float64(domain.TickIntervalMs) / 1000

// World is the simulated world. It is driven one tick at a time by its
// caller, never by a clock: Send hands it the commands the engine issued,
// Step advances it 100 ms and returns the telemetry that reached the ground.
// It is not safe for concurrent use; the loop that drives it owns it.
type World struct {
	tick     int64
	rng      *rand.Rand
	physics  Physics
	comms    comms
	sensors  sensors
	terrain  *Terrain
	vehicles []*vehicle // sorted by id

	up   channel[domain.Command]
	down channel[domain.VectorState]
}

// vehicle is one vector's truth, which only the world knows.
type vehicle struct {
	caps    domain.Capabilities
	home    domain.Position
	pos     domain.Position
	heading float64
	speed   float64
	bat     battery
	// mode is physical: idle, transit toward a commanded point, rtb, down.
	mode    domain.VectorMode
	target  *domain.Position
	route   []domain.Position // ground vehicles only
	lastSeq uint64
	faults  faultState
	down    bool
}

func (v *vehicle) aerial() bool { return v.caps.Domain == domain.DomainAerial }

// New builds a world at mission time zero.
func New(cfg Config) (*World, error) {
	if len(cfg.Fleet) == 0 {
		return nil, fmt.Errorf("%w: no vehicle", ErrInvalidConfig)
	}
	if !(cfg.Physics.ClimbRateMps > 0) {
		return nil, fmt.Errorf("%w: climb rate %v m/s", ErrInvalidConfig, cfg.Physics.ClimbRateMps)
	}
	w := &World{
		// Two words from one seed, as engine.NewRand does, so a seed of
		// zero is still a usable stream.
		rng:     rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9E3779B97F4A7C15)),
		physics: cfg.Physics,
		comms:   comms{cfg: cfg.Physics.Comms, stations: slices.Clone(cfg.Stations), blackouts: slices.Clone(cfg.Physics.Blackouts)},
		sensors: sensors{gpsNoiseM: cfg.Physics.GPSNoiseM},
	}
	extent := slices.Clone(cfg.Extent)
	for _, s := range cfg.Stations {
		extent = append(extent, s.Position)
	}
	for _, f := range cfg.Fleet {
		c := f.Caps.Clone()
		c.SortTags()
		switch {
		case c.ID == "":
			return nil, fmt.Errorf("%w: a vehicle has no id", ErrInvalidConfig)
		case !domain.ValidDomain(c.Domain):
			return nil, fmt.Errorf("%w: %s has domain %q", ErrInvalidConfig, c.ID, c.Domain)
		case !(c.CruiseSpeed > 0):
			return nil, fmt.Errorf("%w: %s has no cruise speed", ErrInvalidConfig, c.ID)
		case !f.State.Position.Finite():
			return nil, fmt.Errorf("%w: %s has no finite position", ErrInvalidConfig, c.ID)
		}
		v := &vehicle{
			caps:    c,
			home:    f.State.Position,
			pos:     f.State.Position,
			heading: domain.NormaliseDeg(f.State.Heading),
			bat:     battery{pct: float64(f.State.BatteryPct)},
			mode:    domain.ModeIdle,
		}
		if f.State.Mode == domain.ModeDown {
			v.down, v.mode = true, domain.ModeDown
		}
		w.vehicles = append(w.vehicles, v)
		extent = append(extent, v.pos)
	}
	slices.SortFunc(w.vehicles, func(a, b *vehicle) int { return strings.Compare(string(a.caps.ID), string(b.caps.ID)) })
	for i := 1; i < len(w.vehicles); i++ {
		if w.vehicles[i].caps.ID == w.vehicles[i-1].caps.ID {
			return nil, fmt.Errorf("%w: %s appears twice", ErrInvalidConfig, w.vehicles[i].caps.ID)
		}
	}
	t, err := NewTerrain(cfg.Physics.Terrain, extent)
	if err != nil {
		return nil, err
	}
	w.terrain = t
	return w, nil
}

// NowMs is the world's mission time.
func (w *World) NowMs() int64 { return w.tick * domain.TickIntervalMs }

func (w *World) vehicle(id domain.VectorID) *vehicle {
	i, ok := slices.BinarySearchFunc(w.vehicles, id, func(v *vehicle, id domain.VectorID) int {
		return strings.Compare(string(v.caps.ID), string(id))
	})
	if !ok {
		return nil
	}
	return w.vehicles[i]
}

// Join returns the vector_joined event of every vehicle not written off, in
// id order, each carrying its capabilities and its initial state (spec
// section 4.5).
func (w *World) Join() []domain.Event {
	var out []domain.Event
	for _, v := range w.vehicles {
		if v.down {
			continue
		}
		caps := v.caps.Clone()
		st := w.state(v, v.pos)
		out = append(out, domain.Event{Kind: domain.EventVectorJoined, Vector: caps.ID, Caps: &caps, Telemetry: &st})
	}
	return out
}

// Send puts the engine's commands on the air at the current mission time.
// Each is lost when the vehicle has no link or the draw says so, and
// otherwise arrives after its jitter, at the start of a later Step.
func (w *World) Send(cmds []domain.Command) {
	for _, c := range cmds {
		v := w.vehicle(c.Vector)
		if v == nil {
			continue
		}
		lost, delay := w.comms.transit(w.rng)
		if lost || !w.linkUp(v) {
			continue
		}
		cc := c
		if c.Waypoint != nil {
			wp := *c.Waypoint
			cc.Waypoint = &wp
		}
		w.up.push(w.tick+delay, cc)
	}
}

// Inject applies a fault now. A fault Fault.Check refuses is ErrInvalidFault,
// one on a vector the world does not simulate ErrUnknownVector. The caller
// records the matching fault_injected event for the engine, so replay
// reproduces the fault as well as its consequences.
func (w *World) Inject(f domain.Fault) error {
	if err := f.Check(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidFault, err)
	}
	v := w.vehicle(f.Vector)
	if v == nil {
		return fmt.Errorf("%w: %q", ErrUnknownVector, f.Vector)
	}
	if err := v.faults.inject(f, w.NowMs(), &v.bat, w.rng); err != nil {
		return err
	}
	if v.faults.killed || v.bat.empty() {
		w.writeOff(v)
	}
	return nil
}

// Step advances the world by one tick and returns the telemetry events that
// reached the ground during it, in arrival order.
//
// Within a tick: the commands due are delivered, every vehicle moves and pays
// for it, then every vehicle transmits. Vehicles are visited in id order and
// every random draw happens in that order, so a seed fixes the run.
func (w *World) Step() []domain.Event {
	w.tick++
	now := w.NowMs()
	for _, c := range w.up.pop(w.tick) {
		if v := w.vehicle(c.Vector); v != nil {
			w.apply(v, c)
		}
	}
	for _, v := range w.vehicles {
		w.advance(v)
	}
	for _, v := range w.vehicles {
		w.transmit(v, now)
	}
	frames := w.down.pop(w.tick)
	out := make([]domain.Event, len(frames))
	for i := range frames {
		st := frames[i]
		out[i] = domain.Event{Kind: domain.EventTelemetry, Vector: st.ID, Telemetry: &st}
	}
	return out
}

// apply executes one delivered command. A Seq the vehicle has already passed
// is ignored, whether it is a re-send of the command it is executing or a
// command overtaken in transit by a later one (spec section 7.2).
func (w *World) apply(v *vehicle, c domain.Command) {
	if v.down || c.Seq <= v.lastSeq {
		return
	}
	v.lastSeq = c.Seq
	switch c.Type {
	case domain.CommandGoto:
		if c.Waypoint == nil {
			return
		}
		w.headFor(v, *c.Waypoint, domain.ModeTransit)
	case domain.CommandRTB:
		dest := v.home
		if c.Waypoint != nil {
			dest = *c.Waypoint
		}
		w.headFor(v, dest, domain.ModeRTB)
	case domain.CommandHold, domain.CommandAbort:
		v.target, v.route, v.mode = nil, nil, domain.ModeIdle
	}
}

// headFor sets a destination. A ground vehicle routes to it over the terrain
// and ignores its altitude; a destination it cannot reach leaves it where it
// is, idle, which the engine observes as a vehicle that never arrives.
func (w *World) headFor(v *vehicle, dest domain.Position, mode domain.VectorMode) {
	if !v.aerial() {
		dest.AltM = 0
		route, err := w.terrain.Route(v.pos, dest)
		if err != nil {
			v.target, v.route, v.mode = nil, nil, domain.ModeIdle
			return
		}
		v.route = route
	}
	v.target, v.mode = &dest, mode
}

// advance moves one vehicle through one tick and charges its battery.
func (w *World) advance(v *vehicle) {
	if v.down {
		return
	}
	m := move{pos: v.pos, heading: v.heading}
	if v.target != nil {
		if v.aerial() {
			m = stepAerial(v.pos, *v.target, v.heading, v.caps.CruiseSpeed, w.physics.ClimbRateMps, tickS)
		} else {
			m, v.route = stepGround(v.pos, v.route, v.caps.CruiseSpeed, tickS)
		}
		if m.arrived {
			// At a goto point the vehicle keeps station there, still under
			// its command; home from an rtb, it is done.
			v.target, v.route = nil, nil
			if v.mode == domain.ModeRTB {
				v.mode = domain.ModeIdle
			}
		}
	}
	v.pos, v.heading, v.speed = m.pos, m.heading, m.speed
	if v.target == nil {
		v.speed = 0
	}
	v.bat.spend(m.movedM, v.caps.MaxRangeM, v.aerial() && v.pos.AltM > airborneAltM, w.physics.HoverDrainPctPerMin, tickS)
	if v.bat.empty() {
		w.writeOff(v)
	}
}

// writeOff takes a vehicle out of the run: killed, or out of battery. It
// neither moves nor transmits again, and the engine learns of it the way it
// would of a real loss, from silence, unless a fault_injected event says so.
// Frames it sent before are already in the air and still arrive.
func (w *World) writeOff(v *vehicle) {
	v.down, v.mode, v.target, v.route, v.speed = true, domain.ModeDown, nil, nil, 0
}

// transmit sends one frame from a vehicle. The draws for noise and transit
// happen whether or not the frame gets through, so a link that comes and goes
// does not shift the stream the other vehicles draw from.
func (w *World) transmit(v *vehicle, now int64) {
	if v.down {
		return
	}
	de, dn := v.faults.drift(now)
	frame := w.state(v, w.sensors.report(v.pos, de, dn, w.rng))
	if stale, ok := v.faults.stale(now); ok {
		frame = stale
	}
	v.faults.sent(frame, now)
	lost, delay := w.comms.transit(w.rng)
	if lost || !w.linkUp(v) {
		return
	}
	w.down.push(w.tick+delay, frame)
}

// state is the frame a vehicle sends now, with the position it reports.
func (w *World) state(v *vehicle, reported domain.Position) domain.VectorState {
	return domain.VectorState{
		ID:         v.caps.ID,
		Position:   reported,
		Heading:    v.heading,
		Speed:      v.speed,
		BatteryPct: v.bat.reported(),
		Link:       domain.LinkOK,
		Mode:       v.mode,
		LastSeenMs: w.NowMs(),
		AckSeq:     v.lastSeq,
	}
}

// linkUp reports whether a vehicle's radio carries anything now.
func (w *World) linkUp(v *vehicle) bool {
	return !v.down && !v.faults.linkCut(w.NowMs()) && w.comms.reach(v.pos)
}

// Truth is what the world knows about a vehicle and the engine may not: its
// true position and state. It exists for tests and for the DST harness,
// which asserts invariants against the world as well as against the engine.
type Truth struct {
	Position   domain.Position
	BatteryPct float64
	Mode       domain.VectorMode
	Down       bool
	LinkUp     bool
	Target     *domain.Position
	LastSeq    uint64
}

// Truth returns a vehicle's true state.
func (w *World) Truth(id domain.VectorID) (Truth, bool) {
	v := w.vehicle(id)
	if v == nil {
		return Truth{}, false
	}
	t := Truth{Position: v.pos, BatteryPct: v.bat.pct, Mode: v.mode, Down: v.down, LinkUp: w.linkUp(v), LastSeq: v.lastSeq}
	if v.target != nil {
		tg := *v.target
		t.Target = &tg
	}
	return t, true
}
