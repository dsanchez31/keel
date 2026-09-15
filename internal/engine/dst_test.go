package engine_test

// The deterministic simulation test harness (design section 4.3).
//
// Each seed runs the reference mission through the closed loop keelsim runs
// (engine, mission log, internal/world) under a fault schedule drawn from the
// seed: faults injected into the world, frames dropped, duplicated, delayed
// and reordered on the wire between the world and the engine, and a pinned
// hot swap. Invariants I1 to I9 (spec section 11) are asserted on the
// engine's state at every tick boundary, and the recorded log is replayed at
// the end. The engine is pure and the world is seeded, so a failing seed is a
// complete bug report: the harness prints the command that reproduces it.
//
// The package is engine_test, not engine: the replay lives in
// internal/missionlog, which imports the engine.

import (
	"bytes"
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/assign"
	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/files"
	"github.com/dsanchez31/keel/internal/missionlog"
	"github.com/dsanchez31/keel/internal/planir"
	"github.com/dsanchez31/keel/internal/world"
)

var (
	dstSeed  = flag.String("dst.seed", "", "run this one seed of the DST sweep, as a failure's reproduction command gives it")
	dstSeeds = flag.Int("dst.seeds", 8, "how many seeds the DST sweep runs, from -dst.from (at most 2 under -short)")
	dstFrom  = flag.Uint64("dst.from", 1, "the first seed of the DST sweep")
)

const (
	repoRoot = "../.."
	// scheduleSalt separates the harness's stream from the engine's and the
	// world's, which the seed itself seeds.
	scheduleSalt = 0x6473745f73636865
	// faultWindowTicks bounds when a scheduled fault or swap lands: inside
	// the first 13 minutes of mission time, while the lanes are being flown.
	faultWindowTicks = 8000
	// maxKills leaves the reference fleet two of its four camera drones at
	// worst, so most seeds complete and the others fail on the stall.
	maxKills = 2
)

func TestDST(t *testing.T) {
	fx := loadFixture(t)
	for _, seed := range sweep(t) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, fx, seed)
			if err := h.run(); err != nil {
				t.Fatalf("%v\nfault schedule:\n%s\nreproduce: go test ./internal/engine -run 'TestDST$' -count=1 -dst.seed=%d", err, h.schedule, seed)
			}
			t.Logf("mission %s at %.1f s, %d of %d cells explored, %d scheduled", h.final.Mission.State, h.final.Clock.Seconds(), explored(h.final), total(h.final), len(h.schedule))
		})
	}
}

// sweep is the seeds to run: the one -dst.seed names, or -dst.seeds of them
// from -dst.from.
func sweep(t *testing.T) []uint64 {
	t.Helper()
	if *dstSeed != "" {
		s, err := strconv.ParseUint(*dstSeed, 10, 64)
		if err != nil {
			t.Fatalf("-dst.seed %q: %v", *dstSeed, err)
		}
		return []uint64{s}
	}
	n := *dstSeeds
	if testing.Short() {
		n = min(n, 2)
	}
	out := make([]uint64, n)
	for i := range out {
		out[i] = *dstFrom + uint64(i)
	}
	return out
}

// fixture is what every seed shares, read once and never written.
type fixture struct {
	world   *planir.World
	physics world.Physics
	packs   *doctrine.Registry
	plan    domain.ApprovedPlan
	// swaps are the packs a scheduled hot swap may target: those that react
	// to every constraint they declare, which I4 asserts.
	swaps []*doctrine.Pack
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	w, err := files.ReadWorld(repoRoot + "/examples/worlds/reference.yaml")
	if err != nil {
		t.Fatal(err)
	}
	packs, err := files.ReadPacks(repoRoot + "/doctrine-packs")
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(repoRoot + "/examples/sims/reference.yaml")
	if err != nil {
		t.Fatal(err)
	}
	sc, err := world.ParseScenario(src)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(repoRoot + "/examples/plans/reference.json")
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
	fx := fixture{world: w, physics: sc.Physics, packs: packs, plan: res.Plan.Clone()}
	fx.plan.Mission = "MSN-DST"

	for _, ref := range packs.Refs() {
		p, err := packs.Resolve(ref)
		if err != nil {
			t.Fatal(err)
		}
		if reactive(p) {
			fx.swaps = append(fx.swaps, p)
		}
		if ref == fx.plan.Doctrine && !reactive(p) {
			t.Fatalf("%s, the reference plan's pack, does not send home a vector violating each of its constraints: I4 cannot hold under it", ref)
		}
	}
	return fx
}

// reactive reports whether every constraint of p has a rule that fires on its
// violation without a window and sends the vector home. I4 asserts the
// reaction, so it holds only under such packs; recon-standard@2.0.0 declares
// a geofence and no rule reacting to it.
func reactive(p *doctrine.Pack) bool {
	for _, c := range p.Constraints {
		if !slices.ContainsFunc(p.Rules, func(r doctrine.Rule) bool {
			return r.When.WindowMs == 0 &&
				strings.Contains(r.When.Expr.String(), "violates("+c.ID+")") &&
				slices.ContainsFunc(r.Then, func(a doctrine.Action) bool { return a.Kind == doctrine.ActionReturnToBase })
		}) {
			return false
		}
	}
	return true
}

// scheduled is one entry of a seed's schedule: a fault to inject into the
// world, or a hot swap to request.
type scheduled struct {
	tick  int64
	fault *domain.Fault
	swap  *doctrine.Pack
}

type schedule []scheduled

func (s schedule) String() string {
	if len(s) == 0 {
		return "  none"
	}
	var b strings.Builder
	for _, e := range s {
		switch {
		case e.fault != nil:
			fmt.Fprintf(&b, "  tick %5d  %-15s %-9s magnitude %g, duration %d ms\n", e.tick, e.fault.Kind, e.fault.Vector, e.fault.Magnitude, e.fault.DurationMs)
		case e.swap != nil:
			fmt.Fprintf(&b, "  tick %5d  hot swap to %s\n", e.tick, engine.PackLabel(e.swap.Ref, e.swap.Hash))
		}
	}
	b.WriteString("  plus frames dropped, duplicated and delayed at 0.5 percent each, and a batch reordered one tick in 50")
	return b.String()
}

// harness is one seed's run.
type harness struct {
	fx       fixture
	seed     uint64
	r        engine.Rand
	schedule schedule
	held     []heldFrame
	// suspects are the lanes pending past the deadline that the allocator
	// gave a vector at the previous tick boundary (I3).
	suspects map[domain.LaneID]bool
	final    engine.State
}

type heldFrame struct {
	due int64
	ev  domain.Event
}

func newHarness(t *testing.T, fx fixture, seed uint64) *harness {
	t.Helper()
	h := &harness{fx: fx, seed: seed, r: engine.NewRand(seed ^ scheduleSalt)}
	h.schedule = h.draw()
	return h
}

// draw is the seed's schedule, sorted by tick: one to four world faults, at
// most maxKills kills, and a hot swap on half the seeds.
func (h *harness) draw() schedule {
	var ids []domain.VectorID
	for _, v := range h.fx.world.Fleet {
		ids = append(ids, v.Caps.ID)
	}
	slices.Sort(ids)
	kinds := domain.FaultKinds()
	tick := func() int64 { return 20 + int64(h.r.IntN(faultWindowTicks)) }
	ticks := func(lo, hi int) int64 { return engine.TicksToMs(int64(lo + h.r.IntN(hi-lo))) }

	var out schedule
	kills := 0
	for range 1 + h.r.IntN(4) {
		f := domain.Fault{Kind: kinds[h.r.IntN(len(kinds))], Vector: ids[h.r.IntN(len(ids))]}
		if f.Kind == domain.FaultKill && kills == maxKills {
			f.Kind = domain.FaultLinkLoss
		}
		switch f.Kind {
		case domain.FaultKill:
			kills++
		case domain.FaultLinkLoss, domain.FaultStaleTelemetry:
			// A quarter of them for the rest of the run.
			if h.r.IntN(4) > 0 {
				f.DurationMs = ticks(30, 300)
			}
		case domain.FaultBatteryDrain:
			f.Magnitude = float64(20 + h.r.IntN(50))
		case domain.FaultGPSDrift:
			f.Magnitude = float64(1 + h.r.IntN(10))
			f.DurationMs = ticks(50, 300)
		}
		out = append(out, scheduled{tick: tick(), fault: &f})
	}
	if len(h.fx.swaps) > 0 && h.r.IntN(2) == 0 {
		out = append(out, scheduled{tick: tick(), swap: h.fx.swaps[h.r.IntN(len(h.fx.swaps))]})
	}
	slices.SortStableFunc(out, func(a, b scheduled) int { return cmp.Compare(a.tick, b.tick) })
	return out
}

// carry is the wire between the world and the engine: every telemetry frame
// is dropped, duplicated or held back for up to 3 s at 0.5 percent each, a
// batch is reordered one tick in 50, and frames held back arrive first on
// their tick. Operator events are never touched.
func (h *harness) carry(tick int64, batch []domain.Event) []domain.Event {
	var out []domain.Event
	kept := h.held[:0]
	for _, f := range h.held {
		if f.due <= tick {
			out = append(out, f.ev)
		} else {
			kept = append(kept, f)
		}
	}
	h.held = kept
	for _, ev := range batch {
		if ev.Kind != domain.EventTelemetry {
			out = append(out, ev)
			continue
		}
		switch p := h.r.IntN(1000); {
		case p < 5:
		case p < 10:
			out = append(out, ev, ev)
		case p < 15:
			h.held = append(h.held, heldFrame{due: tick + 1 + int64(h.r.IntN(30)), ev: ev})
		default:
			out = append(out, ev)
		}
	}
	if h.r.IntN(50) == 0 {
		for i := len(out) - 1; i > 0; i-- {
			j := h.r.IntN(i + 1)
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// run flies the mission as keelsim does, asserting at every tick boundary,
// then replays its log. It returns the first violation.
func (h *harness) run() error {
	fx := h.fx
	var fleet []world.Vehicle
	for _, v := range fx.world.Fleet {
		fleet = append(fleet, world.Vehicle{Caps: v.Caps, State: v.State})
	}
	var extent []domain.Position
	for _, a := range fx.world.Areas {
		extent = append(extent, a.Area.Polygon.Ring...)
	}
	sim, err := world.New(world.Config{Seed: h.seed, Physics: fx.physics, Stations: fx.world.Stations, Fleet: fleet, Extent: extent})
	if err != nil {
		return err
	}

	cfg := engine.DefaultConfig()
	plan := fx.plan.Clone()
	log := eventlog.NewMemLog()
	if err := missionlog.AppendHeader(log, missionlog.Header{Name: "dst", Seed: h.seed, Config: cfg, Plan: plan.Hash, Mission: plan.Mission}); err != nil {
		return err
	}

	s := engine.NewState(h.seed, cfg, fx.packs)
	pending := slices.Clone(h.schedule)
	batch := append(sim.Join(), domain.Event{Kind: domain.EventPlanApproved, Plan: &plan})
	for tick := int64(1); tick <= cfg.MaxTicks; tick++ {
		next, cmds, decs := engine.Step(s, batch)
		if err := missionlog.AppendTick(log, next, batch, decs, cmds); err != nil {
			return err
		}
		if err := h.check(s, next, decs); err != nil {
			return fmt.Errorf("tick %d (%.1f s): %w", next.Clock.Tick, next.Clock.Seconds(), err)
		}
		s = next
		if s.Mission.State.Terminal() {
			break
		}

		sim.Send(cmds)
		var ops []domain.Event
		for len(pending) > 0 && pending[0].tick <= tick {
			e := pending[0]
			pending = pending[1:]
			switch {
			case e.fault != nil:
				if err := sim.Inject(*e.fault); err != nil {
					return fmt.Errorf("injecting %s on %s: %w", e.fault.Kind, e.fault.Vector, err)
				}
				f := *e.fault
				ops = append(ops, domain.Event{Kind: domain.EventFaultInjected, Vector: f.Vector, Fault: &f})
			case e.swap != nil:
				ref := e.swap.Ref
				ops = append(ops, domain.Event{Kind: domain.EventDoctrineSwap, Doctrine: &ref, DoctrineHash: e.swap.Hash})
			}
		}
		batch = append(h.carry(tick+1, sim.Step()), ops...)
	}
	h.final = s

	// I8: coverage or the stall ends the mission, not the tick ceiling.
	if !s.Mission.State.Terminal() || s.Clock.Tick >= cfg.MaxTicks {
		return fmt.Errorf("I8: mission %s at tick %d: the tick ceiling of %d ended it, not coverage or the stall", s.Mission.State, s.Clock.Tick, cfg.MaxTicks)
	}

	// I6 and I7: the log links, and replays to the same chain.
	var b bytes.Buffer
	if _, err := log.WriteTo(&b); err != nil {
		return err
	}
	res, err := missionlog.Replay(&b, missionlog.Options{Packs: fx.packs, Verify: true})
	if err != nil {
		return fmt.Errorf("I6: %w", err)
	}
	if !res.Match() {
		if d := res.Divergence; d != nil {
			return fmt.Errorf("I7: the replay leaves the recording at record %d (%d ms): recorded %s, replayed %s", d.Seq, d.TickMs, payload(d.Recorded), payload(d.Replayed))
		}
		return fmt.Errorf("I7: recorded head %s, replayed %s", res.Recorded, res.Replayed)
	}
	return nil
}

// check asserts the invariants of spec section 11 on one tick boundary, the
// state before the tick and after it. I6, I7 and I8 are asserted on the whole
// run.
func (h *harness) check(prev, cur engine.State, decs []domain.Decision) error {
	// I5: mission time advances by exactly one tick.
	if !prev.Clock.AdvancedBy(cur.Clock) {
		return fmt.Errorf("I5: the clock went from %+v to %+v", prev.Clock, cur.Clock)
	}

	// I1: no cell held by two assigned lanes.
	owner := map[domain.CellID]domain.LaneID{}
	for _, l := range cur.Lanes {
		if !l.Assigned() {
			continue
		}
		for _, c := range l.Cells {
			if o, dup := owner[c]; dup {
				return fmt.Errorf("I1: cell %d held by %s (%s) and %s (%s)", c, o, laneOwner(cur, o), l.ID, l.AssignedTo)
			}
			owner[c] = l.ID
		}
	}

	// I2: an explored cell stays explored. The grid is the mission's, so it
	// is compared only once the mission exists on both sides.
	if before, after := prev.Grid().Explored, cur.Grid().Explored; len(before) == len(after) {
		for i := range before {
			if before[i] && !after[i] {
				return fmt.Errorf("I2: cell %d unexplored again", i)
			}
		}
	}

	// I9: a hot swap changes neither the mission nor its state. The tick's
	// own settlement may still end it, and says so in a decision naming it.
	if slices.ContainsFunc(decs, func(d domain.Decision) bool { return d.Kind == domain.DecisionDoctrineSwap }) {
		settled := slices.ContainsFunc(decs, func(d domain.Decision) bool {
			return d.Kind == domain.DecisionMissionState && d.Subject == string(cur.Mission.ID)
		})
		if cur.Mission.ID != prev.Mission.ID || cur.Mission.Plan != prev.Mission.Plan || (cur.Mission.State != prev.Mission.State && !settled) {
			return fmt.Errorf("I9: the swap took mission %s %s into %s %s", prev.Mission.ID, prev.Mission.State, cur.Mission.ID, cur.Mission.State)
		}
	}

	if cur.Mission.State != domain.MissionRunning || cur.Pack == nil {
		h.suspects = nil
		return nil
	}

	// I4: a vector outside its envelope is on its way home within the tick,
	// on the constraints of the active pack evaluated as the engine does.
	vs, err := doctrine.CheckConstraints(cur.Pack, cur.DoctrineEnv())
	if err != nil {
		return fmt.Errorf("I4: %w", err)
	}
	for _, v := range vs {
		if is := cur.Issued[v.Agent]; is.Command.Type != domain.CommandRTB {
			return fmt.Errorf("I4: %s violates %s of %s and stands on %q, not rtb", v.Agent, v.Constraint, cur.Pack.Ref, is.Command.Type)
		}
	}

	return h.checkI3(cur)
}

// checkI3 asks the allocator the engine's own question over the lanes pending
// at the tick boundary. A lane pending past the deadline that it gives a
// vector at two boundaries in a row is one the engine had a round to assign
// and did not. One boundary is not enough: the lane stage moves the fleet
// after the engine's round, so the question at the boundary is a tick newer
// than the one the engine answered.
func (h *harness) checkI3(cur engine.State) error {
	next := map[domain.LaneID]bool{}
	defer func() { h.suspects = next }()
	pending := cur.PendingLanes()
	if len(pending) == 0 {
		return nil
	}
	out, err := assign.Allocate(cur.AllocationRequest(pending, cur.Plan.Policy))
	if err != nil {
		return fmt.Errorf("I3: the allocator refused the engine's question: %w", err)
	}
	deadlineMs := engine.TicksToMs(cur.Config.RedecomposeDeadlineTicks)
	for _, a := range out {
		since, ok := cur.Unassigned[a.Lane]
		if a.Winner == "" || (ok && cur.Clock.TickMs-since <= deadlineMs) {
			continue
		}
		if h.suspects[a.Lane] {
			return fmt.Errorf("I3: %s unassigned since %d ms, past the %d ms deadline, while the allocator gives it %s: %s", a.Lane, since, deadlineMs, a.Winner, a.Rationale)
		}
		next[a.Lane] = true
	}
	return nil
}

func laneOwner(s engine.State, id domain.LaneID) domain.VectorID {
	l, _ := s.LaneByID(id)
	return l.AssignedTo
}

func explored(s engine.State) int { n, _ := s.Grid().Coverage(); return n }

func total(s engine.State) int { _, n := s.Grid().Coverage(); return n }

func payload(r *eventlog.Record) string {
	if r == nil {
		return "nothing"
	}
	const maxBytes = 512
	if len(r.Payload) > maxBytes {
		return fmt.Sprintf("%s %s... (%d bytes)", r.Kind, r.Payload[:maxBytes], len(r.Payload))
	}
	return fmt.Sprintf("%s %s", r.Kind, r.Payload)
}
