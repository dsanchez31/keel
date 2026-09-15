package doctrine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
)

// packsDir holds the shipped packs, relative to this package.
const packsDir = "../../doctrine-packs"

// loadShipped parses every pack shipped in doctrine-packs/.
func loadShipped(t *testing.T) *Registry {
	t.Helper()
	entries, err := os.ReadDir(packsDir)
	if err != nil {
		t.Fatal(err)
	}
	var packs []*Pack
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		src, err := os.ReadFile(filepath.Join(packsDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		p, err := Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		// The file name is the reference, so a pack is found where its
		// reference says it is.
		if want := p.Ref.Name + "-v" + p.Ref.Version + ".yaml"; e.Name() != want {
			t.Errorf("%s declares %s, want file name %s", e.Name(), p.Ref, want)
		}
		packs = append(packs, p)
	}
	reg, err := NewRegistry(packs...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func resolve(t *testing.T, reg *Registry, ref string) *Pack {
	t.Helper()
	r, err := ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	p, err := reg.Resolve(r)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestShippedPacks(t *testing.T) {
	reg := loadShipped(t)
	var refs []string
	for _, r := range reg.Refs() {
		refs = append(refs, r.String())
	}
	if got := strings.Join(refs, " "); got != "recon-standard@2.0.0 recon-standard@2.1.0 recon-standard@2.2.0" {
		t.Fatalf("shipped packs = %s", got)
	}
	v20 := resolve(t, reg, "recon-standard@2.0.0")
	v21 := resolve(t, reg, "recon-standard@2.1.0")
	if v20.Hash == v21.Hash {
		t.Fatal("2.0.0 and 2.1.0 share a hash")
	}
	if v20.Params.BatteryReservePct != 15 || v21.Params.BatteryReservePct != 15 {
		t.Fatalf("reserve = %d, %d", v20.Params.BatteryReservePct, v21.Params.BatteryReservePct)
	}
	r20, _ := v20.Rule("reassign-on-link-loss")
	r21, _ := v21.Rule("reassign-on-link-loss")
	if r20.When.WindowMs != 10_000 || r21.When.WindowMs != 5000 {
		t.Fatalf("link loss windows = %d, %d, want 10000, 5000", r20.When.WindowMs, r21.When.WindowMs)
	}
	if _, ok := v21.Rule("return-on-geofence-breach"); !ok {
		t.Fatal("2.1.0 has no return-on-geofence-breach")
	}
	if _, ok := v20.Rule("return-on-geofence-breach"); ok {
		t.Fatal("2.0.0 has return-on-geofence-breach")
	}
}

// Gate 4 runs on the projected initial state: a fleet idle at a base outside
// the AO satisfies the shipped constraints.
func TestShippedConstraintsAcceptIdleFleetAtBase(t *testing.T) {
	reg := loadShipped(t)
	for _, ref := range []string{"recon-standard@2.0.0", "recon-standard@2.1.0", "recon-standard@2.2.0"} {
		var fleet []Agent
		for _, id := range []string{"DRONE-01", "DRONE-02", "GROUND-01"} {
			a := testAgent(id)
			a.Mode = domain.ModeIdle
			a.Position = pos(500, -2000)
			fleet = append(fleet, a)
		}
		got, err := CheckConstraints(resolve(t, reg, ref), testEnv(t, 0, fleet...))
		if err != nil || len(got) != 0 {
			t.Errorf("%s: violations %+v, %v, want none", ref, got, err)
		}
	}
}

// The geofence bounds lane work: transiting outside the AO is allowed,
// scanning outside it is a breach, which 2.1.0 reacts to and 2.0.0 does not.
func TestShippedGeofence(t *testing.T) {
	reg := loadShipped(t)
	transit := testAgent("DRONE-01")
	transit.Mode = domain.ModeTransit
	transit.Position = pos(500, -500)
	stray := testAgent("DRONE-02")
	stray.Position = pos(500, 1500)

	for _, c := range []struct {
		ref   string
		fires bool
	}{{"recon-standard@2.0.0", false}, {"recon-standard@2.1.0", true}} {
		res := mustEvaluate(t, resolve(t, reg, c.ref), testEnv(t, 100, transit, stray), nil)
		if len(res.Violations) != 1 || res.Violations[0] != (Violation{Constraint: "geofence", Agent: "DRONE-02"}) {
			t.Errorf("%s: violations = %+v", c.ref, res.Violations)
		}
		fired := len(res.Firings) == 1 && res.Firings[0].Agent == "DRONE-02" && res.Firings[0].RuleID == "return-on-geofence-breach"
		if fired != c.fires || (!c.fires && len(res.Firings) != 0) {
			t.Errorf("%s: firings = %+v", c.ref, res.Firings)
		}
	}
}

// Returning home, rtb_cost_pct falls about as fast as battery_pct and the
// reserve flickers between held and violated. Under 2.1.0 every flicker
// re-fires rtb-on-low-battery for a vector already in rtb; 2.2.0 fires it once,
// for the vector still at work, and never again once it is on its way.
func TestShippedLowBatteryFiresOncePerReturn(t *testing.T) {
	reg := loadShipped(t)
	fired := func(ref string) int {
		pack := resolve(t, reg, ref)
		var windows []Window
		n := 0
		for k := int64(1); k <= 10; k++ {
			a := testAgent("DRONE-01")
			a.BatteryPct = 24 // reserve 15 + cost 10 = 25: violated
			if k > 1 {
				a.Mode = domain.ModeRTB
			}
			if k%2 == 0 {
				a.RTBCostPct = 9 // one step closer to home: held
			}
			res := mustEvaluate(t, pack, testEnv(t, k*domain.TickIntervalMs, a), windows)
			windows = res.Windows
			for _, f := range res.Firings {
				if f.RuleID == "rtb-on-low-battery" {
					n++
				}
			}
		}
		return n
	}
	if got := fired("recon-standard@2.1.0"); got != 5 {
		t.Errorf("2.1.0 fired %d times, want 5: once per flicker", got)
	}
	if got := fired("recon-standard@2.2.0"); got != 1 {
		t.Errorf("2.2.0 fired %d times, want once", got)
	}
}

// The reference scenario's hot swap: DRONE-02's link is lost at 1 s under
// 2.0.0 (10 s window) and the operator swaps to 2.1.0 (5 s window) at 4 s.
// The window keeps its start, so the rule fires 5 s after the loss rather
// than 5 s after the swap, or 10 s after the loss without the swap.
func TestShippedHotSwapLinkLoss(t *testing.T) {
	reg := loadShipped(t)
	v20 := resolve(t, reg, "recon-standard@2.0.0")
	v21 := resolve(t, reg, "recon-standard@2.1.0")

	run := func(swapAtMs int64) int64 {
		pack := v20
		var windows []Window
		for k := int64(1); k <= 200; k++ {
			ms := k * domain.TickIntervalMs
			if ms == swapAtMs {
				var err error
				if windows, _, err = HotSwap(pack, v21, windows); err != nil {
					t.Fatal(err)
				}
				pack = v21
			}
			lost := testAgent("DRONE-02")
			if ms >= 1000 {
				lost.Link = domain.LinkLost
			}
			res := mustEvaluate(t, pack, testEnv(t, ms, testAgent("DRONE-01"), lost), windows)
			windows = res.Windows
			for _, f := range res.Firings {
				if f.Agent == "DRONE-02" && f.RuleID == "reassign-on-link-loss" {
					return ms
				}
			}
		}
		return -1
	}
	if got := run(0); got != 11_000 {
		t.Errorf("2.0.0 alone fired at %d, want 11000", got)
	}
	if got := run(4000); got != 6000 {
		t.Errorf("swap to 2.1.0 at 4000 fired at %d, want 6000", got)
	}
}
