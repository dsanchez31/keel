package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/files"
)

// The shipped artifacts, relative to this package.
var (
	keelsimConfig   = filepath.Join("..", "..", "examples", "keeld", "keelsim.yaml")
	referenceConfig = filepath.Join("..", "..", "examples", "keeld", "reference.yaml")
	referenceWorld  = filepath.Join("..", "..", "examples", "worlds", "reference.yaml")
	shippedPacks    = filepath.Join("..", "..", "doctrine-packs")
)

func TestShippedConfigsLoad(t *testing.T) {
	w, err := files.ReadWorld(referenceWorld)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{keelsimConfig, referenceConfig} {
		c, err := ReadConfig(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if err := c.CheckFleet(w); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(c.Vectors) != len(w.Fleet) {
			t.Errorf("%s binds %d vectors, want the whole fleet of %d", path, len(c.Vectors), len(w.Fleet))
		}
		if c.WorldPath != filepath.Join("..", "..", "examples", "worlds", "reference.yaml") {
			t.Errorf("%s: world %q, want it resolved against the file", path, c.WorldPath)
		}
		if _, err := files.ReadPacks(c.DoctrineDir); err != nil {
			t.Errorf("%s: doctrine_dir: %v", path, err)
		}
		if b, ok := c.Binding("DRONE-02"); !ok || !b.Simulated() {
			t.Errorf("%s: DRONE-02 %+v, want it simulated for the demo's fault", path, b)
		}
		for _, b := range c.Vectors {
			if !b.TimeScalable() {
				t.Errorf("%s: %s %+v cannot follow keeld's pace, and the demo runs faster than real time", path, b.ID, b)
			}
		}
	}

	c, err := ReadConfig(referenceConfig)
	if err != nil {
		t.Fatal(err)
	}
	for id, sysid := range map[domain.VectorID]uint8{"DRONE-01": 1, "UGV-01": 2} {
		if b, _ := c.Binding(id); b.MAVLink == nil || b.MAVLink.SystemID != sysid || b.Simulated() {
			t.Errorf("reference: %s %+v, want MAVLink system id %d, not simulated", id, b, sysid)
		}
	}
	if b, _ := c.Binding("DRONE-01"); !b.MAVLink.SITL {
		t.Errorf("reference: DRONE-01 %+v, want a SITL", b.MAVLink)
	}
	if c.MAVLink == nil || c.MAVLink.Listen != "127.0.0.1:14550" {
		t.Errorf("reference: mavlink %+v", c.MAVLink)
	}
}

const minimal = `apiVersion: keel.daemon/v1
name: t
world: w.yaml
doctrine_dir: packs
data_dir: data
vectors:
  - id: V-1
    native: {url: "ws://127.0.0.1:8090/v1/vectors/V-1"}
`

func TestConfigDefaults(t *testing.T) {
	c, err := ParseConfig([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != DefaultListen || c.Planner.Backend != "ollama" || c.Planner.Timeout != DefaultCompileTimeout || c.Planner.Think || c.Seed != nil || c.MAVLink != nil {
		t.Fatalf("defaults %+v", c)
	}
	if b, _ := c.Binding("V-1"); b.Simulated() {
		t.Fatal("a binding without a fault endpoint is simulated")
	}

	c, err = ParseConfig([]byte(minimal + "seed: 7\nplanner: {backend: claude, timeout_ms: 1000, think: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Seed == nil || *c.Seed != 7 || c.Planner.Backend != "claude" || c.Planner.Timeout != time.Second || !c.Planner.Think {
		t.Fatalf("configured %+v", c)
	}
}

func TestConfigRefuses(t *testing.T) {
	native := func(id string) string {
		return "  - id: " + id + "\n    native: {url: \"ws://h:1/v1/vectors/" + id + "\"}\n"
	}
	head := "apiVersion: keel.daemon/v1\nname: t\nworld: w.yaml\ndoctrine_dir: p\ndata_dir: d\n"
	cases := []struct{ name, src, want string }{
		{"empty", "", "empty document"},
		{"version", strings.Replace(minimal, "v1", "v0", 1), "apiVersion"},
		{"no name", strings.Replace(minimal, "name: t\n", "", 1), "name is required"},
		{"no data dir", strings.Replace(minimal, "data_dir: data\n", "", 1), "data_dir is required"},
		{"unknown field", minimal + "debug: true\n", "debug"},
		{"duplicate key", minimal + "name: u\n", "already defined"},
		{"two documents", minimal + "---\n" + minimal, "single YAML document"},
		{"listen", minimal + "listen: 8080\n", "listen"},
		{"origin with a path", minimal + "trusted_origins: [\"http://localhost:5173/app\"]\n", "trusted origin"},
		{"backend", minimal + "planner: {backend: gpt}\n", "planner.backend"},
		{"timeout", minimal + "planner: {timeout_ms: 0}\n", "timeout_ms"}, {"no vector", head + "vectors: []\n", "at least one binding"},
		{"both adapters", head + "vectors:\n  - id: V\n    native: {url: \"ws://h:1/v\"}\n    mavlink: {sysid: 1}\nmavlink: {listen: \"127.0.0.1:14550\"}\n", "exactly one of native and mavlink"},
		{"neither adapter", head + "vectors:\n  - id: V\n", "exactly one of native and mavlink"},
		{"native scheme", head + "vectors:\n  - id: V\n    native: {url: \"http://h:1/v\"}\n", "native.url"},
		{"fault scheme", head + "vectors:\n  - id: V\n    native: {url: \"ws://h:1/v\", faults: \"ws://h:1/f\"}\n", "native.faults"},
		{"clock scheme", head + "vectors:\n  - id: V\n    native: {url: \"ws://h:1/v\", clock: \"h:1/c\"}\n", "native.clock"},
		{"no mavlink block", head + "vectors:\n  - id: V\n    mavlink: {sysid: 1}\n", "mavlink block is missing"},
		{"broadcast sysid", head + "mavlink: {listen: \"127.0.0.1:14550\"}\nvectors:\n  - id: V\n    mavlink: {sysid: 0}\n", "sysid"},
		{"sysid twice", head + "mavlink: {listen: \"127.0.0.1:14550\"}\nvectors:\n  - id: V\n    mavlink: {sysid: 1}\n  - id: W\n    mavlink: {sysid: 1}\n", "system id 1 is bound to V"},
		{"bound twice", head + "vectors:\n" + native("V") + native("V"), "V is bound twice"},
		{"geoid", head + "mavlink: {listen: \"127.0.0.1:14550\", geoid_separation_m: 900}\nvectors:\n" + native("V"), "geoid_separation_m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.src))
			if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want ErrInvalidConfig mentioning %q", err, tc.want)
			}
		})
	}
}

func TestCheckFleetRefusesAStranger(t *testing.T) {
	w, err := files.ReadWorld(referenceWorld)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseConfig([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckFleet(w); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "V-1 is not in the fleet") {
		t.Fatalf("err %v", err)
	}
}
