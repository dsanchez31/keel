package doctrine

import (
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/dsanchez31/keel/internal/coverage"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/eventlog"
)

// Positions in the tests are metre offsets east and north of a fixed origin.
func pos(eastM, northM float64) domain.Position {
	return domain.Position{
		Lat: 45 + northM/domain.MetresPerDegreeLat(),
		Lon: 5 + eastM/domain.MetresPerDegreeLon(45),
	}
}

// testArea is a 1000 m wide, heightM tall rectangle rastered at 100 m.
func testArea(t *testing.T, heightM float64) domain.Area {
	t.Helper()
	poly := domain.Polygon{Ring: []domain.Position{pos(0, 0), pos(1000, 0), pos(1000, heightM), pos(0, heightM)}}
	a, err := coverage.NewGrid("test-ao", poly, 100)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func testAgent(id string) Agent {
	return Agent{
		ID: domain.VectorID(id), Domain: domain.DomainAerial, BatteryPct: 100,
		RTBCostPct: 10, RTBCostKnown: true, Link: domain.LinkOK, Mode: domain.ModeScanning,
		Position: pos(500, 500), Speed: 12,
	}
}

func testEnv(t *testing.T, tickMs int64, agents ...Agent) Env {
	t.Helper()
	return Env{
		TickMs:  tickMs,
		Mission: MissionBinding{Area: testArea(t, 1000), CoveragePct: 40, State: domain.MissionRunning},
		Agents:  agents,
	}
}

// packWithRules is a minimal pack around a rules block.
func packWithRules(t *testing.T, rules string) *Pack {
	t.Helper()
	return mustParse(t, `apiVersion: keel.doctrine/v1
name: rules-test
version: 1.0.0
params: {battery_reserve_pct: 15}
constraints:
  - id: battery-reserve
    rule: agent.battery_pct >= agent.rtb_cost_pct + doctrine.battery_reserve_pct
rules:
`+rules)
}

func mustEvaluate(t *testing.T, p *Pack, env Env, windows []Window) Result {
	t.Helper()
	res, err := Evaluate(p, env, windows)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestBindAgent(t *testing.T) {
	caps := domain.Capabilities{ID: "DRONE-01", Domain: domain.DomainAerial, MaxRangeM: 10_000}
	st := domain.VectorState{ID: "DRONE-01", Position: pos(0, 0), BatteryPct: 80, Link: domain.LinkOK, Mode: domain.ModeScanning, Speed: 15}
	stations := []domain.Station{{Name: "far", Position: pos(0, 5000)}, {Name: "near", Position: pos(0, 1450)}}
	wp := []domain.Position{pos(0, 0), pos(0, 100), pos(0, 200), pos(0, 300)}
	lanes := []domain.Lane{
		{ID: "lane-03", Index: 3, AssignedTo: "DRONE-01", Waypoints: wp},
		{ID: "lane-01", Index: 1, AssignedTo: "DRONE-01", Waypoints: wp},
		{ID: "lane-00", Index: 0, AssignedTo: "DRONE-02", Waypoints: wp},
	}

	a := BindAgent(caps, st, stations, lanes, map[domain.LaneID]int{"lane-01": 2})
	if !a.RTBCostKnown || a.RTBCostPct != 15 {
		t.Errorf("rtb cost = %d (known %v), want 15: 1450 m of 10 km, rounded up", a.RTBCostPct, a.RTBCostKnown)
	}
	if a.Lane == nil || a.Lane.ID != "lane-01" || a.Lane.ProgressPct != 50 {
		t.Errorf("lane = %+v, want lane-01 at 50 %%", a.Lane)
	}
	if a.BatteryPct != 80 || a.Speed != 15 || a.Domain != domain.DomainAerial {
		t.Errorf("agent = %+v", a)
	}

	if a := BindAgent(caps, st, stations, lanes, nil); a.Lane == nil || a.Lane.ProgressPct != 0 {
		t.Errorf("nil cursor: lane = %+v, want progress 0", a.Lane)
	}
	if a := BindAgent(caps, st, nil, nil, nil); !a.RTBCostKnown || a.RTBCostPct != 0 || a.Lane != nil {
		t.Errorf("no station, no lane: %+v", a)
	}
	lost := st
	lost.Position = domain.Position{Lat: math.NaN()}
	if a := BindAgent(caps, lost, stations, nil, nil); a.RTBCostKnown {
		t.Errorf("non-finite position: rtb cost known")
	}
	noRange := caps
	noRange.MaxRangeM = 0
	if a := BindAgent(noRange, st, stations, nil, nil); a.RTBCostKnown {
		t.Errorf("zero range: rtb cost known")
	}
}

func TestBindMission(t *testing.T) {
	area := testArea(t, 1000)
	area.Grid.Explored[area.Grid.CellAt(0, 0)] = true
	m := BindMission(domain.Mission{Area: area, State: domain.MissionRunning})
	if m.CoveragePct != 1 || m.State != domain.MissionRunning {
		t.Errorf("mission = %+v, want 1 %% coverage (1 of 100 cells)", m)
	}
}

func TestCheckConstraints(t *testing.T) {
	p := mustParse(t, testPackYAML)
	low := testAgent("DRONE-02")
	low.BatteryPct = 24 // needs 10 + 15
	out := testAgent("DRONE-03")
	out.Position = pos(500, 2000)
	lost := testAgent("DRONE-04")
	lost.Position = domain.Position{Lat: math.NaN()}
	lost.RTBCostKnown = false

	got, err := CheckConstraints(p, testEnv(t, 0, lost, out, testAgent("DRONE-01"), low))
	if err != nil {
		t.Fatal(err)
	}
	want := []Violation{
		{Constraint: "battery-reserve", Agent: "DRONE-02"},
		{Constraint: "geofence", Agent: "DRONE-03"},
		{Constraint: "battery-reserve", Agent: "DRONE-04"}, // rtb cost absent: comparison false
		{Constraint: "geofence", Agent: "DRONE-04"},        // position absent: within false
	}
	if !slices.Equal(got, want) {
		t.Fatalf("violations = %+v, want %+v", got, want)
	}
}

// firingTimes runs ticks 1..n of 100 ms, carrying window state, and reports
// the mission times at which ruleID fired for the agent.
func firingTimes(t *testing.T, p *Pack, n int, ruleID string, agentAt func(tickMs int64) Agent) []int64 {
	t.Helper()
	var windows []Window
	var fired []int64
	for k := int64(1); k <= int64(n); k++ {
		ms := k * domain.TickIntervalMs
		res := mustEvaluate(t, p, testEnv(t, ms, agentAt(ms)), windows)
		windows = res.Windows
		for _, f := range res.Firings {
			if f.RuleID == ruleID {
				fired = append(fired, ms)
			}
		}
	}
	return fired
}

// A 5 s window fires once, 5 s after the condition was first observed, and
// re-arms only after the condition has been false.
func TestWindowFiresOnceAfterDuration(t *testing.T) {
	p := mustParse(t, testPackYAML)
	got := firingTimes(t, p, 150, "reassign-on-link-loss", func(ms int64) Agent {
		a := testAgent("DRONE-01")
		if (ms >= 1000 && ms < 7000) || ms >= 8000 {
			a.Link = domain.LinkLost
		}
		return a
	})
	if want := []int64{6000, 13000}; !slices.Equal(got, want) {
		t.Fatalf("fired at %v, want %v", got, want)
	}
}

// A single tick where the condition is false restarts the clock.
func TestWindowClearedByFalseTick(t *testing.T) {
	p := mustParse(t, testPackYAML)
	got := firingTimes(t, p, 120, "reassign-on-link-loss", func(ms int64) Agent {
		a := testAgent("DRONE-01")
		if ms >= 1000 && ms != 3100 {
			a.Link = domain.LinkLost
		}
		return a
	})
	if want := []int64{8200}; !slices.Equal(got, want) {
		t.Fatalf("fired at %v, want %v", got, want)
	}
}

func TestZeroWindowFiresOnEdge(t *testing.T) {
	p := mustParse(t, testPackYAML)
	got := firingTimes(t, p, 30, "also-100", func(ms int64) Agent {
		a := testAgent("DRONE-01")
		if (ms >= 500 && ms < 1000) || ms >= 2000 {
			a.Mode = domain.ModeDown
		}
		return a
	})
	if want := []int64{500, 2000}; !slices.Equal(got, want) {
		t.Fatalf("fired at %v, want %v", got, want)
	}
}

// Of the rules ready for one agent the highest priority wins, ties to the
// lowest id, and every other one is reported with the id that shadowed it.
// Shadowed rules consume their edge.
func TestShadowing(t *testing.T) {
	p := packWithRules(t, `  - {id: zeta, when: agent.mode == DOWN, then: release_lanes(agent), priority: 200}
  - {id: alpha, when: agent.mode == DOWN, then: return_to_base(agent), priority: 200}
  - {id: low, when: agent.mode == DOWN, then: 'notify_operator(message="down")', priority: 100}
  - {id: other, when: agent.battery_pct < 50, then: 'notify_operator(message="low")', priority: 500}
`)
	down := testAgent("DRONE-01")
	down.Mode = domain.ModeDown

	res := mustEvaluate(t, p, testEnv(t, 100, down, testAgent("DRONE-02")), nil)
	if len(res.Firings) != 1 {
		t.Fatalf("firings = %+v, want one", res.Firings)
	}
	f := res.Firings[0]
	if f.Agent != "DRONE-01" || f.RuleID != "alpha" || f.Priority != 200 || !slices.Equal(f.Actions, []Action{{Kind: ActionReturnToBase}}) {
		t.Fatalf("firing = %+v", f)
	}
	wantShadow := []domain.ShadowedRule{
		{RuleID: "zeta", ShadowedBy: "alpha", Priority: 200},
		{RuleID: "low", ShadowedBy: "alpha", Priority: 100},
	}
	if !slices.Equal(f.Shadowed, wantShadow) {
		t.Fatalf("shadowed = %+v, want %+v", f.Shadowed, wantShadow)
	}

	next := mustEvaluate(t, p, testEnv(t, 200, down, testAgent("DRONE-02")), res.Windows)
	if len(next.Firings) != 0 {
		t.Fatalf("second tick fired %+v, want nothing: every edge was consumed", next.Firings)
	}
}

// Priority is resolved per agent: two agents firing different rules on one
// tick do not shadow each other.
func TestPriorityIsPerAgent(t *testing.T) {
	p := mustParse(t, testPackYAML)
	down := testAgent("DRONE-01")
	down.Mode = domain.ModeDown
	low := testAgent("DRONE-02")
	low.BatteryPct = 20
	res := mustEvaluate(t, p, testEnv(t, 100, low, down), nil)
	if len(res.Firings) != 2 ||
		res.Firings[0].Agent != "DRONE-01" || res.Firings[0].RuleID != "also-100" || len(res.Firings[0].Shadowed) != 0 ||
		res.Firings[1].Agent != "DRONE-02" || res.Firings[1].RuleID != "rtb-on-low-battery" || len(res.Firings[1].Shadowed) != 0 {
		t.Fatalf("firings = %+v", res.Firings)
	}
}

func TestViolatesInRule(t *testing.T) {
	p := mustParse(t, testPackYAML)
	got := firingTimes(t, p, 20, "rtb-on-low-battery", func(ms int64) Agent {
		a := testAgent("DRONE-01")
		a.BatteryPct = 30 - int(ms/200) // falls below 10 + 15 at 1200 ms
		return a
	})
	if want := []int64{1200}; !slices.Equal(got, want) {
		t.Fatalf("fired at %v, want %v", got, want)
	}
}

// Every comparison reading an absent value is false, != included.
func TestAbsentLane(t *testing.T) {
	p := packWithRules(t, `  - {id: early, when: lane.progress_pct < 50, then: release_lanes(agent), priority: 2}
  - {id: not-nine, when: lane.id != "lane-09", then: return_to_base(agent), priority: 1}
`)
	free := testAgent("DRONE-01")
	busy := testAgent("DRONE-02")
	busy.Lane = &LaneBinding{ID: "lane-02", Index: 2, ProgressPct: 25}

	res := mustEvaluate(t, p, testEnv(t, 100, free, busy), nil)
	if len(res.Firings) != 1 || res.Firings[0].Agent != "DRONE-02" || res.Firings[0].RuleID != "early" ||
		!slices.Equal(res.Firings[0].Shadowed, []domain.ShadowedRule{{RuleID: "not-nine", ShadowedBy: "early", Priority: 1}}) {
		t.Fatalf("firings = %+v", res.Firings)
	}
}

func TestArithmetic(t *testing.T) {
	p := packWithRules(t, `  - {id: margin, when: agent.battery_pct - agent.rtb_cost_pct < doctrine.battery_reserve_pct + 5, then: 'notify_operator(message="thin")', priority: 1}
`)
	for _, c := range []struct {
		battery int
		fires   bool
	}{{35, false}, {30, false}, {29, true}} { // 30 - 10 = 20 is not < 15 + 5
		a := testAgent("DRONE-01")
		a.BatteryPct = c.battery
		res := mustEvaluate(t, p, testEnv(t, 100, a), nil)
		if got := len(res.Firings) == 1; got != c.fires {
			t.Errorf("battery %d: fired %v, want %v", c.battery, got, c.fires)
		}
	}
}

// within is the geofence at grid resolution: the position's cell must be an
// AO cell. It can disagree with the exact polygon by up to half a cell.
func TestWithinGridResolution(t *testing.T) {
	p := packWithRules(t, `  - {id: inside, when: agent.position within mission.area, then: release_lanes(agent), priority: 1}
`)
	cases := []struct {
		name    string
		heightM float64
		at      domain.Position
		want    bool
	}{
		{"interior", 1060, pos(500, 500), true},
		{"outside the raster", 1060, pos(500, 1150), false},
		{"west of the raster", 1060, pos(-50, 500), false},
		// Row 10 spans 1000 to 1100 m; its centre (1050 m) is inside a
		// 1060 m polygon, so the whole cell is AO.
		{"outside the polygon, in an AO cell", 1060, pos(500, 1080), true},
		// Its centre is outside a 1040 m polygon, so the cell is not AO.
		{"inside the polygon, in a non-AO cell", 1040, pos(500, 1030), false},
	}
	for _, c := range cases {
		env := testEnv(t, 100)
		env.Mission.Area = testArea(t, c.heightM)
		a := testAgent("DRONE-01")
		a.Position = c.at
		env.Agents = []Agent{a}
		res := mustEvaluate(t, p, env, nil)
		if got := len(res.Firings) == 1; got != c.want {
			t.Errorf("%s: within = %v, want %v (polygon contains: %v)", c.name, got, c.want, env.Mission.Area.Polygon.Contains(c.at))
		}
	}
}

func TestWindowsDroppedForAbsentAgentsAndRules(t *testing.T) {
	p := mustParse(t, testPackYAML)
	a := testAgent("DRONE-01")
	a.Link = domain.LinkLost
	windows := []Window{
		{RuleID: "reassign-on-link-loss", Agent: "DRONE-09", SinceMs: 100},
		{RuleID: "retired-rule", Agent: "DRONE-01", SinceMs: 100},
		{RuleID: "reassign-on-link-loss", Agent: "DRONE-01", SinceMs: 300},
	}
	res := mustEvaluate(t, p, testEnv(t, 1000, a), windows)
	want := []Window{{RuleID: "reassign-on-link-loss", Agent: "DRONE-01", SinceMs: 300}}
	if !slices.Equal(res.Windows, want) {
		t.Fatalf("windows = %+v, want %+v", res.Windows, want)
	}
}

func TestInvalidEnv(t *testing.T) {
	p := mustParse(t, testPackYAML)
	for _, agents := range [][]Agent{
		{testAgent("DRONE-01"), testAgent("DRONE-01")},
		{testAgent("")},
	} {
		if _, err := Evaluate(p, testEnv(t, 100, agents...), nil); !errors.Is(err, ErrInvalidEnv) {
			t.Errorf("Evaluate(%v) = %v, want ErrInvalidEnv", agents, err)
		}
		if _, err := CheckConstraints(p, testEnv(t, 100, agents...)); !errors.Is(err, ErrInvalidEnv) {
			t.Errorf("CheckConstraints(%v) = %v, want ErrInvalidEnv", agents, err)
		}
	}
}

// scenarioAgents is a fleet whose state is a pure function of mission time:
// battery drains, links drop and recover, one vector goes down, one drifts
// out of the AO.
func scenarioAgents(ms int64) []Agent {
	k := ms / domain.TickIntervalMs
	var out []Agent
	for i, id := range []string{"DRONE-01", "DRONE-02", "DRONE-03", "DRONE-04", "DRONE-05", "GROUND-01"} {
		a := testAgent(id)
		a.BatteryPct = max(0, 100-int(k)/(3+i))
		a.RTBCostPct = 5 + i
		a.Position = pos(100+float64(i)*150, 100+float64(k)*float64(i))
		switch id {
		case "DRONE-02":
			if k >= 50 && k < 120 {
				a.Link = domain.LinkLost
			}
		case "DRONE-03":
			if (k >= 30 && k < 40) || (k >= 60 && k < 100) {
				a.Link = domain.LinkDegraded
			}
		case "DRONE-04":
			if k >= 200 {
				// Down and drained on the same tick: two rules ready at
				// once, so the run exercises shadowing.
				a.Mode = domain.ModeDown
				a.BatteryPct = 10
			}
		}
		if k%7 == 0 {
			a.Lane = &LaneBinding{ID: domain.LaneID("lane-0" + string(rune('0'+i))), Index: i, ProgressPct: float64(k % 100)}
		}
		out = append(out, a)
	}
	return out
}

// TestDeterminism asserts that a seeded run of the evaluator produces byte
// identical output across repetitions, whatever order the agents and the
// carried windows are handed in.
func TestDeterminism(t *testing.T) {
	p := mustParse(t, testPackYAML)
	area := testArea(t, 1000)
	var firings, shadowed int
	run := func(seed uint64) string {
		rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		var windows []Window
		trace := eventlog.MustHashOf("")
		for k := int64(1); k <= 300; k++ {
			ms := k * domain.TickIntervalMs
			agents := scenarioAgents(ms)
			rng.Shuffle(len(agents), func(i, j int) { agents[i], agents[j] = agents[j], agents[i] })
			rng.Shuffle(len(windows), func(i, j int) { windows[i], windows[j] = windows[j], windows[i] })
			env := Env{TickMs: ms, Mission: MissionBinding{Area: area, CoveragePct: float64(k) / 3, State: domain.MissionRunning}, Agents: agents}
			res := mustEvaluate(t, p, env, windows)
			windows = res.Windows
			trace = eventlog.MustHashOf([]any{trace.String(), res})
			for _, f := range res.Firings {
				firings++
				shadowed += len(f.Shadowed)
			}
		}
		return trace.String()
	}
	first := run(0)
	if firings == 0 || shadowed == 0 {
		t.Fatalf("scenario exercises %d firings and %d shadowed rules, want both", firings, shadowed)
	}
	for seed := uint64(1); seed < 100; seed++ {
		if got := run(seed); got != first {
			t.Fatalf("seed %d: trace %s, want %s", seed, got, first)
		}
	}
}
