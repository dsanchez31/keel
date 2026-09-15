package missionlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/files"
	"github.com/dsanchez31/keel/internal/planir"
	"github.com/dsanchez31/keel/internal/world"
)

var root = filepath.Join("..", "..")

// recorded runs the reference scenario for ticks ticks through keelsim's
// closed loop (engine, log, world), with a pinned hot swap to 2.1.0 at tick
// 100 and DRONE-02 killed at tick 150, and returns the mission log.
func recorded(t *testing.T, ticks int64) *eventlog.MemLog {
	t.Helper()
	packs := mustPacks(t)
	w, err := files.ReadWorld(filepath.Join(root, "examples", "worlds", "reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(root, "examples", "sims", "reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sc, err := world.ParseScenario(src)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "examples", "plans", "reference.json"))
	if err != nil {
		t.Fatal(err)
	}
	var head struct {
		Intent string `json:"intent"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		t.Fatal(err)
	}
	res := planir.Validate(planir.Input{Intent: head.Intent, Raw: raw, World: w, Doctrines: packs, Arrival: engine.DefaultConfig().Arrival()})
	if !res.OK() {
		t.Fatalf("reference plan refused: %+v", res.Diagnostics)
	}
	plan := res.Plan.Clone()
	plan.Mission = sc.Mission

	var fleet []world.Vehicle
	for _, v := range w.Fleet {
		fleet = append(fleet, world.Vehicle{Caps: v.Caps, State: v.State})
	}
	var extent []domain.Position
	for _, a := range w.Areas {
		extent = append(extent, a.Area.Polygon.Ring...)
	}
	sim, err := world.New(world.Config{Seed: sc.Seed, Physics: sc.Physics, Stations: w.Stations, Fleet: fleet, Extent: extent})
	if err != nil {
		t.Fatal(err)
	}

	log := eventlog.NewMemLog()
	cfg := engine.DefaultConfig()
	if err := AppendHeader(log, Header{Name: sc.Name, Seed: sc.Seed, Config: cfg, Plan: plan.Hash, Mission: plan.Mission}); err != nil {
		t.Fatal(err)
	}
	to, err := packs.Resolve(domain.DoctrineRef{Name: "recon-standard", Version: "2.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	kill := domain.Fault{Kind: domain.FaultKill, Vector: "DRONE-02"}

	s := engine.NewState(sc.Seed, cfg, packs)
	batch := append(sim.Join(), domain.Event{Kind: domain.EventPlanApproved, Plan: &plan})
	for tick := int64(1); tick <= ticks; tick++ {
		next, cmds, decs := engine.Step(s, batch)
		s = next
		if err := AppendTick(log, s, batch, decs, cmds); err != nil {
			t.Fatal(err)
		}
		sim.Send(cmds)
		batch = sim.Step()
		switch tick {
		case 100:
			ref := to.Ref
			batch = append(batch, domain.Event{Kind: domain.EventDoctrineSwap, Doctrine: &ref, DoctrineHash: to.Hash})
		case 150:
			if err := sim.Inject(kill); err != nil {
				t.Fatal(err)
			}
			batch = append(batch, domain.Event{Kind: domain.EventFaultInjected, Vector: kill.Vector, Fault: &kill})
		}
	}
	return log
}

func mustPacks(t *testing.T) *doctrine.Registry {
	t.Helper()
	packs, err := files.ReadPacks(filepath.Join(root, "doctrine-packs"))
	if err != nil {
		t.Fatal(err)
	}
	return packs
}

func encoded(t *testing.T, log *eventlog.MemLog) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	if _, err := log.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return &b
}

// rewritten copies a recording through a fresh chain, edit changing the
// payload of any record, so a forged recording still links (I6) and only a
// replay can tell it from the original.
func rewritten(t *testing.T, recs []eventlog.Record, edit func(eventlog.Record) []byte) *eventlog.MemLog {
	t.Helper()
	out := eventlog.NewMemLog()
	for _, r := range recs {
		if _, err := out.Append(r.Kind, r.TickMs, eventlog.RawJSON(edit(r))); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func count(recs []eventlog.Record, kind eventlog.RecordKind) int {
	n := 0
	for _, r := range recs {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

// The recording replays to the same chain, record for record: invariant I7,
// through a hot swap, a kill and the lossy radio of the reference scenario.
func TestReplayReproducesTheRecording(t *testing.T) {
	log := recorded(t, 300)
	recs := log.Records()
	var swaps, kills int
	for _, r := range recs {
		switch r.Kind {
		case eventlog.RecordDecision:
			var d domain.Decision
			if err := json.Unmarshal(r.Payload, &d); err != nil {
				t.Fatal(err)
			}
			if d.Kind == domain.DecisionDoctrineSwap {
				swaps++
			}
		case eventlog.RecordEvent:
			var ev domain.Event
			if err := json.Unmarshal(r.Payload, &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Fault != nil && ev.Fault.Kind == domain.FaultKill {
				kills++
			}
		}
	}
	if swaps == 0 || kills == 0 {
		t.Fatalf("the recording holds %d swap decision(s) and %d kill(s), want both exercised", swaps, kills)
	}

	var ticks int
	res, err := Replay(encoded(t, log), Options{Packs: mustPacks(t), Verify: true, OnTick: func(ReplayedTick) { ticks++ }})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Match() || res.Divergence != nil {
		t.Fatalf("replay diverged at %+v: recorded head %s, replayed %s", res.Divergence, res.Recorded, res.Replayed)
	}
	if res.Recorded != log.Head() || res.Records != log.Seq() {
		t.Fatalf("read %d records to %s, the recording holds %d to %s", res.Records, res.Recorded, log.Seq(), log.Head())
	}
	if res.Ticks != 300 || ticks != 300 || res.Decisions != count(recs, eventlog.RecordDecision) || res.Commands != count(recs, eventlog.RecordCommand) {
		t.Fatalf("replayed %d ticks (%d visited), %d decisions, %d commands", res.Ticks, ticks, res.Decisions, res.Commands)
	}
	if res.Header.Mission != "MSN-042" || res.State.Pack == nil || res.State.Pack.Ref.Version != "2.1.0" {
		t.Fatalf("header %+v, final pack %v", res.Header, res.State.Pack)
	}
}

// Without verification the replay still rebuilds the whole chain, so its head
// can be compared by whoever reads it.
func TestReplayWithoutVerifyRebuildsTheHead(t *testing.T) {
	log := recorded(t, 120)
	res, err := Replay(encoded(t, log), Options{Packs: mustPacks(t)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Replayed != log.Head() || !res.Match() {
		t.Fatalf("replayed head %s, recorded %s", res.Replayed, log.Head())
	}
}

// A replay can stop part way, and every tick it hands over chains onto the
// last: the state and head before a tick are the state and head after the
// one before it, which is what lets a window be projected from a snapshot.
func TestReplayStopsAtItsWindowAndHandsOverChainedTicks(t *testing.T) {
	log := recorded(t, 120)
	var ticks []ReplayedTick
	res, err := Replay(encoded(t, log), Options{Packs: mustPacks(t), Verify: true, UntilMs: 5000, OnTick: func(rt ReplayedTick) {
		ticks = append(ticks, rt)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete || !res.Match() || len(ticks) != 50 || res.Ticks != 50 {
		t.Fatalf("stopped after %d ticks (%d handed over), complete %v, match %v; want 50, stopped short, matching", res.Ticks, len(ticks), res.Complete, res.Match())
	}
	for i, rt := range ticks {
		if rt.TickMs != rt.State.Clock.TickMs || len(rt.Batch) == 0 && i == 0 {
			t.Fatalf("tick %d: at %d ms, state at %d ms, batch of %d", i, rt.TickMs, rt.State.Clock.TickMs, len(rt.Batch))
		}
		if i > 0 && (rt.PrevHead != ticks[i-1].Head || rt.Prev.Clock.TickMs != ticks[i-1].State.Clock.TickMs) {
			t.Fatalf("tick %d does not chain onto tick %d", i, i-1)
		}
	}
	if last := ticks[len(ticks)-1]; last.Head != res.Replayed || last.TickMs != 5000 {
		t.Fatalf("the last tick handed over ends at %s, %d ms; the replay at %s", last.Head, last.TickMs, res.Replayed)
	}

	// Reading ahead to see whether the recording ends there links nothing:
	// the replay stands on the records of the ticks it ran.
	var through uint64
	for _, r := range log.Records() {
		if r.TickMs <= 5000 {
			through = r.Seq
		}
	}
	if res.Records != through {
		t.Fatalf("stopped at 5000 ms having linked %d records, want %d", res.Records, through)
	}

	whole, err := Replay(encoded(t, log), Options{Packs: mustPacks(t)})
	if err != nil {
		t.Fatal(err)
	}
	if !whole.Complete {
		t.Fatal("a replay read to the end of the recording is not complete")
	}

	// A window closing on the recording's last tick reads it to its end.
	last := whole.State.Clock.TickMs
	end, err := Replay(encoded(t, log), Options{Packs: mustPacks(t), Verify: true, UntilMs: last})
	if err != nil {
		t.Fatal(err)
	}
	if !end.Complete || !end.Match() || end.Records != whole.Records || end.Recorded != whole.Recorded {
		t.Fatalf("a window stopping on the last tick (%d ms): complete %v, match %v, %d records, want complete and matching the whole replay's %d", last, end.Complete, end.Match(), end.Records, whole.Records)
	}
}

// A forged decision whose chain was rebuilt around it links, and only the
// replay finds it: at its own record, not later.
func TestReplayFindsTheFirstDivergence(t *testing.T) {
	recs := recorded(t, 120).Records()
	var forged uint64
	log := rewritten(t, recs, func(r eventlog.Record) []byte {
		if forged == 0 && r.Kind == eventlog.RecordDecision && r.TickMs > 5000 {
			forged = r.Seq
			return bytes.Replace(r.Payload, []byte(`"rationale":"`), []byte(`"rationale":"forged: `), 1)
		}
		return r.Payload
	})
	if forged == 0 {
		t.Fatal("no decision after 5 s to forge")
	}
	res, err := Replay(encoded(t, log), Options{Packs: mustPacks(t), Verify: true})
	if err != nil {
		t.Fatal(err)
	}
	d := res.Divergence
	if res.Match() || d == nil || d.Seq != forged || d.Recorded == nil || d.Replayed == nil {
		t.Fatalf("divergence %+v, want one at the forged seq %d", d, forged)
	}
	if !bytes.Contains(d.Recorded.Payload, []byte("forged: ")) || bytes.Contains(d.Replayed.Payload, []byte("forged: ")) {
		t.Fatalf("recorded %s, replayed %s", d.Recorded.Payload, d.Replayed.Payload)
	}
}

// Replayed against a registry whose pack was edited under the recorded
// version, the approval is refused on its pin, and the replay leaves the
// recording at that decision, naming the pack.
func TestReplayUnderAnEditedPackDiverges(t *testing.T) {
	log := recorded(t, 20)
	var packs []*doctrine.Pack
	for _, v := range []string{"2.0.0", "2.1.0", "2.2.0"} {
		src, err := os.ReadFile(filepath.Join(root, "doctrine-packs", "recon-standard-v"+v+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if v == "2.2.0" {
			// A priority changed in place, the version left as it was.
			src = bytes.Replace(src, []byte("priority: "), []byte("priority: 1"), 1)
		}
		p, err := doctrine.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		packs = append(packs, p)
	}
	reg, err := doctrine.NewRegistry(packs...)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Replay(encoded(t, log), Options{Packs: reg, Verify: true})
	if err != nil {
		t.Fatal(err)
	}
	d := res.Divergence
	if d == nil || d.Replayed == nil || d.TickMs != engine.TickIntervalMs {
		t.Fatalf("divergence %+v, want one on the approval tick", d)
	}
	if got := string(d.Replayed.Payload); !strings.Contains(got, "refused") || !strings.Contains(got, "recon-standard@2.2.0") || !strings.Contains(got, "expected") {
		t.Fatalf("replayed %s, want the approval refused on its pin", got)
	}
}

// A recording that does not link, or that is not a mission log, is an error,
// not a divergence: nothing read from it could be trusted.
func TestReplayRefusesWhatIsNotAMissionLog(t *testing.T) {
	log := recorded(t, 20)
	lines := strings.SplitAfter(encoded(t, log).String(), "\n")
	lines = lines[:len(lines)-1] // the empty tail after the last newline

	tampered := append([]string(nil), lines...)
	last := len(tampered) - 1
	tampered[last] = strings.Replace(tampered[last], `"explored":`, `"explored":9`, 1)

	cases := []struct {
		name string
		src  string
		want error
	}{
		{"a record altered", strings.Join(tampered, ""), ErrBroken},
		{"a record dropped", strings.Join(append(append([]string(nil), lines[:5]...), lines[6:]...), ""), ErrBroken},
		{"the tail cut inside a tick", strings.Join(lines[:last], ""), ErrLayout},
		{"no header", strings.Join(rewrittenLines(t, log.Records()[1:]), ""), ErrLayout},
		{"empty", "", ErrLayout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Replay(strings.NewReader(tc.src), Options{Packs: mustPacks(t), Verify: true})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
		})
	}
}

// rewrittenLines re-chains recs from the zero head, in the on-disk format.
func rewrittenLines(t *testing.T, recs []eventlog.Record) []string {
	t.Helper()
	out := rewritten(t, recs, func(r eventlog.Record) []byte { return r.Payload })
	return strings.SplitAfter(encoded(t, out).String(), "\n")
}
