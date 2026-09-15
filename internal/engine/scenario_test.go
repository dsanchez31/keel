package engine

import (
	"fmt"
	"math"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/eventlog"
)

// A closed-loop scenario for the phase 1 tests.
//
// The geometry here is built by hand rather than by internal/coverage on
// purpose. TestDeterminism asserts a property of the engine, and it should
// not start failing because decomposition improved. The golden-file test in
// internal/coverage is where a change in geometry is supposed to show up.

const (
	testCellM  = 50
	testRefLat = 45.0045
)

// testAO is a rectangle roughly 1000 m by 945 m at latitude 45.
func testAO() Area {
	minLat, maxLat := 45.0000, 45.0090
	minLon, maxLon := 5.0000, 5.0120

	poly := Polygon{Ring: []Position{
		{Lat: minLat, Lon: minLon},
		{Lat: minLat, Lon: maxLon},
		{Lat: maxLat, Lon: maxLon},
		{Lat: maxLat, Lon: minLon},
	}}

	mLat := domain.MetresPerDegreeLat()
	mLon := domain.MetresPerDegreeLon(testRefLat)
	rows := int(math.Ceil((maxLat - minLat) * mLat / testCellM))
	cols := int(math.Ceil((maxLon - minLon) * mLon / testCellM))

	g := Grid{
		Origin:   Position{Lat: minLat, Lon: minLon},
		RefLat:   testRefLat,
		CellM:    testCellM,
		Rows:     rows,
		Cols:     cols,
		InAO:     make([]bool, rows*cols),
		Explored: make([]bool, rows*cols),
	}
	for i := range g.InAO {
		g.InAO[i] = poly.Contains(g.Centre(CellID(i)))
	}
	return Area{Name: "test_ao", Polygon: poly, Grid: g}
}

// testPlan builds a four-lane serpentine over the AO, unassigned as a real
// approved plan is: the engine allocates the lanes at launch. Its doctrine is
// pinned against the shipped packs, as gate 2 pins it.
func testPlan(t testing.TB, area Area) ApprovedPlan {
	t.Helper()
	const lanes = 4
	g := area.Grid

	// Columns are split evenly, so no lane is wider than one pass of the
	// test caps' 120 m footprint either side of its middle column.
	out := make([]Lane, 0, lanes)
	for i := range lanes {
		col0 := i * g.Cols / lanes
		col1 := (i+1)*g.Cols/lanes - 1

		var cells []CellID
		for row := range g.Rows {
			for col := col0; col <= col1; col++ {
				id := g.CellAt(row, col)
				if g.InAO[id] {
					cells = append(cells, id)
				}
			}
		}

		// Boustrophedon: even lanes sweep north, odd lanes sweep south, so
		// consecutive lanes are traversed in opposite senses.
		var wps []Position
		midCol := (col0 + col1) / 2
		rowsOrder := make([]int, 0, g.Rows)
		for row := range g.Rows {
			rowsOrder = append(rowsOrder, row)
		}
		if i%2 == 1 {
			for a, b := 0, len(rowsOrder)-1; a < b; a, b = a+1, b-1 {
				rowsOrder[a], rowsOrder[b] = rowsOrder[b], rowsOrder[a]
			}
		}
		for _, row := range rowsOrder {
			// Every waypoint is the centre of an in-AO cell, as expansion
			// guarantees (spec section 5.5 step 7): a waypoint off the AO
			// is a geofence breach the engine rightly reacts to.
			id := g.CellAt(row, midCol)
			if !g.InAO[id] {
				continue
			}
			p := g.Centre(id)
			p.AltM = 120
			wps = append(wps, p)
		}

		l := Lane{
			ID:        LaneID(fmt.Sprintf("lane-%02d", i)),
			Index:     i,
			Cells:     cells,
			Waypoints: wps,
		}
		l.SortCells()
		out = append(out, l)
	}

	plan := ApprovedPlan{
		Mission:   "MSN-TEST",
		Intent:    "Grid-search the unexplored area to lift the fog of war",
		Area:      area,
		Doctrine:  DoctrineRef{Name: "recon-standard", Version: "2.1.0"},
		Lanes:     out,
		Requires:  []string{"aerial", "camera"},
		Policy:    PolicyNearestCapable,
		Rationale: "four parallel lanes along the long axis, one per available aerial camera vector",
		SwathM:    2 * 120,
		ScanAltM:  120,
	}
	return pin(t, plan)
}

// pin fixes the plan's doctrine hash to the shipped pack its reference
// resolves to, then content-addresses the plan. A reference no shipped pack
// answers keeps its hash, for the tests of a refused approval.
func pin(t testing.TB, plan ApprovedPlan) ApprovedPlan {
	t.Helper()
	if p, err := loadRegistry(t).Resolve(plan.Doctrine); err == nil {
		plan.DoctrineHash = p.Hash
	}
	plan.Hash = ""
	plan.Hash = PlanHash(eventlog.MustHashOf(plan).String())
	return plan
}

func testCaps(id VectorID) Capabilities {
	c := Capabilities{
		ID:            id,
		Domain:        DomainAerial,
		Tags:          []string{"camera", "aerial", "gps"},
		CruiseSpeed:   30,
		MaxRangeM:     20000,
		SensorRadiusM: 120,
	}
	c.SortTags()
	return c
}

// testFleet is one drone per lane, DRONE-01 to DRONE-04.
func testFleet(plan ApprovedPlan) []VectorID {
	ids := make([]VectorID, len(plan.Lanes))
	for i := range plan.Lanes {
		ids[i] = VectorID(fmt.Sprintf("DRONE-%02d", i+1))
	}
	return ids
}

// joinEvents announces the test fleet where the sim placed it, idle on a
// full battery, and approves the plan.
func joinEvents(plan ApprovedPlan, w *sim) []Event {
	var out []Event
	for _, id := range testFleet(plan) {
		c := testCaps(id)
		st := VectorState{ID: id, Position: w.pos[id], BatteryPct: 100, Link: LinkOK, Mode: ModeIdle}
		out = append(out, Event{Kind: EventVectorJoined, Vector: id, Caps: &c, Telemetry: &st})
	}
	return append(out, Event{Kind: EventPlanApproved, Plan: &plan})
}

// sim is a minimal closed loop: it flies each vector toward the waypoint the
// engine most recently commanded and feeds the result back as telemetry.
//
// It is not internal/world. It exists so the engine tests have a moving fleet
// without depending on the simulator.
type sim struct {
	pos    map[VectorID]Position
	target map[VectorID]Position
	speed  map[VectorID]float64
	// silent vectors neither send telemetry nor receive commands: a lost
	// link as the engine experiences it.
	silent map[VectorID]bool
	// deaf vectors send telemetry but never receive commands: a lossy
	// uplink.
	deaf map[VectorID]bool
	// acked is the last Seq each vehicle applied, reported in its frames as
	// an adapter reports it.
	acked map[VectorID]uint64
	// battery pins the battery percent a vehicle reports, in place of the
	// sim's steady drain.
	battery map[VectorID]int
}

// newSim places DRONE-0n just off the first waypoint of lane n-1, so the
// nearest_capable launch round gives each drone the lane it sits by and the
// first goto is a real transit rather than an instant arrival.
func newSim(plan ApprovedPlan) *sim {
	s := &sim{
		pos:     map[VectorID]Position{},
		target:  map[VectorID]Position{},
		speed:   map[VectorID]float64{},
		silent:  map[VectorID]bool{},
		deaf:    map[VectorID]bool{},
		acked:   map[VectorID]uint64{},
		battery: map[VectorID]int{},
	}
	for i, id := range testFleet(plan) {
		start := plan.Lanes[i].Waypoints[0]
		start.Lat -= 0.002
		s.pos[id] = start
		s.speed[id] = 30
	}
	return s
}

func (s *sim) apply(cmds []Command) {
	for _, c := range cmds {
		if s.silent[c.Vector] || s.deaf[c.Vector] || c.Seq <= s.acked[c.Vector] {
			continue
		}
		s.acked[c.Vector] = c.Seq
		if (c.Type == CommandGoto || c.Type == CommandRTB) && c.Waypoint != nil {
			s.target[c.Vector] = *c.Waypoint
		}
		if c.Type == CommandHold || c.Type == CommandAbort {
			delete(s.target, c.Vector)
		}
	}
}

// advance moves every vector one tick toward its target and returns the
// telemetry batch, ordered by vector id so the harness never depends on map
// iteration either.
func (s *sim) advance(tickMs int64) []Event {
	var out []Event
	for _, id := range SortedKeys(s.pos) {
		p := s.pos[id]
		if t, ok := s.target[id]; ok {
			p = stepToward(p, t, s.speed[id]*float64(TickIntervalMs)/1000)
			s.pos[id] = p
		}
		// Physical modes only, as an adapter reports them: moving or not.
		// Whether the movement is lane work is the engine's to derive.
		mode := ModeTransit
		if _, ok := s.target[id]; !ok {
			mode = ModeIdle
		}
		if s.silent[id] {
			continue
		}
		st := VectorState{
			ID:         id,
			Position:   p,
			Heading:    domain.BearingDeg(p, s.target[id]),
			Speed:      s.speed[id],
			BatteryPct: 100 - int(tickMs/6000),
			Link:       LinkOK,
			Mode:       mode,
			LastSeenMs: tickMs,
			AckSeq:     s.acked[id],
		}
		if pct, ok := s.battery[id]; ok {
			st.BatteryPct = pct
		}
		out = append(out, Event{Kind: EventTelemetry, Vector: id, Telemetry: &st})
	}
	return out
}

// stepToward moves from a toward b by at most stepM metres, on the local
// tangent plane. Exact arrival rather than overshoot, so a vector parked on a
// waypoint stays there instead of oscillating around it.
func stepToward(a, b Position, stepM float64) Position {
	d := domain.HaversineM(a, b)
	if d <= stepM || d == 0 {
		return Position{Lat: b.Lat, Lon: b.Lon, AltM: b.AltM}
	}
	f := stepM / d
	return Position{
		Lat:  a.Lat + (b.Lat-a.Lat)*f,
		Lon:  a.Lon + (b.Lon-a.Lon)*f,
		AltM: a.AltM + (b.AltM-a.AltM)*f,
	}
}

// scenarioLossTick is when DRONE-02 loses its link in runScenario: early
// enough that reassign-on-link-loss fires, redecomposes and reallocates
// within the run, so the head hash covers the whole phase 5 path.
const scenarioLossTick = 100

// runScenario executes a fixed seeded mission and returns the head hash of
// the recorded chain, plus the final state.
//
// Everything that happened is recorded: the events fed in, the decisions
// taken, the commands issued and a per-tick coverage summary. Comparing two
// runs is then comparing two digests.
func runScenario(t *testing.T, seed uint64, ticks int64) (eventlog.Digest, State) {
	t.Helper()

	area := testAO()
	plan := testPlan(t, area)
	world := newSim(plan)
	log := eventlog.NewMemLog()

	cfg := DefaultConfig()
	cfg.MaxTicks = ticks + 1
	s := NewState(seed, cfg, loadRegistry(t))

	if _, err := log.Append(eventlog.RecordHeader, 0, map[string]any{
		"seed":             seed,
		"tick_interval_ms": TickIntervalMs,
		"plan":             string(plan.Hash),
	}); err != nil {
		t.Fatalf("append header: %v", err)
	}

	batch := joinEvents(plan, world)
	for i := int64(0); i < ticks; i++ {
		next, cmds, decs := Step(s, batch)
		s = next
		world.apply(cmds)

		for _, ev := range batch {
			if _, err := log.Append(eventlog.RecordEvent, s.Clock.TickMs, ev); err != nil {
				t.Fatalf("append event: %v", err)
			}
		}
		for _, d := range decs {
			if _, err := log.Append(eventlog.RecordDecision, s.Clock.TickMs, d); err != nil {
				t.Fatalf("append decision: %v", err)
			}
		}
		for _, c := range cmds {
			if _, err := log.Append(eventlog.RecordCommand, s.Clock.TickMs, c); err != nil {
				t.Fatalf("append command: %v", err)
			}
		}
		explored, total := s.Grid().Coverage()
		if _, err := log.Append(eventlog.RecordTick, s.Clock.TickMs, map[string]any{
			"tick":     s.Clock.Tick,
			"explored": explored,
			"total":    total,
			"state":    string(s.Mission.State),
		}); err != nil {
			t.Fatalf("append tick: %v", err)
		}

		batch = world.advance(s.Clock.TickMs + TickIntervalMs)
		if s.Clock.Tick+1 == scenarioLossTick {
			world.silent["DRONE-02"] = true
			batch = append(batch, Event{Kind: EventFaultInjected, Fault: &Fault{Kind: FaultLinkLoss, Vector: "DRONE-02"}})
		}
	}

	return log.Head(), s
}
