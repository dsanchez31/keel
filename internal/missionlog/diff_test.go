package missionlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
)

func resolve(t *testing.T, reg *doctrine.Registry, version string) *doctrine.Pack {
	t.Helper()
	p, err := reg.Resolve(domain.DoctrineRef{Name: "recon-standard", Version: version})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// withPack is the shipped registry plus recon-standard@3.0.0: 2.2.0 with the
// whole battery held in reserve, which leaves no range to the launch round and
// violates battery-reserve from the first tick.
func withPack(t *testing.T) *doctrine.Registry {
	t.Helper()
	var packs []*doctrine.Pack
	for _, v := range []string{"2.0.0", "2.1.0", "2.2.0"} {
		src, err := os.ReadFile(filepath.Join(root, "doctrine-packs", "recon-standard-v"+v+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := doctrine.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		packs = append(packs, p)
		if v == "2.2.0" {
			src = bytes.Replace(src, []byte("version: 2.2.0"), []byte("version: 3.0.0"), 1)
			src = bytes.Replace(src, []byte("battery_reserve_pct: 15"), []byte("battery_reserve_pct: 100"), 1)
			p, err := doctrine.Parse(src)
			if err != nil {
				t.Fatal(err)
			}
			packs = append(packs, p)
		}
	}
	reg, err := doctrine.NewRegistry(packs...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// The recorded pack against itself decides as the recording did, tick for
// tick.
func TestDiffOfAPackWithItselfIsEmpty(t *testing.T) {
	log := recorded(t, 150)
	reg := mustPacks(t)
	p := resolve(t, reg, "2.2.0")
	res, err := Diff(encoded(t, log), reg, p, p, func(d TickDiff) { t.Errorf("tick %d differs: %+v", d.TickMs, d) })
	if err != nil {
		t.Fatal(err)
	}
	if !res.Same() || res.Ticks != 150 || res.A.Decisions != count(log.Records(), eventlog.RecordDecision) {
		t.Fatalf("%d ticks, %d diverging, %d decisions under A", res.Ticks, res.Diverging, res.A.Decisions)
	}
}

// 2.1.0 and 2.2.0 differ only for a vector returning to base on a low
// battery, which the first 30 s of the reference run never see. The approval
// names its pack and the recorded swap to 2.1.0 leaves it, and neither is a
// difference: the diff reads the starting pack's identity away, and only
// there, since the swap's target is 2.1.0 in both replays.
func TestDiffReadsThePackIdentityAway(t *testing.T) {
	log := recorded(t, 300)
	reg := mustPacks(t)
	a, b := resolve(t, reg, "2.2.0"), resolve(t, reg, "2.1.0")
	res, err := Diff(encoded(t, log), reg, a, b, func(d TickDiff) { t.Errorf("tick %d differs: %+v", d.TickMs, d) })
	if err != nil {
		t.Fatal(err)
	}
	if !res.Same() || res.A.State.Pack.Ref != b.Ref || res.B.State.Pack.Ref != b.Ref {
		t.Fatalf("%d diverging ticks, final packs %s and %s", res.Diverging, res.A.State.Pack.Ref, res.B.State.Pack.Ref)
	}
}

// A pack that allocates differently diverges from the launch round, and every
// diverging tick lists what each pack decided that the other did not.
func TestDiffReportsDivergingDecisions(t *testing.T) {
	log := recorded(t, 120)
	reg := withPack(t)
	a, b := resolve(t, reg, "2.2.0"), resolve(t, reg, "3.0.0")
	var diffs []TickDiff
	res, err := Diff(encoded(t, log), reg, a, b, func(d TickDiff) { diffs = append(diffs, d) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Same() || res.FirstMs != engine.TickIntervalMs || int64(len(diffs)) != res.Diverging {
		t.Fatalf("first divergence at %d ms, %d diverging ticks, %d visited", res.FirstMs, res.Diverging, len(diffs))
	}
	first := diffs[0]
	if len(first.OnlyA) == 0 && len(first.OnlyB) == 0 {
		t.Fatalf("launch tick: only under A %+v, only under B %+v", first.OnlyA, first.OnlyB)
	}
	if res.A.State.Pack.Ref.Version != "2.1.0" || res.B.State.Pack.Ref.Version != "2.1.0" {
		t.Fatalf("final packs %s and %s, want both swapped to 2.1.0 as recorded", res.A.State.Pack.Ref, res.B.State.Pack.Ref)
	}
}

// A recording that approves no plan has no pack to substitute.
func TestDiffNeedsAnApprovedPlan(t *testing.T) {
	recs := recorded(t, 20).Records()
	var kept []eventlog.Record
	for _, r := range recs {
		if r.Kind == eventlog.RecordEvent {
			var ev domain.Event
			if err := json.Unmarshal(r.Payload, &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Kind == domain.EventPlanApproved {
				continue
			}
		}
		kept = append(kept, r)
	}
	log := rewritten(t, kept, func(r eventlog.Record) []byte { return r.Payload })
	reg := mustPacks(t)
	p := resolve(t, reg, "2.2.0")
	if _, err := Diff(encoded(t, log), reg, p, p, nil); !errors.Is(err, ErrLayout) {
		t.Fatalf("err %v, want ErrLayout", err)
	}
}
