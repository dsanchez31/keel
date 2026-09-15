// Package daemon is keeld: the orchestrator that binds a fleet of vectors,
// compiles operator intent into plans, runs the engine on the plan an
// operator approved, records every mission on the hash chain and serves all
// of it to the operator (spec section 16).
//
// It is the impure half of the tick loop. The engine decides; the daemon
// drains the fleet into events, hands them to engine.Step, records what Step
// returned and dispatches its commands. Nothing here decides anything a
// replay would have to reproduce: every input to a decision is an event in
// the log.
package daemon

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/planir"
	"github.com/dsanchez31/keel/internal/planner"
)

// ConfigAPIVersion is the only configuration schema version keeld reads.
const ConfigAPIVersion = "keel.daemon/v1"

// Defaults of the optional fields.
const (
	// DefaultListen is loopback: the API carries no authentication (spec
	// section 1), and a daemon reachable from the network would open its
	// human gate to anyone.
	DefaultListen = "127.0.0.1:8080"
	// DefaultCompileTimeout bounds one compilation, every attempt included.
	DefaultCompileTimeout = 5 * time.Minute
)

// ErrInvalidConfig reports a configuration that does not parse or validate.
var ErrInvalidConfig = errors.New("daemon: invalid configuration")

// Config is a keel.daemon/v1 file, read and resolved: paths are absolute or
// relative to the working directory, no longer to the file.
type Config struct {
	Name           string
	WorldPath      string
	DoctrineDir    string
	DataDir        string
	Listen         string
	TrustedOrigins []string
	// Seed seeds every mission's engine. Nil draws one per mission, recorded
	// in the mission's header like any seed.
	Seed    *uint64
	Planner PlannerConfig
	// MAVLink is the shared link of the MAVLink bindings, nil when there is
	// none.
	MAVLink *MAVLinkConfig
	// Vectors are the bindings, sorted by id.
	Vectors []Binding
}

// PlannerConfig selects the planner backend compile requests use by default.
type PlannerConfig struct {
	Backend string
	Model   string
	// OllamaURL empty means the caller's default: keeld reads OLLAMA_HOST.
	OllamaURL string
	Timeout   time.Duration
	// Think lets the model reason before replying, whichever backend a
	// compile request selects (planner.BackendOptions).
	Think bool
}

// MAVLinkConfig is the one UDP endpoint every MAVLink vehicle pushes to, and
// the geoid separation at the operating area (spec section 7.4).
type MAVLinkConfig struct {
	Listen           string
	GeoidSeparationM float64
}

// Binding is how one vector of the world's fleet is reached: exactly one of
// Native and MAVLink.
type Binding struct {
	ID      domain.VectorID
	Native  *NativeBinding
	MAVLink *MAVLinkBinding
}

// NativeBinding is a vehicle speaking the native protocol (spec section 7.3).
// Its capabilities are the ones its hello declares.
type NativeBinding struct {
	URL string
	// Faults is the fault endpoint of the simulator serving the vehicle
	// (keelsim --serve, spec section 15.4). Empty means the vehicle is not
	// simulated, and a fault on it is refused.
	Faults string
	// Clock is the clock endpoint of that simulator (keelsim --serve, spec
	// section 15.4), through which keeld brings the world to its own pace.
	// Empty means the vehicle keeps real time, and keeld with it.
	Clock string
}

// MAVLinkBinding is an autopilot on the shared MAVLink link. MAVLink carries
// no capabilities: the world's fleet entry for the id provides them.
type MAVLinkBinding struct {
	SystemID uint8
	// SITL says the autopilot is an ArduPilot SITL, whose SIM_SPEEDUP keeld
	// holds to its own pace. False means a vehicle in real time.
	SITL bool
}

// Simulated reports whether faults can be injected on the vector.
func (b Binding) Simulated() bool { return b.Native != nil && b.Native.Faults != "" }

// TimeScalable reports whether the vector's world can run at keeld's pace:
// a simulator whose clock keeld sets. A real vehicle cannot, and while one is
// bound keeld stays in real time (spec section 16.5).
func (b Binding) TimeScalable() bool {
	return (b.Native != nil && b.Native.Clock != "") || (b.MAVLink != nil && b.MAVLink.SITL)
}

// Binding returns the binding of a vector.
func (c *Config) Binding(id domain.VectorID) (Binding, bool) {
	i, ok := slices.BinarySearchFunc(c.Vectors, id, func(b Binding, id domain.VectorID) int { return cmp.Compare(b.ID, id) })
	if !ok {
		return Binding{}, false
	}
	return c.Vectors[i], true
}

// ReadConfig reads a configuration file and resolves its paths against the
// file's directory, so a configuration means the same from wherever keeld
// starts.
func ReadConfig(path string) (*Config, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the configuration: %w", err)
	}
	c, err := ParseConfig(src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dir := filepath.Dir(path)
	for _, p := range []*string{&c.WorldPath, &c.DoctrineDir, &c.DataDir} {
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	return c, nil
}

// CheckFleet holds the bindings to a world: every bound id is a vector of its
// fleet, which a MAVLink binding takes its capabilities from. A fleet vector
// left unbound is not flown.
func (c *Config) CheckFleet(w *planir.World) error {
	for _, b := range c.Vectors {
		if _, ok := fleetVector(w, b.ID); !ok {
			return fmt.Errorf("%w: vector %s is not in the fleet of world %s", ErrInvalidConfig, b.ID, w.Name)
		}
	}
	return nil
}

func fleetVector(w *planir.World, id domain.VectorID) (planir.FleetVector, bool) {
	i, ok := slices.BinarySearchFunc(w.Fleet, id, func(v planir.FleetVector, id domain.VectorID) int { return cmp.Compare(v.Caps.ID, id) })
	if !ok {
		return planir.FleetVector{}, false
	}
	return w.Fleet[i], true
}

// The YAML shape. Optional numbers are pointers so absence is detectable.
type rawConfig struct {
	APIVersion     string       `yaml:"apiVersion"`
	Name           string       `yaml:"name"`
	World          string       `yaml:"world"`
	DoctrineDir    string       `yaml:"doctrine_dir"`
	DataDir        string       `yaml:"data_dir"`
	Listen         string       `yaml:"listen"`
	TrustedOrigins []string     `yaml:"trusted_origins"`
	Seed           *uint64      `yaml:"seed"`
	Planner        rawPlanner   `yaml:"planner"`
	MAVLink        *rawMAVLink  `yaml:"mavlink"`
	Vectors        []rawBinding `yaml:"vectors"`
}

type rawPlanner struct {
	Backend   string `yaml:"backend"`
	Model     string `yaml:"model"`
	OllamaURL string `yaml:"ollama_url"`
	TimeoutMs *int64 `yaml:"timeout_ms"`
	Think     bool   `yaml:"think"`
}

type rawMAVLink struct {
	Listen           string  `yaml:"listen"`
	GeoidSeparationM float64 `yaml:"geoid_separation_m"`
}

type rawBinding struct {
	ID      string             `yaml:"id"`
	Native  *rawNative         `yaml:"native"`
	MAVLink *rawMAVLinkBinding `yaml:"mavlink"`
}

type rawNative struct {
	URL    string `yaml:"url"`
	Faults string `yaml:"faults"`
	Clock  string `yaml:"clock"`
}

type rawMAVLinkBinding struct {
	SysID *int `yaml:"sysid"`
	SITL  bool `yaml:"sitl"`
}

// ParseConfig reads one configuration from YAML, paths left as written.
//
// The schema is closed like a world's or a scenario's: an unknown field, a
// duplicate key or a second document is an error, and every value is checked
// here, so a daemon whose configuration parses does not fail on it later.
// Whether the bound ids are in the fleet is CheckFleet's, the fleet living in
// the world file.
func ParseConfig(src []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true)
	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: empty document", ErrInvalidConfig)
		}
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: a configuration is a single YAML document", ErrInvalidConfig)
	}
	c, err := buildConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	return c, nil
}

func buildConfig(raw rawConfig) (*Config, error) {
	if raw.APIVersion != ConfigAPIVersion {
		return nil, fmt.Errorf("apiVersion %q, want %q", raw.APIVersion, ConfigAPIVersion)
	}
	for _, f := range [...]struct{ name, value string }{
		{"name", raw.Name}, {"world", raw.World}, {"doctrine_dir", raw.DoctrineDir}, {"data_dir", raw.DataDir},
	} {
		if strings.TrimSpace(f.value) == "" {
			return nil, fmt.Errorf("%s is required", f.name)
		}
	}
	c := &Config{
		Name:        raw.Name,
		WorldPath:   raw.World,
		DoctrineDir: raw.DoctrineDir,
		DataDir:     raw.DataDir,
		Listen:      cmp.Or(raw.Listen, DefaultListen),
		Seed:        raw.Seed,
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return nil, fmt.Errorf("listen %q: %v", c.Listen, err)
	}
	for _, o := range raw.TrustedOrigins {
		u, err := url.Parse(o)
		if err != nil || u.Scheme == "" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			return nil, fmt.Errorf("trusted origin %q, want scheme://host[:port]", o)
		}
		c.TrustedOrigins = append(c.TrustedOrigins, o)
	}

	c.Planner = PlannerConfig{
		Backend:   cmp.Or(raw.Planner.Backend, planner.BackendOllama),
		Model:     raw.Planner.Model,
		OllamaURL: raw.Planner.OllamaURL,
		Timeout:   DefaultCompileTimeout,
		Think:     raw.Planner.Think,
	}
	if !slices.Contains(planner.Backends(), c.Planner.Backend) {
		return nil, fmt.Errorf("planner.backend %q, want %s", c.Planner.Backend, strings.Join(planner.Backends(), " or "))
	}
	if u := c.Planner.OllamaURL; u != "" {
		if err := checkURL(u, "http", "https"); err != nil {
			return nil, fmt.Errorf("planner.ollama_url: %v", err)
		}
	}
	if t := raw.Planner.TimeoutMs; t != nil {
		if *t <= 0 {
			return nil, fmt.Errorf("planner.timeout_ms %d is not positive", *t)
		}
		c.Planner.Timeout = time.Duration(*t) * time.Millisecond
	}

	if m := raw.MAVLink; m != nil {
		if _, _, err := net.SplitHostPort(m.Listen); err != nil {
			return nil, fmt.Errorf("mavlink.listen %q: %v", m.Listen, err)
		}
		if math.IsNaN(m.GeoidSeparationM) || math.IsInf(m.GeoidSeparationM, 0) || math.Abs(m.GeoidSeparationM) > 200 {
			return nil, fmt.Errorf("mavlink.geoid_separation_m %v is outside [-200, 200]", m.GeoidSeparationM)
		}
		c.MAVLink = &MAVLinkConfig{Listen: m.Listen, GeoidSeparationM: m.GeoidSeparationM}
	}

	if len(raw.Vectors) == 0 {
		return nil, errors.New("vectors: at least one binding is required")
	}
	sysids := map[uint8]domain.VectorID{}
	for i, rb := range raw.Vectors {
		b, err := binding(rb)
		if err != nil {
			return nil, fmt.Errorf("vectors[%d]: %v", i, err)
		}
		if b.MAVLink != nil {
			if c.MAVLink == nil {
				return nil, fmt.Errorf("vectors[%d]: %s is bound over MAVLink and the mavlink block is missing", i, b.ID)
			}
			if other, dup := sysids[b.MAVLink.SystemID]; dup {
				return nil, fmt.Errorf("vectors[%d]: system id %d is bound to %s already", i, b.MAVLink.SystemID, other)
			}
			sysids[b.MAVLink.SystemID] = b.ID
		}
		c.Vectors = append(c.Vectors, b)
	}
	slices.SortFunc(c.Vectors, func(a, b Binding) int { return cmp.Compare(a.ID, b.ID) })
	for i := 1; i < len(c.Vectors); i++ {
		if c.Vectors[i].ID == c.Vectors[i-1].ID {
			return nil, fmt.Errorf("vectors: %s is bound twice", c.Vectors[i].ID)
		}
	}
	return c, nil
}

func binding(rb rawBinding) (Binding, error) {
	id := strings.TrimSpace(rb.ID)
	if id == "" || strings.Contains(id, "/") {
		return Binding{}, fmt.Errorf("id %q, want a non-empty vector id", rb.ID)
	}
	b := Binding{ID: domain.VectorID(id)}
	switch {
	case (rb.Native == nil) == (rb.MAVLink == nil):
		return Binding{}, fmt.Errorf("%s: exactly one of native and mavlink is required", id)
	case rb.Native != nil:
		if err := checkURL(rb.Native.URL, "ws", "wss"); err != nil {
			return Binding{}, fmt.Errorf("%s: native.url: %v", id, err)
		}
		if f := rb.Native.Faults; f != "" {
			if err := checkURL(f, "http", "https"); err != nil {
				return Binding{}, fmt.Errorf("%s: native.faults: %v", id, err)
			}
		}
		if c := rb.Native.Clock; c != "" {
			if err := checkURL(c, "http", "https"); err != nil {
				return Binding{}, fmt.Errorf("%s: native.clock: %v", id, err)
			}
		}
		b.Native = &NativeBinding{URL: rb.Native.URL, Faults: rb.Native.Faults, Clock: rb.Native.Clock}
	default:
		s := rb.MAVLink.SysID
		// 0 is MAVLink's broadcast address, not a vehicle.
		if s == nil || *s < 1 || *s > 255 {
			return Binding{}, fmt.Errorf("%s: mavlink.sysid is required, in [1, 255]", id)
		}
		b.MAVLink = &MAVLinkBinding{SystemID: uint8(*s), SITL: rb.MAVLink.SITL}
	}
	return b, nil
}

// checkURL requires an absolute URL of one of the schemes.
func checkURL(raw string, schemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if !slices.Contains(schemes, u.Scheme) || u.Host == "" {
		return fmt.Errorf("%q, want an absolute %s URL", raw, strings.Join(schemes, " or "))
	}
	return nil
}
