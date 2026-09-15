package transport

import (
	"bytes"
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/planir"
)

// referenceIntent is the operator intent of spec section 14.
const referenceIntent = "Grid-search the unexplored area to lift the fog of war"

// launch is the reference mission's first tick: the fleet of the reference
// world joining and the reference plan, validated through the four gates,
// approved in the same batch.
type launch struct {
	prev, next engine.State
	batch      []domain.Event
	decisions  []domain.Decision
	world      *planir.World
	plan       domain.ApprovedPlan
}

func referenceLaunch(t *testing.T) launch {
	t.Helper()
	root := filepath.Join("..", "..")
	src, err := os.ReadFile(filepath.Join(root, "examples", "worlds", "reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := planir.ParseWorld(src)
	if err != nil {
		t.Fatal(err)
	}
	reg := referencePacks(t)
	raw, err := os.ReadFile(filepath.Join(root, "examples", "plans", "reference.json"))
	if err != nil {
		t.Fatal(err)
	}
	res := planir.Validate(planir.Input{Intent: referenceIntent, Raw: raw, World: w, Doctrines: reg, Arrival: engine.DefaultConfig().Arrival()})
	if !res.OK() {
		t.Fatalf("reference plan refused: %+v", res.Diagnostics)
	}
	plan := res.Plan.Clone()
	plan.Mission = "MSN-042"

	var batch []domain.Event
	for _, v := range w.Fleet {
		caps, st := v.Caps, v.State
		batch = append(batch, domain.Event{Kind: domain.EventVectorJoined, Vector: caps.ID, Caps: &caps, Telemetry: &st})
	}
	batch = append(batch, domain.Event{Kind: domain.EventPlanApproved, Plan: &plan})

	prev := engine.NewState(42, engine.DefaultConfig(), reg)
	next, _, decs := engine.Step(prev, batch)
	if next.Mission.State != domain.MissionRunning {
		t.Fatalf("mission %s after approval, want running", next.Mission.State)
	}
	return launch{prev: prev, next: next, batch: batch, decisions: decs, world: w, plan: plan}
}

// referencePacks is the registry of the shipped doctrine packs.
func referencePacks(t *testing.T) *doctrine.Registry {
	t.Helper()
	root := filepath.Join("..", "..")
	paths, err := filepath.Glob(filepath.Join(root, "doctrine-packs", "*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no doctrine pack: %v", err)
	}
	var packs []*doctrine.Pack
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		pack, err := doctrine.Parse(b)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		packs = append(packs, pack)
	}
	reg, err := doctrine.NewRegistry(packs...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

var testHead = eventlog.MustHashOf("head")

func TestNewMissionView(t *testing.T) {
	l := referenceLaunch(t)
	v := NewMissionView(l.next, testHead)

	if v.Tick != 1 || v.TickMs != engine.TickIntervalMs || v.Head != testHead.String() {
		t.Fatalf("clock and head: tick %d at %d ms, head %s", v.Tick, v.TickMs, v.Head)
	}
	if v.Mission.ID != "MSN-042" || v.Mission.Plan != l.plan.Hash || v.Mission.State != domain.MissionRunning {
		t.Fatalf("mission %+v", v.Mission)
	}
	if v.DoctrineHash != l.next.Pack.Hash {
		t.Fatalf("doctrine hash %q, want %q", v.DoctrineHash, l.next.Pack.Hash)
	}
	if len(v.Lanes) != len(l.plan.Lanes) {
		t.Fatalf("%d lanes, want %d", len(v.Lanes), len(l.plan.Lanes))
	}
	for i, lane := range v.Lanes {
		if lane.ID != l.next.Lanes[i].ID || lane.AssignedTo == "" || lane.Cursor != l.next.Cursor[lane.ID] {
			t.Fatalf("lane %d: %s assigned to %q at cursor %d", i, lane.ID, lane.AssignedTo, lane.Cursor)
		}
	}
	if len(v.Vectors) != len(l.world.Fleet) {
		t.Fatalf("%d vectors, want %d", len(v.Vectors), len(l.world.Fleet))
	}
	if !slices.IsSortedFunc(v.Vectors, func(a, b VectorView) int { return cmp.Compare(a.State.ID, b.State.ID) }) {
		t.Fatal("vectors not in id order")
	}
	for _, vv := range v.Vectors {
		if vv.Caps.ID != vv.State.ID {
			t.Fatalf("vector %s carries the capabilities of %s", vv.State.ID, vv.Caps.ID)
		}
	}
	if len(v.Stations) != 1 || v.Stations[0].Name != "gcs-west" {
		t.Fatalf("stations %+v, want gcs-west", v.Stations)
	}
	explored, total := l.next.Grid().Coverage()
	if v.Explored != explored || v.Total != total || total == 0 {
		t.Fatalf("coverage %d/%d, want %d/%d", v.Explored, v.Total, explored, total)
	}
	if _, err := eventlog.Canonical(v); err != nil {
		t.Fatalf("the view does not encode: %v", err)
	}
}

func TestNewWorldView(t *testing.T) {
	v := NewWorldView(referenceLaunch(t).world)
	if v.Name != "reference" || len(v.Areas) != 1 || len(v.Stations) != 1 || v.Stations[0].Name != "gcs-west" {
		t.Fatalf("world %q: %d areas, stations %+v", v.Name, len(v.Areas), v.Stations)
	}
	a := v.Areas[0]
	if a.Name != "fog_of_war_east" || len(a.Polygon.Ring) != 6 || a.CellM != 50 || a.ScanAltM != 120 {
		t.Fatalf("area %+v", a)
	}
	// The world file states about 14.3 km²; the raster counts whole cells.
	if a.AreaKm2 < 13.8 || a.AreaKm2 > 14.8 {
		t.Fatalf("area %.2f km², want about 14.3", a.AreaKm2)
	}
	b, err := eventlog.Canonical(v)
	if err != nil {
		t.Fatalf("the view does not encode: %v", err)
	}
	if bytes.Contains(b, []byte(`"in_ao"`)) || bytes.Contains(b, []byte(`"explored"`)) {
		t.Fatalf("the world view carries a raster: %s", b)
	}
}

func TestNewMissionViewBeforeAnyMission(t *testing.T) {
	v := NewMissionView(engine.NewState(1, engine.DefaultConfig(), nil), eventlog.ZeroDigest)
	if v.Mission.State != domain.MissionPlanning || v.DoctrineHash != "" || len(v.Lanes) != 0 || len(v.Vectors) != 0 {
		t.Fatalf("empty state view %+v", v)
	}
	b, err := eventlog.Canonical(v)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("null")) {
		t.Fatalf("an empty view encodes a null: %s", b)
	}
}

// rank is the position of a frame type in the order TickMessages emits.
var rank = map[FrameType]int{
	FrameEvent: 0, FrameDecision: 1, FrameDoctrine: 2, FrameMission: 3,
	FrameTelemetry: 4, FrameCoverage: 5, FrameTick: 6,
}

func typesOf(msgs []Message) []FrameType {
	out := make([]FrameType, len(msgs))
	for i, m := range msgs {
		out[i] = m.Type
	}
	return out
}

func count(types []FrameType, t FrameType) int {
	n := 0
	for _, x := range types {
		if x == t {
			n++
		}
	}
	return n
}

func checkOrder(t *testing.T, msgs []Message) {
	t.Helper()
	types := typesOf(msgs)
	if !slices.IsSortedFunc(types, func(a, b FrameType) int { return rank[a] - rank[b] }) {
		t.Fatalf("frames out of order: %v", types)
	}
	if len(types) == 0 || types[len(types)-1] != FrameTick || count(types, FrameTick) != 1 {
		t.Fatalf("a tick closes with exactly one tick frame: %v", types)
	}
	for _, m := range msgs {
		if _, err := eventlog.Canonical(m.Data); err != nil {
			t.Fatalf("%s frame does not encode: %v", m.Type, err)
		}
	}
}

func TestTickMessagesOnApproval(t *testing.T) {
	l := referenceLaunch(t)
	msgs := TickMessages(l.prev, l.next, l.batch, l.decisions, testHead)
	checkOrder(t, msgs)
	types := typesOf(msgs)

	if got, want := count(types, FrameEvent), len(l.batch); got != want {
		t.Fatalf("%d event frames, want %d (every join and the approval)", got, want)
	}
	if got, want := count(types, FrameDecision), len(l.decisions); got != want || want == 0 {
		t.Fatalf("%d decision frames, want %d", got, want)
	}
	for _, typ := range []FrameType{FrameDoctrine, FrameMission, FrameTelemetry} {
		if count(types, typ) != 1 {
			t.Fatalf("the launch tick carries one %s frame: %v", typ, types)
		}
	}
	if count(types, FrameCoverage) != 0 {
		t.Fatalf("a mission frame carries the new raster, no coverage delta: %v", types)
	}
	for _, m := range msgs {
		switch d := m.Data.(type) {
		case DoctrineInfo:
			if d.Ref != l.plan.Doctrine.String() || d.Hash != l.next.Pack.Hash {
				t.Fatalf("doctrine frame %s %s", d.Ref, d.Hash)
			}
		case []domain.VectorState:
			if len(d) != len(l.world.Fleet) {
				t.Fatalf("telemetry frame holds %d vectors, want %d", len(d), len(l.world.Fleet))
			}
		case TickSummary:
			if d.Tick != 1 || d.Head != testHead.String() || d.State != domain.MissionRunning || len(d.Cursors) != len(l.plan.Lanes) {
				t.Fatalf("tick frame %+v", d)
			}
		}
	}
}

func TestTickMessagesSteadyTick(t *testing.T) {
	l := referenceLaunch(t)
	lane := l.next.Lanes[0]
	frame := l.next.Vectors[lane.AssignedTo]
	frame.Position = lane.Waypoints[0]
	frame.Mode = domain.ModeTransit
	frame.LastSeenMs = l.next.Clock.TickMs + engine.TickIntervalMs
	batch := []domain.Event{{Kind: domain.EventTelemetry, Vector: frame.ID, Telemetry: &frame}}
	next, _, decs := engine.Step(l.next, batch)

	msgs := TickMessages(l.next, next, batch, decs, testHead)
	checkOrder(t, msgs)
	types := typesOf(msgs)
	if count(types, FrameEvent) != 0 {
		t.Fatalf("telemetry events travel in the telemetry frame, not as events: %v", types)
	}
	if count(types, FrameDoctrine) != 0 || count(types, FrameMission) != 0 {
		t.Fatalf("a tick that reshapes nothing carries no doctrine or mission frame: %v", types)
	}
	var delta *CoverageDelta
	for _, m := range msgs {
		if d, ok := m.Data.(CoverageDelta); ok {
			delta = &d
		}
	}
	if delta == nil || len(delta.Cells) == 0 {
		t.Fatalf("a vector on its lane explores cells, no coverage frame: %v", types)
	}
	if !slices.IsSorted(delta.Cells) {
		t.Fatal("coverage delta not ascending")
	}
	for _, c := range delta.Cells {
		if l.next.Grid().IsExplored(c) || !next.Grid().IsExplored(c) {
			t.Fatalf("cell %d is not newly explored", c)
		}
	}
	explored, _ := next.Grid().Coverage()
	before, _ := l.next.Grid().Coverage()
	if explored-before != len(delta.Cells) {
		t.Fatalf("delta of %d cells, coverage grew by %d", len(delta.Cells), explored-before)
	}
}

func TestTickMessagesReshapeOnRelease(t *testing.T) {
	l := referenceLaunch(t)
	victim := l.next.Lanes[0].AssignedTo
	batch := []domain.Event{{Kind: domain.EventFaultInjected, Vector: victim, Fault: &domain.Fault{Kind: domain.FaultKill, Vector: victim}}}
	next, _, decs := engine.Step(l.next, batch)

	msgs := TickMessages(l.next, next, batch, decs, testHead)
	checkOrder(t, msgs)
	if count(typesOf(msgs), FrameMission) != 1 {
		t.Fatalf("a kill releases lanes, which reshapes the mission: %v", typesOf(msgs))
	}
}

func TestNewlyExploredAcrossRasters(t *testing.T) {
	a := domain.Grid{Explored: []bool{false, true}}
	b := domain.Grid{Explored: []bool{true, true, true}}
	if cells := newlyExplored(a, b); cells != nil {
		t.Fatalf("two rasters have no delta, got %v", cells)
	}
	c := domain.Grid{Explored: []bool{true, true}}
	if cells := newlyExplored(a, c); !slices.Equal(cells, []domain.CellID{0}) {
		t.Fatalf("delta %v, want [0]", cells)
	}
}

// A tick left out of the sample keeps every frame but its telemetry and tick
// frames, which come back apart in their order.
func TestThinTick(t *testing.T) {
	msgs := []Message{
		{Type: FrameEvent}, {Type: FrameDecision}, {Type: FrameDoctrine}, {Type: FrameMission},
		{Type: FrameTelemetry}, {Type: FrameCoverage}, {Type: FrameTick},
	}
	types := func(ms []Message) []FrameType {
		out := make([]FrameType, len(ms))
		for i, m := range ms {
			out[i] = m.Type
		}
		return out
	}
	kept, thinned := ThinTick(msgs, true)
	if len(kept) != len(msgs) || thinned != nil {
		t.Fatalf("sampled: kept %v, thinned %v", types(kept), types(thinned))
	}
	kept, thinned = ThinTick(msgs, false)
	if want := []FrameType{FrameEvent, FrameDecision, FrameDoctrine, FrameMission, FrameCoverage}; !slices.Equal(types(kept), want) {
		t.Fatalf("kept %v, want %v", types(kept), want)
	}
	if want := []FrameType{FrameTelemetry, FrameTick}; !slices.Equal(types(thinned), want) {
		t.Fatalf("thinned %v, want %v", types(thinned), want)
	}
	for tick, want := range map[int64]bool{10: true, 20: true, 11: false} {
		if got := Sampled(tick, 10); got != want {
			t.Errorf("Sampled(%d, 10) = %v", tick, got)
		}
	}
	if !Sampled(7, 1) || !Sampled(7, 0) {
		t.Error("a stream thinned by one tick or none thinned a tick")
	}
}

func TestStampSpeed(t *testing.T) {
	msgs := []Message{{Type: FrameMission, Data: MissionView{Tick: 1}}, {Type: FrameEvent, Data: domain.Event{}}, {Type: FrameTick, Data: TickSummary{Tick: 1}}}
	StampSpeed(msgs, 20)
	if v := msgs[0].Data.(MissionView); v.Speed != 20 || v.Tick != 1 {
		t.Errorf("mission frame %+v", v)
	}
	if s := msgs[2].Data.(TickSummary); s.Speed != 20 || s.Tick != 1 {
		t.Errorf("tick frame %+v", s)
	}
}
