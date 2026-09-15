package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/pacer"
)

var (
	referenceScenario = filepath.Join("..", "..", "examples", "sims", "reference.yaml")
	shippedPacks      = filepath.Join("..", "..", "doctrine-packs")
)

// throughLoss is enough ticks of the reference scenario to reach 60 percent
// coverage, inject the link loss on DRONE-02 and see reassign-on-link-loss
// fire 5 s later, so a comparison covers the whole reactive path.
const throughLoss = 10000

func loadReference(t *testing.T, maxTicks int64) *Run {
	t.Helper()
	r, err := load(referenceScenario, shippedPacks, maxTicks)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return r
}

// The phase 6 definition of done: headless-fast and real time are one code
// path with identical results. The real-time run is paced by the wall clock,
// sped up so the test stays short, and still has to produce the same chain
// to the last bit as the run that never waits.
func TestFastAndRealTimeAgree(t *testing.T) {
	fast := loadReference(t, throughLoss)
	var decisions []domain.Decision
	fast.OnTick = func(tr engine.TickResult) { decisions = append(decisions, tr.Decisions...) }
	fastSum, err := fast.Execute(context.Background())
	if err != nil {
		t.Fatalf("fast run: %v", err)
	}

	rt := loadReference(t, throughLoss)
	p, err := pacer.NewRealTime(50000)
	if err != nil {
		t.Fatal(err)
	}
	rt.Pacer = p
	start := time.Now()
	rtSum, err := rt.Execute(context.Background())
	if err != nil {
		t.Fatalf("real-time run: %v", err)
	}
	// 10000 ticks of 100 ms at 50000 times real time is 20 ms of pacing:
	// the pacer ran, and the chain did not notice.
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("real-time run took %v, the pacer did not pace", elapsed)
	}
	if rtSum.Head != fastSum.Head || rtSum.Ticks != fastSum.Ticks || rtSum.Explored != fastSum.Explored {
		t.Fatalf("fast and real-time diverge:\n  fast %+v\n  real %+v", fastSum, rtSum)
	}

	if len(fastSum.Injected) != 1 || fastSum.Injected[0].Vector != "DRONE-02" || fastSum.Injected[0].Kind != domain.FaultLinkLoss {
		t.Fatalf("injected %+v, want the link loss on DRONE-02", fastSum.Injected)
	}
	var fired bool
	for _, d := range decisions {
		if d.Kind == domain.DecisionDoctrineRule && d.RuleFired == "reassign-on-link-loss" && d.Subject == "DRONE-02" {
			fired = true
		}
	}
	if !fired {
		t.Fatal("reassign-on-link-loss never fired for DRONE-02")
	}
}

// The reference scenario of spec section 14, flown to the end: DRONE-02 loses
// its link at 60 percent coverage, reassign-on-link-loss fires 5 s later, its
// lane is redistributed across the three survivors, and coverage completes.
// Nothing else reacts: no residue of a walked lane, no geofence breach on the
// system's own routes, no return to base.
func TestReferenceRunCompletes(t *testing.T) {
	r := loadReference(t, engine.DefaultConfig().MaxTicks)
	if r.Plan.Doctrine.String() != "recon-standard@2.2.0" {
		t.Fatalf("reference plan under %s", r.Plan.Doctrine)
	}
	var decisions []domain.Decision
	r.OnTick = func(tr engine.TickResult) { decisions = append(decisions, tr.Decisions...) }
	sum, err := r.Execute(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sum.State != domain.MissionComplete || sum.Explored != sum.Total {
		t.Fatalf("mission %s with %d of %d cells explored", sum.State, sum.Explored, sum.Total)
	}

	var lossMs, firedMs int64 = -1, -1
	for _, d := range decisions {
		switch d.Kind {
		case domain.DecisionOperatorNotice:
			if strings.Contains(d.Rationale, "fault link_loss injected on DRONE-02") {
				lossMs = d.TickMs
			}
			if strings.Contains(d.Rationale, "walked by") {
				t.Errorf("a lane was walked with cells left: %s", d.Rationale)
			}
		case domain.DecisionDoctrineRule:
			if d.RuleFired != "reassign-on-link-loss" || d.Subject != "DRONE-02" {
				t.Errorf("unexpected reaction at %d ms: %s", d.TickMs, d.Rationale)
				continue
			}
			firedMs = d.TickMs
		case domain.DecisionAssignment, domain.DecisionReassignment:
			if len(d.Candidates) != 6 {
				t.Errorf("%s decision on %s lists %d candidates, want the fleet of 6", d.Kind, d.Subject, len(d.Candidates))
			}
		}
	}
	if lossMs < 0 || firedMs != lossMs+5000 {
		t.Fatalf("link loss at %d ms, rule fired at %d ms, want 5 s apart", lossMs, firedMs)
	}
	t.Logf("complete after %.0f s, head %s", float64(sum.MissionMs)/1000, sum.Head)
}

// A seed and a scenario are a complete bug report: the same run twice is the
// same chain.
func TestRunIsReproducible(t *testing.T) {
	a, err := loadReference(t, 2000).Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := loadReference(t, 2000).Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.Head != b.Head || a.Head.IsZero() {
		t.Fatalf("heads %s and %s", a.Head, b.Head)
	}
}

// The command line records to a directory whose chain verifies and whose
// head is the one printed.
func TestCommandRecordsAVerifiableLog(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	code := run([]string{"--scenario", referenceScenario, "--doctrine-dir", shippedPacks, "--max-ticks", "300", "--quiet", "--out", dir}, &out, &errOut)
	if code != exitIncomplete {
		t.Fatalf("exit %d, want %d for a run cut short: %s", code, exitIncomplete, errOut.String())
	}
	records, err := eventlog.ReadDir(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	head, err := eventlog.Verify(records)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(out.String(), "head       "+head.String()) {
		t.Fatalf("printed summary does not carry the verified head %s:\n%s", head, out.String())
	}

	code = run([]string{"--scenario", referenceScenario, "--doctrine-dir", shippedPacks, "--max-ticks", "10", "--quiet", "--out", dir}, &out, &errOut)
	if code != exitError {
		t.Fatalf("a second run into a used directory exited %d, want %d", code, exitError)
	}
}

// An interrupted run did not finish: exit status 2, as for the tick ceiling,
// not 1 (spec section 15.3).
func TestInterruptedRunIsIncomplete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	err := simulate(ctx, &out, options{scenario: referenceScenario, doctrineDir: shippedPacks, maxTicks: 300, quiet: true})
	if !errors.Is(err, errIncomplete) || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err %v, want errIncomplete naming the interruption", err)
	}
}

func TestLoadRejectsAFaultOutsideTheFleet(t *testing.T) {
	dir := t.TempDir()
	src, err := os.ReadFile(referenceScenario)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(filepath.Dir(referenceScenario))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ReplaceAll(string(src), ": ../", ": "+abs+"/../")
	doc = strings.Replace(doc, "vector: DRONE-02", "vector: DRONE-99", 1)
	path := filepath.Join(dir, "s.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := load(path, shippedPacks, 10); err == nil || !strings.Contains(err.Error(), "DRONE-99") {
		t.Fatalf("err %v, want the unknown vector named", err)
	}
}
