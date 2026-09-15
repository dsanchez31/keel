package planir

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
)

// Shipped artifacts, relative to this package.
const (
	referenceWorldPath = "../../examples/worlds/reference.yaml"
	packsDir           = "../../doctrine-packs"
)

func loadReferenceWorld(t *testing.T) *World {
	t.Helper()
	src, err := os.ReadFile(referenceWorldPath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := ParseWorld(src)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func loadPacks(t *testing.T) *doctrine.Registry {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(packsDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var packs []*doctrine.Pack
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		p, err := doctrine.Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		packs = append(packs, p)
	}
	reg, err := doctrine.NewRegistry(packs...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// shoelaceKm2 is the polygon area in the local planar frame of its grid.
func shoelaceKm2(a domain.Area) float64 {
	mLat := domain.MetresPerDegreeLat()
	mLon := domain.MetresPerDegreeLon(a.Grid.RefLat)
	ring := a.Polygon.Ring
	var twice float64
	for i := range ring {
		j := (i + 1) % len(ring)
		xi, yi := ring[i].Lon*mLon, ring[i].Lat*mLat
		xj, yj := ring[j].Lon*mLon, ring[j].Lat*mLat
		twice += float64(xi*yj) - float64(xj*yi)
	}
	return math.Abs(twice) / 2 / 1e6
}

func TestReferenceWorldMatchesSpecScenario(t *testing.T) {
	w := loadReferenceWorld(t)

	ao, ok := w.Area("fog_of_war_east")
	if !ok {
		t.Fatalf("fog_of_war_east missing, have %v", w.AreaNames())
	}
	if km2 := shoelaceKm2(ao.Area); math.Abs(km2-14.3) > 14.3*0.02 {
		t.Fatalf("AO measures %.2f km², spec section 14 says roughly 14.3", km2)
	}
	if _, ok := w.Station("gcs-west"); !ok {
		t.Fatalf("gcs-west missing, have %v", w.StationNames())
	}

	count := func(tags ...string) int {
		slices.Sort(tags)
		n := 0
		for _, v := range w.Fleet {
			if slices.Equal(v.Caps.Tags, tags) {
				n++
			}
		}
		return n
	}
	if a, g, r := count("aerial", "camera", "gps"), count("ground", "camera", "gps"), count("aerial", "radio_mesh"); a != 4 || g != 1 || r != 1 {
		t.Fatalf("fleet has %d aerial, %d ground, %d relay vectors, spec section 14 has 4, 1, 1", a, g, r)
	}
	if !slices.IsSortedFunc(w.Fleet, func(a, b FleetVector) int { return strings.Compare(string(a.Caps.ID), string(b.Caps.ID)) }) {
		t.Fatal("fleet is not sorted by id")
	}
}

func TestParseWorldRejects(t *testing.T) {
	valid := `apiVersion: keel.world/v1
name: t
areas:
  - name: a
    cell_m: 100
    scan_alt_m: 100
    polygon:
      - {lat: 45.00, lon: 5.00}
      - {lat: 45.00, lon: 5.02}
      - {lat: 45.02, lon: 5.02}
      - {lat: 45.02, lon: 5.00}
stations:
  - name: s
    position: {lat: 45.0, lon: 4.99}
fleet:
  - id: V1
    domain: aerial
    tags: [camera, aerial]
    cruise_speed: 10
    max_range_m: 10000
    sensor_radius_m: 50
    state:
      position: {lat: 45.0, lon: 4.99}
      battery_pct: 90
      link: ok
      mode: idle
`
	w, err := ParseWorld([]byte(valid))
	if err != nil {
		t.Fatalf("valid world refused: %v", err)
	}
	if want := []string{"aerial", "camera"}; !slices.Equal(w.Fleet[0].Caps.Tags, want) {
		t.Fatalf("tags not sorted at construction: %v", w.Fleet[0].Caps.Tags)
	}

	cases := map[string]string{
		"unknown field":      strings.Replace(valid, "name: t", "name: t\nweather: fog", 1),
		"wrong api version":  strings.Replace(valid, "keel.world/v1", "keel.world/v2", 1),
		"second document":    valid + "---\n{}\n",
		"duplicate area":     strings.Replace(valid, "stations:", "  - name: a\n    cell_m: 100\n    scan_alt_m: 1\n    polygon: [{lat: 45, lon: 5}, {lat: 45, lon: 5.02}, {lat: 45.02, lon: 5.02}]\nstations:", 1),
		"missing cell size":  strings.Replace(valid, "    cell_m: 100\n", "", 1),
		"degenerate polygon": strings.Replace(valid, "      - {lat: 45.02, lon: 5.02}\n      - {lat: 45.02, lon: 5.00}\n", "", 1),
		"bad domain":         strings.Replace(valid, "domain: aerial", "domain: naval", 1),
		"bad link":           strings.Replace(valid, "link: ok", "link: great", 1),
		"battery above 100":  strings.Replace(valid, "battery_pct: 90", "battery_pct: 101", 1),
		"missing position":   strings.Replace(valid, "      position: {lat: 45.0, lon: 4.99}\n", "", 1),
		"duplicate vector":   valid + "  - id: V1\n    domain: aerial\n    tags: []\n    cruise_speed: 1\n    max_range_m: 1\n    sensor_radius_m: 1\n    state: {position: {lat: 45, lon: 5}, battery_pct: 1, link: ok, mode: idle}\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseWorld([]byte(src)); !errors.Is(err, ErrInvalidWorld) {
				t.Fatalf("got %v, want ErrInvalidWorld", err)
			}
		})
	}
}
