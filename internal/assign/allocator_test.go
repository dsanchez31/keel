package assign

import (
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

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

// laneAt is a 1000 m lane running north from (eastM, 0).
func laneAt(index int, id string, eastM float64) domain.Lane {
	return domain.Lane{
		ID:        domain.LaneID(id),
		Index:     index,
		Waypoints: []domain.Position{pos(eastM, 0), pos(eastM, 1000)},
	}
}

func fourLanes() []domain.Lane {
	return []domain.Lane{
		laneAt(0, "lane-00", 0),
		laneAt(1, "lane-01", 300),
		laneAt(2, "lane-02", 600),
		laneAt(3, "lane-03", 900),
	}
}

func vector(id string, domainKind domain.Domain, tags []string, at domain.Position) Vector {
	c := domain.Capabilities{
		ID: domain.VectorID(id), Domain: domainKind, Tags: tags,
		CruiseSpeed: 20, MaxRangeM: 20000, SensorRadiusM: 120,
	}
	c.SortTags()
	return Vector{
		Caps:   c,
		State:  domain.VectorState{ID: c.ID, Position: at, BatteryPct: 100, Link: domain.LinkOK, Mode: domain.ModeScanning},
		FreeAt: at,
	}
}

var droneTags = []string{"aerial", "camera", "gps"}

// mixedFleet is four capable drones below the lanes, plus one vector for each
// rejection reason, listed out of id order on purpose.
func mixedFleet() []Vector {
	lost := vector("DRONE-05", domain.DomainAerial, droneTags, pos(0, -10))
	lost.State.Link = domain.LinkLost
	down := vector("DRONE-06", domain.DomainAerial, droneTags, pos(0, -15))
	down.State.Mode = domain.ModeDown
	weak := vector("DRONE-07", domain.DomainAerial, droneTags, pos(0, -20))
	weak.State.BatteryPct = 20 // 5 % above a 15 % reserve: 1000 m of range
	lostPos := vector("DRONE-08", domain.DomainAerial, droneTags, domain.Position{Lat: math.NaN()})
	lostPos.FreeAt = domain.Position{Lat: math.NaN()}

	return []Vector{
		vector("RELAY-01", domain.DomainAerial, []string{"aerial", "radio_mesh"}, pos(500, -50)),
		vector("DRONE-04", domain.DomainAerial, droneTags, pos(900, -100)),
		vector("DRONE-02", domain.DomainAerial, droneTags, pos(300, -100)),
		weak, down, lost, lostPos,
		vector("GROUND-01", domain.DomainGround, []string{"camera", "gps", "ground"}, pos(100, -50)),
		vector("DRONE-01", domain.DomainAerial, droneTags, pos(0, -100)),
		vector("DRONE-03", domain.DomainAerial, droneTags, pos(600, -100)),
	}
}

func request(policy domain.AssignPolicy, lanes []domain.Lane, fleet []Vector) Request {
	return Request{
		Policy:     policy,
		Requires:   []string{"camera", "aerial"},
		Lanes:      lanes,
		Fleet:      fleet,
		ReservePct: 15,
	}
}

func mustAllocate(t *testing.T, r Request) []Assignment {
	t.Helper()
	out, err := Allocate(r)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	checkTraces(t, r, out)
	return out
}

// checkTraces asserts the phase 5 contract on every assignment: every vector
// of the fleet is a candidate, only the winner is accepted, every loser says
// why, and no vector wins two lanes.
func checkTraces(t *testing.T, r Request, out []Assignment) {
	t.Helper()
	if len(out) != len(r.Lanes) {
		t.Fatalf("%d assignments for %d lanes", len(out), len(r.Lanes))
	}
	won := map[domain.VectorID]domain.LaneID{}
	for _, a := range out {
		if len(a.Candidates) != len(r.Fleet) {
			t.Fatalf("%s: %d candidates, fleet has %d", a.Lane, len(a.Candidates), len(r.Fleet))
		}
		accepted := 0
		for _, c := range a.Candidates {
			if !c.Rejected {
				accepted++
				if c.Vector != a.Winner {
					t.Fatalf("%s: accepted candidate %s is not the winner %s", a.Lane, c.Vector, a.Winner)
				}
				continue
			}
			if c.Reason == "" {
				t.Fatalf("%s: %s rejected without a reason", a.Lane, c.Vector)
			}
		}
		if (a.Winner != "") != (accepted == 1) || accepted > 1 {
			t.Fatalf("%s: winner %q with %d accepted candidates", a.Lane, a.Winner, accepted)
		}
		if a.Winner != "" {
			if prev, dup := won[a.Winner]; dup {
				t.Fatalf("%s won both %s and %s", a.Winner, prev, a.Lane)
			}
			won[a.Winner] = a.Lane
		}
		if a.Rationale == "" {
			t.Fatalf("%s: empty rationale", a.Lane)
		}
	}
}

func winners(out []Assignment) map[domain.LaneID]domain.VectorID {
	m := map[domain.LaneID]domain.VectorID{}
	for _, a := range out {
		m[a.Lane] = a.Winner
	}
	return m
}

func reasonOf(a Assignment, v domain.VectorID) string {
	for _, c := range a.Candidates {
		if c.Vector == v {
			return c.Reason
		}
	}
	return "<absent>"
}

func TestNearestCapable(t *testing.T) {
	r := request(domain.PolicyNearestCapable, fourLanes(), mixedFleet())
	out := mustAllocate(t, r)

	want := map[domain.LaneID]domain.VectorID{
		"lane-00": "DRONE-01", "lane-01": "DRONE-02", "lane-02": "DRONE-03", "lane-03": "DRONE-04",
	}
	for lane, v := range want {
		if got := winners(out)[lane]; got != v {
			t.Fatalf("%s won by %s, want %s", lane, got, v)
		}
	}

	a := out[0]
	for v, reason := range map[domain.VectorID]string{
		"DRONE-05":  "link lost",
		"DRONE-06":  "vector down",
		"DRONE-08":  "no known position",
		"GROUND-01": "missing capability aerial",
		"RELAY-01":  "missing capability camera",
		"DRONE-02":  "assigned lane-01 in this round",
	} {
		if got := reasonOf(a, v); got != reason {
			t.Fatalf("lane-00, %s: reason %q, want %q", v, got, reason)
		}
	}
	if got := reasonOf(a, "DRONE-07"); !strings.HasPrefix(got, "insufficient range: needs 2040 m, has 1000 m") {
		t.Fatalf("lane-00, DRONE-07: reason %q", got)
	}
	if a.Candidates[0].Vector != "DRONE-01" || a.Candidates[0].Rejected {
		t.Fatalf("the winner does not lead the candidate list: %+v", a.Candidates[0])
	}
	if !strings.HasPrefix(a.Rationale, "DRONE-01 is the nearest capable vector to lane-00 at 100 m") {
		t.Fatalf("rationale %q", a.Rationale)
	}
}

// A vector sent home by doctrine keeps its link and is not down, so only its
// mode says it must not be handed work. It sits on the lane's first waypoint,
// the cheapest candidate there is, and still loses to a farther drone.
func TestReturningToBaseIsIneligible(t *testing.T) {
	rtb := vector("DRONE-09", domain.DomainAerial, droneTags, pos(0, 0))
	rtb.State.Mode = domain.ModeRTB
	fleet := []Vector{rtb, vector("DRONE-01", domain.DomainAerial, droneTags, pos(0, -500))}

	for _, policy := range []domain.AssignPolicy{domain.PolicyNearestCapable, domain.PolicyRoundRobin, domain.PolicyLowestCost} {
		out := mustAllocate(t, request(policy, fourLanes()[:1], fleet))
		if out[0].Winner != "DRONE-01" {
			t.Fatalf("%s: lane-00 won by %q, want DRONE-01", policy, out[0].Winner)
		}
		if got := reasonOf(out[0], "DRONE-09"); got != "returning to base" {
			t.Fatalf("%s: DRONE-09 reason %q, want %q", policy, got, "returning to base")
		}
	}
}

// With the drones placed in reverse id order, nearest_capable and round_robin
// disagree, which is what shows each policy is the one running.
func TestPoliciesDisagree(t *testing.T) {
	fleet := []Vector{
		vector("DRONE-01", domain.DomainAerial, droneTags, pos(900, -100)),
		vector("DRONE-02", domain.DomainAerial, droneTags, pos(600, -100)),
		vector("DRONE-03", domain.DomainAerial, droneTags, pos(300, -100)),
		vector("DRONE-04", domain.DomainAerial, droneTags, pos(0, -100)),
	}
	nearest := winners(mustAllocate(t, request(domain.PolicyNearestCapable, fourLanes(), fleet)))
	robin := winners(mustAllocate(t, request(domain.PolicyRoundRobin, fourLanes(), fleet)))
	if nearest["lane-00"] != "DRONE-04" || nearest["lane-03"] != "DRONE-01" {
		t.Fatalf("nearest_capable %v", nearest)
	}
	if robin["lane-00"] != "DRONE-01" || robin["lane-01"] != "DRONE-02" || robin["lane-03"] != "DRONE-04" {
		t.Fatalf("round_robin %v", robin)
	}
}

// A vector that cannot fly a lane passes its turn in the rotation.
func TestRoundRobinSkipsInfeasible(t *testing.T) {
	r := request(domain.PolicyRoundRobin, fourLanes(), mixedFleet())
	got := winners(mustAllocate(t, r))
	want := map[domain.LaneID]domain.VectorID{
		"lane-00": "DRONE-01", "lane-01": "DRONE-02", "lane-02": "DRONE-03", "lane-03": "DRONE-04",
	}
	for lane, v := range want {
		if got[lane] != v {
			t.Fatalf("round_robin %v, want %v", got, want)
		}
	}
}

// Greedy in lane order gives lane A its nearest vector, which is the vector
// lane B needed. The exact solver finds the cheaper pairing.
func TestLowestCostBeatsGreedy(t *testing.T) {
	lanes := []domain.Lane{laneAt(0, "lane-A", 0), laneAt(1, "lane-B", 1000)}
	fleet := []Vector{
		vector("V1", domain.DomainAerial, droneTags, pos(5, 0)),
		vector("V2", domain.DomainAerial, droneTags, pos(-20, 0)),
	}
	stations := []domain.Station{{Name: "GCS", Position: pos(500, 1000)}}

	greedy := request(domain.PolicyNearestCapable, lanes, fleet)
	greedy.Stations = stations
	exact := request(domain.PolicyLowestCost, lanes, fleet)
	exact.Stations = stations

	g := winners(mustAllocate(t, greedy))
	e := winners(mustAllocate(t, exact))
	if g["lane-A"] != "V1" || g["lane-B"] != "V2" {
		t.Fatalf("nearest_capable %v, want lane-A V1, lane-B V2", g)
	}
	if e["lane-A"] != "V2" || e["lane-B"] != "V1" {
		t.Fatalf("lowest_cost %v, want lane-A V2, lane-B V1", e)
	}
}

func TestMoreLanesThanVectors(t *testing.T) {
	fleet := []Vector{
		vector("DRONE-01", domain.DomainAerial, droneTags, pos(0, -100)),
		vector("DRONE-02", domain.DomainAerial, droneTags, pos(900, -100)),
		vector("GROUND-01", domain.DomainGround, []string{"camera", "ground"}, pos(300, -100)),
	}
	for _, policy := range []domain.AssignPolicy{domain.PolicyNearestCapable, domain.PolicyRoundRobin, domain.PolicyLowestCost} {
		t.Run(string(policy), func(t *testing.T) {
			out := mustAllocate(t, request(policy, fourLanes(), fleet))
			assigned := 0
			for _, a := range out {
				if a.Winner != "" {
					assigned++
					continue
				}
				if !strings.HasPrefix(a.Rationale, "no vector can take") {
					t.Fatalf("%s: rationale %q", a.Lane, a.Rationale)
				}
			}
			if assigned != 2 {
				t.Fatalf("%d lanes assigned, want 2", assigned)
			}
		})
	}
}

func TestBusyVector(t *testing.T) {
	lanes := []domain.Lane{laneAt(4, "lane-04", 300)}
	busy := vector("DRONE-01", domain.DomainAerial, droneTags, pos(0, 500))
	busy.FreeAt = pos(0, 1000)
	busy.CommittedM = 500
	busy.Holds = []domain.LaneID{"lane-00"}
	full := vector("DRONE-02", domain.DomainAerial, droneTags, pos(300, -50))
	full.CommittedM = 19000
	full.Holds = []domain.LaneID{"lane-01"}

	out := mustAllocate(t, request(domain.PolicyNearestCapable, lanes, []Vector{busy, full}))
	a := out[0]
	if a.Winner != "DRONE-01" {
		t.Fatalf("winner %s, want the busy vector with range to spare", a.Winner)
	}
	if !strings.HasSuffix(a.Rationale, "queued after lane-00") {
		t.Fatalf("rationale %q does not say the lane is queued", a.Rationale)
	}
	if got := reasonOf(a, "DRONE-02"); !strings.HasPrefix(got, "insufficient range") {
		t.Fatalf("DRONE-02: reason %q, its committed distance should exhaust its range", got)
	}
	// Transit is measured from FreeAt, the end of the held lane, not from
	// the vector's current position.
	if c := a.Candidates[0]; math.Abs(c.Cost-domain.HaversineM(pos(0, 1000), pos(300, 0))) > 1e-6 {
		t.Fatalf("transit cost %v, want the distance from FreeAt", c.Cost)
	}
}

func TestAllocateInvalid(t *testing.T) {
	ok := request(domain.PolicyNearestCapable, fourLanes(), mixedFleet())
	cases := map[string]func(*Request){
		"unknown policy": func(r *Request) { r.Policy = "fastest" },
		"reserve 101":    func(r *Request) { r.ReservePct = 101 },
		"duplicate id":   func(r *Request) { r.Fleet = append(r.Fleet, r.Fleet[0]) },
		"empty id":       func(r *Request) { r.Fleet = append(r.Fleet, Vector{}) },
		"no waypoint":    func(r *Request) { r.Lanes = append(r.Lanes, domain.Lane{ID: "lane-99", Index: 99}) },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			r := ok
			r.Fleet = append([]Vector(nil), ok.Fleet...)
			r.Lanes = append([]domain.Lane(nil), ok.Lanes...)
			mod(&r)
			if _, err := Allocate(r); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err %v, want ErrInvalidRequest", err)
			}
		})
	}
}

// input order does not reach the decision.
func TestAllocateDeterminism(t *testing.T) {
	for _, policy := range []domain.AssignPolicy{domain.PolicyNearestCapable, domain.PolicyRoundRobin, domain.PolicyLowestCost} {
		t.Run(string(policy), func(t *testing.T) {
			first := mustAllocate(t, request(policy, fourLanes(), mixedFleet()))
			want := eventlog.MustHashOf(first)
			for i := 1; i < 100; i++ {
				fleet := mixedFleet()
				// Rotate the input order: the result must not move.
				k := i % len(fleet)
				fleet = append(fleet[k:], fleet[:k]...)
				got := eventlog.MustHashOf(mustAllocate(t, request(policy, fourLanes(), fleet)))
				if got != want {
					t.Fatalf("repetition %d: hash %s, want %s", i, got, want)
				}
			}
		})
	}
}

// The Hungarian solver against brute force on small random matrices.
func TestHungarianOptimal(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for trial := range 300 {
		n := 1 + rng.IntN(5)
		m := n + rng.IntN(3)
		cost := make([][]int64, n)
		for i := range cost {
			cost[i] = make([]int64, m)
			for j := range cost[i] {
				cost[i][j] = int64(rng.IntN(1000))
			}
		}
		got := hungarian(cost)
		seen := make([]bool, m)
		var total int64
		for i, j := range got {
			if seen[j] {
				t.Fatalf("trial %d: column %d used twice", trial, j)
			}
			seen[j] = true
			total += cost[i][j]
		}
		if best := bruteForce(cost); total != best {
			t.Fatalf("trial %d: total %d, optimum %d", trial, total, best)
		}
	}
}

func bruteForce(cost [][]int64) int64 {
	n, m := len(cost), len(cost[0])
	used := make([]bool, m)
	best := int64(math.MaxInt64)
	var rec func(i int, acc int64)
	rec = func(i int, acc int64) {
		if i == n {
			best = min(best, acc)
			return
		}
		for j := range m {
			if !used[j] {
				used[j] = true
				rec(i+1, acc+cost[i][j])
				used[j] = false
			}
		}
	}
	rec(0, 0)
	return best
}
