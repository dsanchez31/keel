package planir

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dsanchez31/keel/internal/coverage"
	"github.com/dsanchez31/keel/internal/domain"
)

// WorldAPIVersion is the only world file version this package reads.
const WorldAPIVersion = "keel.world/v1"

// ErrInvalidWorld reports a world file that does not parse or validate.
var ErrInvalidWorld = errors.New("planir: invalid world")

// World is what a Plan IR is resolved against: the named areas, the named
// ground stations and a snapshot of the fleet.
//
// Names are the only thing a planner can refer to. The geometry behind a name
// lives here, server side, and never reaches the model.
type World struct {
	Name     string
	Areas    []AreaSpec       // sorted by name
	Stations []domain.Station // sorted by name
	Fleet    []FleetVector    // sorted by id
}

// AreaSpec is one named AO with its raster and the parameters expansion takes
// from the server rather than from the model.
type AreaSpec struct {
	Area domain.Area
	// ScanAltM is the altitude given to every waypoint of a lane over this
	// area, in metres above the WGS84 ellipsoid.
	ScanAltM float64
}

// FleetVector is one vector of the snapshot: what it declares and its last
// known state.
type FleetVector struct {
	Caps  domain.Capabilities
	State domain.VectorState
}

// Area returns the area with the given name.
func (w *World) Area(name string) (AreaSpec, bool) {
	i, ok := slices.BinarySearchFunc(w.Areas, name, func(a AreaSpec, n string) int { return strings.Compare(a.Area.Name, n) })
	if !ok {
		return AreaSpec{}, false
	}
	return w.Areas[i], true
}

// Station returns the ground station with the given name.
func (w *World) Station(name string) (domain.Station, bool) {
	i, ok := slices.BinarySearchFunc(w.Stations, name, func(s domain.Station, n string) int { return strings.Compare(s.Name, n) })
	if !ok {
		return domain.Station{}, false
	}
	return w.Stations[i], true
}

// AreaNames lists every area name, sorted.
func (w *World) AreaNames() []string {
	out := make([]string, len(w.Areas))
	for i, a := range w.Areas {
		out[i] = a.Area.Name
	}
	return out
}

// StationNames lists every station name, sorted.
func (w *World) StationNames() []string {
	out := make([]string, len(w.Stations))
	for i, s := range w.Stations {
		out[i] = s.Name
	}
	return out
}

// The YAML shape. Required numbers are pointers so absence is detectable.
type rawWorld struct {
	APIVersion string       `yaml:"apiVersion"`
	Name       string       `yaml:"name"`
	Areas      []rawArea    `yaml:"areas"`
	Stations   []rawStation `yaml:"stations"`
	Fleet      []rawVector  `yaml:"fleet"`
}

type rawArea struct {
	Name     string        `yaml:"name"`
	CellM    *float64      `yaml:"cell_m"`
	ScanAltM *float64      `yaml:"scan_alt_m"`
	Polygon  []rawPosition `yaml:"polygon"`
}

type rawPosition struct {
	Lat  *float64 `yaml:"lat"`
	Lon  *float64 `yaml:"lon"`
	AltM float64  `yaml:"alt_m"`
}

type rawStation struct {
	Name     string       `yaml:"name"`
	Position *rawPosition `yaml:"position"`
}

type rawVector struct {
	ID            string    `yaml:"id"`
	Domain        string    `yaml:"domain"`
	Tags          []string  `yaml:"tags"`
	CruiseSpeed   *float64  `yaml:"cruise_speed"`
	MaxRangeM     *float64  `yaml:"max_range_m"`
	SensorRadiusM *float64  `yaml:"sensor_radius_m"`
	State         *rawState `yaml:"state"`
}

type rawState struct {
	Position   *rawPosition `yaml:"position"`
	Heading    float64      `yaml:"heading"`
	Speed      float64      `yaml:"speed"`
	BatteryPct *int         `yaml:"battery_pct"`
	Link       string       `yaml:"link"`
	Mode       string       `yaml:"mode"`
}

// ParseWorld reads one world from YAML.
//
// The schema is closed like a doctrine pack's: an unknown field, a duplicate
// key or a second document is an error. Every area is rasterised here through
// coverage.NewGrid, so an AO whose raster is disconnected or empty is refused
// when the world loads, not when a plan first names it.
func ParseWorld(src []byte) (*World, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true)
	var raw rawWorld
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: empty document", ErrInvalidWorld)
		}
		return nil, fmt.Errorf("%w: %w", ErrInvalidWorld, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: a world is a single YAML document", ErrInvalidWorld)
	}
	return buildWorld(raw)
}

func buildWorld(raw rawWorld) (*World, error) {
	fail := func(format string, args ...any) (*World, error) {
		return nil, fmt.Errorf("%w: "+format, append([]any{ErrInvalidWorld}, args...)...)
	}
	if raw.APIVersion != WorldAPIVersion {
		return fail("apiVersion %q, want %q", raw.APIVersion, WorldAPIVersion)
	}
	if strings.TrimSpace(raw.Name) == "" {
		return fail("name is required")
	}
	w := &World{Name: raw.Name}

	for i, a := range raw.Areas {
		if strings.TrimSpace(a.Name) == "" {
			return fail("areas[%d].name is required", i)
		}
		if a.CellM == nil || a.ScanAltM == nil {
			return fail("areas[%s] needs cell_m and scan_alt_m", a.Name)
		}
		if !finite(*a.ScanAltM) {
			return fail("areas[%s].scan_alt_m is not finite", a.Name)
		}
		ring := make([]domain.Position, len(a.Polygon))
		for j, v := range a.Polygon {
			p, err := position(v)
			if err != nil {
				return fail("areas[%s].polygon[%d]: %v", a.Name, j, err)
			}
			ring[j] = p
		}
		area, err := coverage.NewGrid(a.Name, domain.Polygon{Ring: ring}, *a.CellM)
		if err != nil {
			return fail("areas[%s]: %v", a.Name, err)
		}
		w.Areas = append(w.Areas, AreaSpec{Area: area, ScanAltM: *a.ScanAltM})
	}
	slices.SortFunc(w.Areas, func(a, b AreaSpec) int { return strings.Compare(a.Area.Name, b.Area.Name) })
	if dup := duplicate(w.AreaNames()); dup != "" {
		return fail("area %q is declared twice", dup)
	}

	for i, s := range raw.Stations {
		if strings.TrimSpace(s.Name) == "" {
			return fail("stations[%d].name is required", i)
		}
		if s.Position == nil {
			return fail("stations[%s].position is required", s.Name)
		}
		p, err := position(*s.Position)
		if err != nil {
			return fail("stations[%s].position: %v", s.Name, err)
		}
		w.Stations = append(w.Stations, domain.Station{Name: s.Name, Position: p})
	}
	slices.SortFunc(w.Stations, func(a, b domain.Station) int { return strings.Compare(a.Name, b.Name) })
	if dup := duplicate(w.StationNames()); dup != "" {
		return fail("station %q is declared twice", dup)
	}

	ids := make([]string, 0, len(raw.Fleet))
	for i, v := range raw.Fleet {
		fv, err := fleetVector(v)
		if err != nil {
			return fail("fleet[%d]: %v", i, err)
		}
		w.Fleet = append(w.Fleet, fv)
		ids = append(ids, string(fv.Caps.ID))
	}
	slices.SortFunc(w.Fleet, func(a, b FleetVector) int { return strings.Compare(string(a.Caps.ID), string(b.Caps.ID)) })
	slices.Sort(ids)
	if dup := duplicate(ids); dup != "" {
		return fail("vector %q is declared twice", dup)
	}
	return w, nil
}

func fleetVector(v rawVector) (FleetVector, error) {
	if strings.TrimSpace(v.ID) == "" {
		return FleetVector{}, errors.New("id is required")
	}
	caps := domain.Capabilities{ID: domain.VectorID(v.ID), Domain: domain.Domain(v.Domain), Tags: slices.Clone(v.Tags)}
	if !domain.ValidDomain(caps.Domain) {
		return FleetVector{}, fmt.Errorf("%s: domain %q, want aerial or ground", v.ID, v.Domain)
	}
	for _, tag := range caps.Tags {
		if strings.TrimSpace(tag) == "" {
			return FleetVector{}, fmt.Errorf("%s: empty tag", v.ID)
		}
	}
	caps.SortTags()
	if v.CruiseSpeed == nil || v.MaxRangeM == nil || v.SensorRadiusM == nil {
		return FleetVector{}, fmt.Errorf("%s needs cruise_speed, max_range_m and sensor_radius_m", v.ID)
	}
	caps.CruiseSpeed, caps.MaxRangeM, caps.SensorRadiusM = *v.CruiseSpeed, *v.MaxRangeM, *v.SensorRadiusM
	if !(caps.CruiseSpeed > 0) || !(caps.MaxRangeM > 0) || !finite(caps.CruiseSpeed) || !finite(caps.MaxRangeM) {
		return FleetVector{}, fmt.Errorf("%s: cruise_speed and max_range_m must be finite and positive", v.ID)
	}
	if !(caps.SensorRadiusM >= 0) || !finite(caps.SensorRadiusM) {
		return FleetVector{}, fmt.Errorf("%s: sensor_radius_m must be finite and not negative", v.ID)
	}

	if v.State == nil || v.State.Position == nil || v.State.BatteryPct == nil {
		return FleetVector{}, fmt.Errorf("%s needs state.position and state.battery_pct", v.ID)
	}
	pos, err := position(*v.State.Position)
	if err != nil {
		return FleetVector{}, fmt.Errorf("%s: state.position: %w", v.ID, err)
	}
	st := domain.VectorState{
		ID:         caps.ID,
		Position:   pos,
		Heading:    v.State.Heading,
		Speed:      v.State.Speed,
		BatteryPct: *v.State.BatteryPct,
		Link:       domain.LinkState(v.State.Link),
		Mode:       domain.VectorMode(v.State.Mode),
	}
	if st.BatteryPct < 0 || st.BatteryPct > 100 {
		return FleetVector{}, fmt.Errorf("%s: battery_pct %d outside [0, 100]", v.ID, st.BatteryPct)
	}
	switch st.Link {
	case domain.LinkOK, domain.LinkDegraded, domain.LinkLost:
	default:
		return FleetVector{}, fmt.Errorf("%s: link %q, want ok, degraded or lost", v.ID, v.State.Link)
	}
	switch st.Mode {
	case domain.ModeIdle, domain.ModeTransit, domain.ModeScanning, domain.ModeRTB, domain.ModeDown:
	default:
		return FleetVector{}, fmt.Errorf("%s: mode %q, want idle, transit, scanning, rtb or down", v.ID, v.State.Mode)
	}
	if !finite(st.Heading) || !finite(st.Speed) {
		return FleetVector{}, fmt.Errorf("%s: heading and speed must be finite", v.ID)
	}
	return FleetVector{Caps: caps, State: st}, nil
}

func position(v rawPosition) (domain.Position, error) {
	if v.Lat == nil || v.Lon == nil {
		return domain.Position{}, errors.New("lat and lon are required")
	}
	p := domain.Position{Lat: *v.Lat, Lon: *v.Lon, AltM: v.AltM}
	if !p.Finite() {
		return domain.Position{}, errors.New("not finite")
	}
	if p.Lat < -90 || p.Lat > 90 || p.Lon < -180 || p.Lon > 180 {
		return domain.Position{}, fmt.Errorf("lat %v, lon %v outside WGS84 bounds", p.Lat, p.Lon)
	}
	return p, nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// duplicate returns the first repeated name of a sorted list, or "".
func duplicate(sorted []string) string {
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return sorted[i]
		}
	}
	return ""
}
