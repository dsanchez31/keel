package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/files"
	"github.com/dsanchez31/keel/internal/missionlog"
)

var updateReference = flag.Bool("update-reference", false, "rewrite testdata/reference.heads from the current code")

// referenceHeads pins the reference mission's chain rather than the 62 MB
// recording, which keelsim rebuilds byte for byte from the scenario's seed:
// the head every checkpointMs of mission time, and at the end. A change to a
// decision, to a record's layout or to the canonical encoding moves them, and
// the first checkpoint that moved bounds where.
var referenceHeads = filepath.Join("testdata", "reference.heads")

const checkpointMs = 100_000

type checkpoint struct {
	tickMs int64
	head   string
}

// TestReferenceReplay records the reference mission, replays the recording
// through Step, rebuilding and comparing every record (invariant I7), and
// holds its chain to the pinned heads. A deliberate change of behaviour
// re-pins them with `make record-reference`.
func TestReferenceReplay(t *testing.T) {
	dir := t.TempDir()
	opts := options{scenario: referenceScenario, doctrineDir: shippedPacks, out: dir, maxTicks: engine.DefaultConfig().MaxTicks, quiet: true}
	if err := simulate(context.Background(), io.Discard, opts); err != nil {
		t.Fatalf("recording the reference mission: %v", err)
	}

	packs, err := files.ReadPacks(shippedPacks)
	if err != nil {
		t.Fatal(err)
	}
	src, err := eventlog.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	var got []checkpoint
	var lastMs int64
	res, err := missionlog.Replay(src, missionlog.Options{Packs: packs, Verify: true, OnTick: func(rt missionlog.ReplayedTick) {
		lastMs = rt.TickMs
		if rt.TickMs%checkpointMs == 0 {
			got = append(got, checkpoint{rt.TickMs, rt.Head.String()})
		}
	}})
	if err != nil {
		t.Fatalf("replaying the reference mission: %v", err)
	}
	if d := res.Divergence; d != nil {
		t.Fatalf("the replay leaves the recording at record %d (%d ms): recorded %s, replayed %s",
			d.Seq, d.TickMs, payload(d.Recorded), payload(d.Replayed))
	}
	if !res.Complete || !res.Match() {
		t.Fatalf("replay: complete %t, recorded head %s, replayed %s", res.Complete, res.Recorded, res.Replayed)
	}
	if got := res.State.Mission.State; got != domain.MissionComplete {
		t.Errorf("the reference mission ends %s, want %s", got, domain.MissionComplete)
	}
	if lastMs%checkpointMs != 0 {
		got = append(got, checkpoint{lastMs, res.Replayed.String()})
	}

	if *updateReference {
		writeHeads(t, got)
		return
	}
	want := readHeads(t)
	for i := range max(len(got), len(want)) {
		switch {
		case i >= len(got):
			t.Fatalf("the mission now ends at %d ms, pinned to run to %d ms; a deliberate change re-pins it with `make record-reference`", lastMs, want[len(want)-1].tickMs)
		case i >= len(want):
			t.Fatalf("the mission now runs to %d ms, pinned to end at %d ms; a deliberate change re-pins it with `make record-reference`", lastMs, want[len(want)-1].tickMs)
		case got[i] != want[i]:
			t.Fatalf("the chain moved by %d ms: pinned %s at %d ms, now %s at %d ms; a deliberate change re-pins it with `make record-reference`",
				got[i].tickMs, want[i].head, want[i].tickMs, got[i].head, got[i].tickMs)
		}
	}
}

func payload(r *eventlog.Record) string {
	if r == nil {
		return "nothing"
	}
	return fmt.Sprintf("%s %s", r.Kind, r.Payload)
}

func readHeads(t *testing.T) []checkpoint {
	t.Helper()
	f, err := os.Open(referenceHeads)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []checkpoint
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("%s: %q, want <tick_ms> <head>", referenceHeads, line)
		}
		ms, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			t.Fatalf("%s: %v", referenceHeads, err)
		}
		out = append(out, checkpoint{ms, fields[1]})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("%s pins no head", referenceHeads)
	}
	return out
}

func writeHeads(t *testing.T, heads []checkpoint) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# The reference mission's chain (examples/sims/reference.yaml), pinned: its head\n")
	b.WriteString("# every 100 s of mission time and at the end, as <tick_ms> <head>. Written by\n")
	b.WriteString("# `make record-reference`; checked by TestReferenceReplay in cmd/keelsim.\n")
	for _, h := range heads {
		fmt.Fprintf(&b, "%d %s\n", h.tickMs, h.head)
	}
	if err := os.MkdirAll(filepath.Dir(referenceHeads), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(referenceHeads, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
