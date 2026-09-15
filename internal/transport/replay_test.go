package transport

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/missionlog"
)

// recording is the reference mission's log over its first ticks: the launch
// batch, then empty batches, recorded as the live loop records.
func recording(t *testing.T, ticks int) ([]byte, *doctrine.Registry, eventlog.Digest) {
	t.Helper()
	l := referenceLaunch(t)
	reg := referencePacks(t)
	log := eventlog.NewMemLog()
	cfg := engine.DefaultConfig()
	if err := missionlog.AppendHeader(log, missionlog.Header{Name: "reference", Seed: 42, Config: cfg, Plan: l.plan.Hash, Mission: l.plan.Mission}); err != nil {
		t.Fatal(err)
	}
	s := engine.NewState(42, cfg, reg)
	batch := l.batch
	for range ticks {
		next, cmds, decs := engine.Step(s, batch)
		if err := missionlog.AppendTick(log, next, batch, decs, cmds); err != nil {
			t.Fatal(err)
		}
		s, batch = next, nil
	}
	var b bytes.Buffer
	if _, err := log.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), reg, log.Head()
}

// windowTypes lists a window's frame types in order.
func windowTypes(w ReplayWindow) (out []FrameType) {
	for _, f := range w.Frames {
		out = append(out, f.Type)
	}
	return out
}

func TestReplayWindowStartsFromASnapshotAndThinsTelemetry(t *testing.T) {
	log, reg, _ := recording(t, 50)
	w, err := BuildReplayWindow(bytes.NewReader(log), reg, 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	first := w.Frames[0]
	if first.Type != FrameMission || first.Seq != 1 || first.T != 0 {
		t.Fatalf("first frame %s, seq %d, at %d ms; want the snapshot before the first tick", first.Type, first.Seq, first.T)
	}
	for i, f := range w.Frames {
		if f.Seq != uint64(i+1) {
			t.Fatalf("frame %d carries seq %d", i, f.Seq)
		}
	}
	// Ticks 1 to 20: telemetry and tick frames at ticks 10 and 20 only.
	if n, m := count(windowTypes(w), FrameTelemetry), count(windowTypes(w), FrameTick); n != 2 || m != 2 {
		t.Fatalf("%d telemetry and %d tick frames, want 2 each: %v", n, m, windowTypes(w))
	}
	if last := w.Frames[len(w.Frames)-1]; last.Type != FrameTick || last.T != 2000 {
		t.Fatalf("the window ends on a %s at %d ms, want the tick at 2000", last.Type, last.T)
	}
	if count(windowTypes(w), FrameDecision) == 0 {
		t.Fatal("the launch tick's decisions are missing")
	}
	if w.VerifiedMs != 2000 || w.Divergence != nil || w.End != nil {
		t.Fatalf("verified to %d, divergence %+v, end %+v", w.VerifiedMs, w.Divergence, w.End)
	}
}

func TestReplayWindowReachingTheEndCarriesBothHeads(t *testing.T) {
	log, reg, head := recording(t, 50)
	w, err := BuildReplayWindow(bytes.NewReader(log), reg, 3000, 30_000)
	if err != nil {
		t.Fatal(err)
	}
	var snap MissionView
	if err := json.Unmarshal(w.Frames[0].Data, &snap); err != nil {
		t.Fatal(err)
	}
	if w.Frames[0].T != 2900 || snap.TickMs != 2900 {
		t.Fatalf("snapshot at %d ms, view at %d; want the state after the tick at 2900", w.Frames[0].T, snap.TickMs)
	}
	if w.End == nil || w.End.TickMs != 5000 || w.End.Recorded != head.String() || w.End.Replayed != head.String() || w.VerifiedMs != 5000 {
		t.Fatalf("end %+v, verified to %d; want both heads %s at 5000 ms", w.End, w.VerifiedMs, head)
	}
}

func TestReplayWindowPastTheEndHoldsTheFinalView(t *testing.T) {
	log, reg, head := recording(t, 20)
	w, err := BuildReplayWindow(bytes.NewReader(log), reg, 6000, 7000)
	if err != nil {
		t.Fatal(err)
	}
	var v MissionView
	if len(w.Frames) != 1 || json.Unmarshal(w.Frames[0].Data, &v) != nil || v.TickMs != 2000 || v.Head != head.String() {
		t.Fatalf("frames %v, final view at %d ms with head %s; want the view after the last tick", windowTypes(w), v.TickMs, v.Head)
	}
	if v.Mission.State != domain.MissionRunning || w.End == nil {
		t.Fatalf("mission %s, end %+v", v.Mission.State, w.End)
	}
}

func TestCheckReplayWindow(t *testing.T) {
	for _, c := range []struct {
		from, to int64
		ok       bool
	}{{0, 0, true}, {0, MaxReplayWindowMs, true}, {-1, 10, false}, {10, 5, false}, {0, MaxReplayWindowMs + 1, false}} {
		if err := CheckReplayWindow(c.from, c.to); (err == nil) != c.ok {
			t.Errorf("CheckReplayWindow(%d, %d) = %v", c.from, c.to, err)
		}
	}
}
