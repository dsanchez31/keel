package world

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
)

func inject(t *testing.T, fs *faultState, bat *battery, f domain.Fault, nowMs int64) {
	t.Helper()
	if err := fs.inject(f, nowMs, bat, rand.New(rand.NewPCG(3, 4))); err != nil {
		t.Fatalf("inject %s: %v", f.Kind, err)
	}
}

func TestLinkLossLastsItsDuration(t *testing.T) {
	var fs faultState
	bat := battery{pct: 100}
	inject(t, &fs, &bat, domain.Fault{Kind: domain.FaultLinkLoss, DurationMs: 2000}, 1000)
	if fs.linkCut(900) || !fs.linkCut(1000) || !fs.linkCut(2900) || fs.linkCut(3000) {
		t.Fatal("a 2 s link loss from 1 s holds on [1 s, 3 s)")
	}
	inject(t, &fs, &bat, domain.Fault{Kind: domain.FaultLinkLoss}, 5000)
	if !fs.linkCut(1 << 40) {
		t.Fatal("a link loss without duration lasts for good")
	}
}

func TestBatteryDrainAndKill(t *testing.T) {
	var fs faultState
	bat := battery{pct: 80}
	inject(t, &fs, &bat, domain.Fault{Kind: domain.FaultBatteryDrain, Magnitude: 50}, 0)
	if bat.reported() != 30 {
		t.Fatalf("battery %d%% after a 50%% drain from 80%%", bat.reported())
	}
	inject(t, &fs, &bat, domain.Fault{Kind: domain.FaultKill}, 0)
	if !fs.killed {
		t.Fatal("kill did not take")
	}
}

// The drift grows at its rate in a unit direction and vanishes when the
// fault ends.
func TestGPSDriftWalksAndRecovers(t *testing.T) {
	var fs faultState
	bat := battery{pct: 100}
	inject(t, &fs, &bat, domain.Fault{Kind: domain.FaultGPSDrift, Magnitude: 2, DurationMs: 10000}, 1000)
	if e, n := fs.drift(1000); e != 0 || n != 0 {
		t.Fatalf("offset %v, %v at the start", e, n)
	}
	e, n := fs.drift(6000)
	if d := math.Hypot(e, n); math.Abs(d-10) > 1e-9 {
		t.Fatalf("offset of %v m after 5 s at 2 m/s, want 10", d)
	}
	if e, n := fs.drift(11000); e != 0 || n != 0 {
		t.Fatalf("offset %v, %v after the fault ended", e, n)
	}
}

// A stale link repeats the last frame sent before the fault, timestamp and
// all, then the vehicle speaks again.
func TestStaleTelemetryRepeatsTheLastFrame(t *testing.T) {
	var fs faultState
	bat := battery{pct: 100}
	before := domain.VectorState{ID: "V", BatteryPct: 90, LastSeenMs: 400}
	fs.sent(before, 400)
	inject(t, &fs, &bat, domain.Fault{Kind: domain.FaultStaleTelemetry, DurationMs: 1000}, 500)

	for _, now := range []int64{500, 900, 1400} {
		got, ok := fs.stale(now)
		if !ok || got != before {
			t.Fatalf("at %d ms: %+v %v, want the frame of 400 ms repeated", now, got, ok)
		}
	}
	if _, ok := fs.stale(1500); ok {
		t.Fatal("still stale after the fault ended")
	}
}

func TestStaleBeforeAnyFrameFreezesTheFirst(t *testing.T) {
	var fs faultState
	bat := battery{pct: 100}
	inject(t, &fs, &bat, domain.Fault{Kind: domain.FaultStaleTelemetry}, 0)
	if _, ok := fs.stale(0); ok {
		t.Fatal("stale with nothing to repeat")
	}
	first := domain.VectorState{ID: "V", LastSeenMs: 100}
	fs.sent(first, 100)
	if got, ok := fs.stale(5000); !ok || got != first {
		t.Fatalf("got %+v %v, want the first frame frozen", got, ok)
	}
}
