// Package world is the simulator: vehicles that fly, drive, drain their
// batteries, lose their links and report what they believe their position is.
//
// It is outside the decision path and deliberately separate from it. The
// engine never imports it; the two meet only through the events the world
// produces and the commands the engine sends. It is nonetheless deterministic:
// every random draw comes from one PCG source seeded by the scenario, the
// vehicles are visited in id order, and time is a count of 100 ms ticks, never
// a wall clock. A seed and a scenario are therefore a complete bug report,
// which is what the DST harness of phase 8 builds on.
package world

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dsanchez31/keel/internal/domain"
)

// ScenarioAPIVersion is the only scenario schema version this package reads.
const ScenarioAPIVersion = "keel.sim/v1"

// ErrInvalidScenario reports a scenario that does not parse or validate.
var ErrInvalidScenario = errors.New("world: invalid scenario")

// Scenario is one simulated mission: where it runs, what the operator
// approved, the physics of the simulated world and the faults to inject.
//
// The world file (named areas, stations, fleet) and the Plan IR are
// referenced by path rather than embedded, so the planner-facing registry and
// the simulator's physics stay two documents: the planner never sees a
// blackout zone, and a scenario never redefines the AO.
type Scenario struct {
	Name string
	// Seed seeds both the engine and the world.
	Seed uint64
	// Mission is the id the approved plan runs under.
	Mission domain.MissionID
	// WorldPath and PlanPath are as written, relative to the scenario file.
	WorldPath string
	PlanPath  string
	Physics   Physics
	// Faults are in the order written.
	Faults []ScheduledFault
}

// Physics is everything the world simulates beyond the fleet itself.
type Physics struct {
	// ClimbRateMps caps an aerial vehicle's vertical speed.
	ClimbRateMps float64
	Comms        Comms
	Blackouts    []Blackout
	Terrain      TerrainSpec
	// GPSNoiseM bounds the error on each horizontal axis of a reported
	// position, drawn uniformly in [-GPSNoiseM, GPSNoiseM].
	GPSNoiseM float64
	// HoverDrainPctPerMin is the battery an airborne vehicle spends per
	// minute on top of what distance costs it, holding included.
	HoverDrainPctPerMin float64
}

// Comms is the radio link between the vehicles and the ground.
type Comms struct {
	// RangeM is the distance from the nearest station beyond which there is
	// no link. Zero means unlimited.
	RangeM float64
	// LossPct is the probability, in percent, that one frame or one command
	// is lost in transit.
	LossPct float64
	// JitterMs is the largest extra delay a frame or a command can suffer,
	// drawn uniformly in whole ticks from [0, JitterMs]. A frame delayed past
	// a later one arrives out of order, which the engine drops as stale.
	JitterMs int64
}

// Blackout is a zone with no radio link at all.
type Blackout struct {
	Name    string
	Center  domain.Position
	RadiusM float64
}

// Contains reports whether a position lies in the zone.
func (b Blackout) Contains(p domain.Position) bool {
	return domain.HaversineM(b.Center, p) <= b.RadiusM
}

// TerrainSpec is the ground a ground vehicle drives over.
type TerrainSpec struct {
	// CellM is the edge of a traversability cell.
	CellM float64
	// Blocked are the areas no ground vehicle can enter.
	Blocked []BlockedArea
}

// BlockedArea is one named impassable polygon.
type BlockedArea struct {
	Name    string
	Polygon domain.Polygon
}

// Trigger is what starts a scheduled fault.
type Trigger string

const (
	// TriggerTime fires at a mission time.
	TriggerTime Trigger = "time"
	// TriggerCoverage fires on the first tick coverage reaches a percentage.
	TriggerCoverage Trigger = "coverage"
)

// ScheduledFault is one fault and when to inject it.
type ScheduledFault struct {
	Trigger     Trigger
	AtMs        int64
	AtCoverage  float64 // percent, [0, 100]
	Fault       domain.Fault
	Description string
}

// Due reports whether the fault should be injected at a tick with the given
// mission time and coverage percentage.
func (f ScheduledFault) Due(tickMs int64, coveragePct float64) bool {
	if f.Trigger == TriggerCoverage {
		return coveragePct >= f.AtCoverage
	}
	return tickMs >= f.AtMs
}

// DefaultTerrainCellM is the traversability cell edge when a scenario blocks
// terrain without saying how finely.
const DefaultTerrainCellM = 25

// The YAML shape. Required values are pointers so absence is detectable.
type rawScenario struct {
	APIVersion string         `yaml:"apiVersion"`
	Name       string         `yaml:"name"`
	Seed       *uint64        `yaml:"seed"`
	Mission    string         `yaml:"mission"`
	World      string         `yaml:"world"`
	Plan       string         `yaml:"plan"`
	Kinematics *rawKinematics `yaml:"kinematics"`
	Comms      *rawComms      `yaml:"comms"`
	Blackouts  []rawBlackout  `yaml:"blackout_zones"`
	Terrain    *rawTerrain    `yaml:"terrain"`
	Sensors    *rawSensors    `yaml:"sensors"`
	Battery    *rawBattery    `yaml:"battery"`
	Faults     []rawFault     `yaml:"faults"`
}

type rawKinematics struct {
	ClimbRateMps *float64 `yaml:"climb_rate_mps"`
}

type rawComms struct {
	RangeM   float64 `yaml:"range_m"`
	LossPct  float64 `yaml:"loss_pct"`
	JitterMs int64   `yaml:"jitter_ms"`
}

type rawPosition struct {
	Lat  *float64 `yaml:"lat"`
	Lon  *float64 `yaml:"lon"`
	AltM float64  `yaml:"alt_m"`
}

type rawBlackout struct {
	Name    string       `yaml:"name"`
	Center  *rawPosition `yaml:"center"`
	RadiusM *float64     `yaml:"radius_m"`
}

type rawTerrain struct {
	CellM   float64      `yaml:"cell_m"`
	Blocked []rawBlocked `yaml:"blocked"`
}

type rawBlocked struct {
	Name    string        `yaml:"name"`
	Polygon []rawPosition `yaml:"polygon"`
}

type rawSensors struct {
	GPSNoiseM float64 `yaml:"gps_noise_m"`
}

type rawBattery struct {
	HoverDrainPctPerMin float64 `yaml:"hover_drain_pct_per_min"`
}

type rawFault struct {
	AtMs          *int64   `yaml:"at_ms"`
	AtCoveragePct *float64 `yaml:"at_coverage_pct"`
	Vector        string   `yaml:"vector"`
	Kind          string   `yaml:"kind"`
	Magnitude     float64  `yaml:"magnitude"`
	DurationMs    int64    `yaml:"duration_ms"`
	Description   string   `yaml:"description"`
}

// ParseScenario reads one scenario from YAML.
//
// The schema is closed like a world's or a doctrine pack's: an unknown field,
// a duplicate key or a second document is an error. Every bound is checked
// here, so a scenario that parses cannot fail halfway through a run. Whether
// a fault names a vector of the fleet is checked when the world is built,
// since the fleet lives in the world file.
func ParseScenario(src []byte) (*Scenario, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true)
	var raw rawScenario
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: empty document", ErrInvalidScenario)
		}
		return nil, fmt.Errorf("%w: %w", ErrInvalidScenario, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: a scenario is a single YAML document", ErrInvalidScenario)
	}
	return buildScenario(raw)
}

func buildScenario(raw rawScenario) (*Scenario, error) {
	fail := func(format string, args ...any) (*Scenario, error) {
		return nil, fmt.Errorf("%w: "+format, append([]any{ErrInvalidScenario}, args...)...)
	}
	if raw.APIVersion != ScenarioAPIVersion {
		return fail("apiVersion %q, want %q", raw.APIVersion, ScenarioAPIVersion)
	}
	for _, f := range [...]struct{ name, value string }{
		{"name", raw.Name}, {"mission", raw.Mission}, {"world", raw.World}, {"plan", raw.Plan},
	} {
		if strings.TrimSpace(f.value) == "" {
			return fail("%s is required", f.name)
		}
	}
	if raw.Seed == nil {
		return fail("seed is required: a run without one is not reproducible")
	}
	sc := &Scenario{
		Name:      raw.Name,
		Seed:      *raw.Seed,
		Mission:   domain.MissionID(raw.Mission),
		WorldPath: raw.World,
		PlanPath:  raw.Plan,
	}

	if raw.Kinematics == nil || raw.Kinematics.ClimbRateMps == nil {
		return fail("kinematics.climb_rate_mps is required")
	}
	if c := *raw.Kinematics.ClimbRateMps; !(c > 0) || math.IsInf(c, 0) {
		return fail("kinematics.climb_rate_mps %v is not a finite positive number", c)
	}
	sc.Physics.ClimbRateMps = *raw.Kinematics.ClimbRateMps

	if c := raw.Comms; c != nil {
		switch {
		case !finite(c.RangeM) || c.RangeM < 0:
			return fail("comms.range_m %v is negative or not finite", c.RangeM)
		case !finite(c.LossPct) || c.LossPct < 0 || c.LossPct > 100:
			return fail("comms.loss_pct %v is outside [0, 100]", c.LossPct)
		case c.JitterMs < 0 || c.JitterMs%domain.TickIntervalMs != 0:
			return fail("comms.jitter_ms %d is not a non-negative whole number of %d ms ticks", c.JitterMs, domain.TickIntervalMs)
		}
		sc.Physics.Comms = Comms{RangeM: c.RangeM, LossPct: c.LossPct, JitterMs: c.JitterMs}
	}

	names := map[string]bool{}
	for i, b := range raw.Blackouts {
		if strings.TrimSpace(b.Name) == "" || names[b.Name] {
			return fail("blackout_zones[%d] has an empty or duplicate name %q", i, b.Name)
		}
		names[b.Name] = true
		if b.Center == nil {
			return fail("blackout_zones[%s].center is required", b.Name)
		}
		center, err := position(*b.Center)
		if err != nil {
			return fail("blackout_zones[%s].center: %v", b.Name, err)
		}
		if b.RadiusM == nil || !(*b.RadiusM > 0) || math.IsInf(*b.RadiusM, 0) {
			return fail("blackout_zones[%s].radius_m must be a finite positive number", b.Name)
		}
		sc.Physics.Blackouts = append(sc.Physics.Blackouts, Blackout{Name: b.Name, Center: center, RadiusM: *b.RadiusM})
	}

	if t := raw.Terrain; t != nil {
		cell := t.CellM
		if cell == 0 {
			cell = DefaultTerrainCellM
		}
		if !(cell > 0) || math.IsInf(cell, 0) {
			return fail("terrain.cell_m %v is not a finite positive number", t.CellM)
		}
		sc.Physics.Terrain.CellM = cell
		for i, b := range t.Blocked {
			if strings.TrimSpace(b.Name) == "" || names[b.Name] {
				return fail("terrain.blocked[%d] has an empty or duplicate name %q", i, b.Name)
			}
			names[b.Name] = true
			if len(b.Polygon) < 3 {
				return fail("terrain.blocked[%s] needs at least 3 vertices", b.Name)
			}
			var ring []domain.Position
			for j, v := range b.Polygon {
				p, err := position(v)
				if err != nil {
					return fail("terrain.blocked[%s].polygon[%d]: %v", b.Name, j, err)
				}
				ring = append(ring, p)
			}
			sc.Physics.Terrain.Blocked = append(sc.Physics.Terrain.Blocked, BlockedArea{Name: b.Name, Polygon: domain.Polygon{Ring: ring}})
		}
	}

	if s := raw.Sensors; s != nil {
		if !finite(s.GPSNoiseM) || s.GPSNoiseM < 0 {
			return fail("sensors.gps_noise_m %v is negative or not finite", s.GPSNoiseM)
		}
		sc.Physics.GPSNoiseM = s.GPSNoiseM
	}
	if b := raw.Battery; b != nil {
		if !finite(b.HoverDrainPctPerMin) || b.HoverDrainPctPerMin < 0 {
			return fail("battery.hover_drain_pct_per_min %v is negative or not finite", b.HoverDrainPctPerMin)
		}
		sc.Physics.HoverDrainPctPerMin = b.HoverDrainPctPerMin
	}

	for i, f := range raw.Faults {
		sf, err := scheduledFault(f)
		if err != nil {
			return fail("faults[%d]: %v", i, err)
		}
		sc.Faults = append(sc.Faults, sf)
	}
	return sc, nil
}

func scheduledFault(f rawFault) (ScheduledFault, error) {
	var sf ScheduledFault
	switch {
	case (f.AtMs == nil) == (f.AtCoveragePct == nil):
		return sf, errors.New("exactly one of at_ms and at_coverage_pct is required")
	case f.AtMs != nil:
		if *f.AtMs < 0 || *f.AtMs%domain.TickIntervalMs != 0 {
			return sf, fmt.Errorf("at_ms %d is not a non-negative whole number of ticks", *f.AtMs)
		}
		sf.Trigger, sf.AtMs = TriggerTime, *f.AtMs
	default:
		if c := *f.AtCoveragePct; !finite(c) || c < 0 || c > 100 {
			return sf, fmt.Errorf("at_coverage_pct %v is outside [0, 100]", c)
		}
		sf.Trigger, sf.AtCoverage = TriggerCoverage, *f.AtCoveragePct
	}
	sf.Fault = domain.Fault{Kind: domain.FaultKind(f.Kind), Vector: domain.VectorID(f.Vector), Magnitude: f.Magnitude, DurationMs: f.DurationMs}
	if err := sf.Fault.Check(); err != nil {
		return sf, err
	}
	sf.Description = f.Description
	return sf, nil
}

func position(v rawPosition) (domain.Position, error) {
	if v.Lat == nil || v.Lon == nil {
		return domain.Position{}, errors.New("lat and lon are required")
	}
	p := domain.Position{Lat: *v.Lat, Lon: *v.Lon, AltM: v.AltM}
	if !p.Finite() || p.Lat < -90 || p.Lat > 90 || p.Lon < -180 || p.Lon > 180 {
		return domain.Position{}, fmt.Errorf("%v is not a WGS84 position", p)
	}
	return p, nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
