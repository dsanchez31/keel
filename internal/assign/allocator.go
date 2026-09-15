// Package assign matches lanes to vectors: capability matching, a range check
// against the doctrine battery reserve, and one of three allocation policies.
//
// It is in the decision path, so it is a pure function of its request. Every
// candidate the allocator looked at is reported, winner and losers alike,
// each loser with the reason it lost: a decision that reports only its winner
// cannot answer "why not that vector", which is the question an operator
// actually asks.
package assign

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/dsanchez31/keel/internal/domain"
)

// Vector is one fleet member as the allocator sees it.
type Vector struct {
	Caps  domain.Capabilities `json:"caps"`
	State domain.VectorState  `json:"state"`
	// FreeAt is where the vector becomes free for new work: its position
	// when it holds no lane, the last waypoint of its last held lane
	// otherwise. A busy vector queues new work after what it already holds.
	FreeAt domain.Position `json:"free_at"`
	// CommittedM is the distance it still owes to the lanes it holds.
	CommittedM float64 `json:"committed_m"`
	// Holds lists the lanes it already holds, sorted.
	Holds []domain.LaneID `json:"holds,omitempty"`
}

// Request is one allocation round.
type Request struct {
	Policy domain.AssignPolicy `json:"policy"`
	// Requires is the capability set the plan demands.
	Requires []string `json:"requires"`
	// Lanes are the lanes to fill. They are allocated in Index order.
	Lanes []domain.Lane `json:"lanes"`
	// Fleet is every vector known to the mission, eligible or not.
	Fleet []Vector `json:"fleet"`
	// Stations are the ground stations a vector returns to.
	Stations []domain.Station `json:"stations,omitempty"`
	// ReservePct is the doctrine battery reserve, withheld from range.
	ReservePct int `json:"reserve_pct"`
}

// Assignment is the outcome for one lane.
type Assignment struct {
	Lane domain.LaneID `json:"lane"`
	// Winner is empty when no vector could take the lane.
	Winner domain.VectorID `json:"winner,omitempty"`
	// Candidates lists every vector of the fleet, sorted by
	// domain.SortCandidates. Only the winner is not rejected.
	Candidates []domain.Candidate `json:"candidates"`
	Rationale  string             `json:"rationale"`
}

// ErrInvalidRequest reports a request the allocator cannot reason about.
var ErrInvalidRequest = errors.New("assign: invalid request")

// Allocate assigns each lane of the request to at most one vector, and each
// vector to at most one lane of the request.
//
// Eligibility is checked in a fixed order, and the first failure is the
// reason a vector is rejected: link lost, vector down, returning to base, no
// known position, missing capability, insufficient range. The range a vector needs is its
// committed distance, plus the transit from FreeAt to the lane's first
// waypoint, plus the lane, plus the return from the lane's last waypoint to
// the nearest station (to FreeAt when there is none). It must fit in
// MaxRangeM derated to the battery left above the reserve.
//
// Policies:
//   - nearest_capable: lanes in Index order, each to the eligible vector with
//     the shortest transit, ties on VectorID.
//   - round_robin: eligible vectors in id order take lanes in turn, a vector
//     that cannot fly a lane passing its turn to the next.
//   - lowest_cost: the assignment minimising the total range needed over the
//     whole round, solved exactly on integer millimetres.
func Allocate(r Request) ([]Assignment, error) {
	if err := validate(r); err != nil {
		return nil, err
	}

	fleet := slices.Clone(r.Fleet)
	slices.SortStableFunc(fleet, func(a, b Vector) int { return strings.Compare(string(a.Caps.ID), string(b.Caps.ID)) })
	lanes := slices.Clone(r.Lanes)
	slices.SortStableFunc(lanes, func(a, b domain.Lane) int {
		if a.Index != b.Index {
			return a.Index - b.Index
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	requires := slices.Clone(r.Requires)
	slices.Sort(requires)
	requires = slices.Compact(requires)

	evals := make([][]eval, len(lanes))
	for li, l := range lanes {
		evals[li] = make([]eval, len(fleet))
		for vi, v := range fleet {
			evals[li][vi] = evaluate(v, l, requires, r.Stations, r.ReservePct)
		}
	}

	var winners []int
	switch r.Policy {
	case domain.PolicyNearestCapable:
		winners = nearestCapable(evals, fleet)
	case domain.PolicyRoundRobin:
		winners = roundRobin(evals, fleet)
	case domain.PolicyLowestCost:
		winners = lowestCost(evals)
	}

	laneOf := make([]int, len(fleet))
	for vi := range laneOf {
		laneOf[vi] = -1
	}
	for li, vi := range winners {
		if vi >= 0 {
			laneOf[vi] = li
		}
	}

	out := make([]Assignment, len(lanes))
	for li, l := range lanes {
		out[li] = assignment(r.Policy, l, lanes, fleet, evals[li], winners[li], laneOf)
	}
	return out, nil
}

func validate(r Request) error {
	switch r.Policy {
	case domain.PolicyNearestCapable, domain.PolicyRoundRobin, domain.PolicyLowestCost:
	default:
		return fmt.Errorf("%w: unknown policy %q", ErrInvalidRequest, r.Policy)
	}
	if r.ReservePct < 0 || r.ReservePct > 100 {
		return fmt.Errorf("%w: reserve %d%% outside 0..100", ErrInvalidRequest, r.ReservePct)
	}
	ids := make([]string, len(r.Fleet))
	for i, v := range r.Fleet {
		if v.Caps.ID == "" {
			return fmt.Errorf("%w: fleet member %d has no id", ErrInvalidRequest, i)
		}
		ids[i] = string(v.Caps.ID)
	}
	slices.Sort(ids)
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return fmt.Errorf("%w: vector %s appears twice", ErrInvalidRequest, ids[i])
		}
	}
	for _, l := range r.Lanes {
		if len(l.Waypoints) == 0 {
			return fmt.Errorf("%w: lane %s has no waypoint", ErrInvalidRequest, l.ID)
		}
	}
	return nil
}

// eval is one (lane, vector) pair after the eligibility checks.
type eval struct {
	rejected bool
	reason   string
	// known reports whether the vector has a position, so distances exist.
	known    bool
	transitM float64
	needM    float64
	rangeM   float64
}

func evaluate(v Vector, l domain.Lane, requires []string, stations []domain.Station, reservePct int) eval {
	var e eval
	if v.FreeAt.Finite() {
		e.known = true
		first, last := l.Waypoints[0], l.Waypoints[len(l.Waypoints)-1]
		e.transitM = domain.HaversineM(v.FreeAt, first)
		rtb := domain.HaversineM(last, v.FreeAt)
		if len(stations) > 0 {
			best := domain.MinByCost(stations,
				func(s domain.Station) float64 { return domain.HaversineM(last, s.Position) },
				func(s domain.Station) string { return s.Name },
			)
			rtb = domain.HaversineM(last, stations[best].Position)
		}
		e.needM = v.CommittedM + e.transitM + l.LengthM() + rtb
	}
	e.rangeM = math.Max(0, v.Caps.MaxRangeM*float64(v.State.BatteryPct-reservePct)/100)

	switch {
	case v.State.Link == domain.LinkLost:
		e.reject("link lost")
	case v.State.Mode == domain.ModeDown:
		e.reject("vector down")
	case v.State.Mode == domain.ModeRTB:
		e.reject("returning to base")
	case !e.known:
		e.reject("no known position")
	default:
		for _, tag := range requires {
			if !v.Caps.HasTag(tag) {
				e.reject("missing capability " + tag)
				return e
			}
		}
		if domain.CompareCost(e.needM, e.rangeM) > 0 {
			e.reject(fmt.Sprintf("insufficient range: needs %.0f m, has %.0f m above the %d%% reserve", e.needM, e.rangeM, reservePct))
		}
	}
	return e
}

func (e *eval) reject(reason string) {
	e.rejected = true
	e.reason = reason
}

// cost is what a policy ranks on: transit distance for the two greedy
// policies, total range needed for lowest_cost.
func cost(policy domain.AssignPolicy, e eval) float64 {
	if policy == domain.PolicyLowestCost {
		return e.needM
	}
	return e.transitM
}

// nearestCapable gives each lane, in Index order, the eligible vector with
// the shortest transit that has not already won a lane, ties on VectorID.
func nearestCapable(evals [][]eval, fleet []Vector) []int {
	winners := make([]int, len(evals))
	used := make([]bool, len(fleet))
	for li, row := range evals {
		var free []int
		for vi, e := range row {
			if !e.rejected && !used[vi] {
				free = append(free, vi)
			}
		}
		best := domain.MinByCost(free,
			func(vi int) float64 { return row[vi].transitM },
			func(vi int) string { return string(fleet[vi].Caps.ID) },
		)
		winners[li] = -1
		if best >= 0 {
			winners[li] = free[best]
			used[free[best]] = true
		}
	}
	return winners
}

// roundRobin walks the vectors in id order (the fleet is sorted by id before
// evals are built), handing each lane to the next vector in the rotation that
// can fly it.
func roundRobin(evals [][]eval, fleet []Vector) []int {
	winners := make([]int, len(evals))
	used := make([]bool, len(fleet))
	next := 0
	for li, row := range evals {
		winners[li] = -1
		for step := range len(row) {
			vi := (next + step) % len(row)
			if row[vi].rejected || used[vi] {
				continue
			}
			winners[li] = vi
			used[vi] = true
			next = vi + 1
			break
		}
	}
	return winners
}

// forbidden is the cost of a pair the solver must avoid: larger than any real
// total, small enough that sixteen of them cannot overflow an int64.
const forbidden int64 = 1 << 50

// lowestCost solves the round exactly: each lane to a distinct vector,
// minimising the summed range needed, in integer millimetres. Infeasible
// pairs cost forbidden, and dummy columns pad the matrix when lanes outnumber
// vectors, so the solver first maximises the number of real assignments and
// then minimises their cost. A lane left on a forbidden pair stays unassigned.
func lowestCost(evals [][]eval) []int {
	winners := make([]int, len(evals))
	if len(evals) == 0 {
		return winners
	}
	cols := max(len(evals[0]), len(evals))
	matrix := make([][]int64, len(evals))
	for li, row := range evals {
		matrix[li] = make([]int64, cols)
		for vi := range cols {
			matrix[li][vi] = forbidden
			if vi < len(row) && !row[vi].rejected {
				matrix[li][vi] = int64(math.Round(row[vi].needM * 1000))
			}
		}
	}
	for li, vi := range hungarian(matrix) {
		winners[li] = -1
		if matrix[li][vi] < forbidden {
			winners[li] = vi
		}
	}
	return winners
}

// assignment builds the trace of one lane: every vector as a candidate, each
// loser with the reason it lost.
func assignment(policy domain.AssignPolicy, l domain.Lane, lanes []domain.Lane, fleet []Vector, row []eval, winner int, laneOf []int) Assignment {
	a := Assignment{Lane: l.ID}
	cands := make([]domain.Candidate, len(fleet))
	for vi, v := range fleet {
		e := row[vi]
		c := domain.Candidate{Vector: v.Caps.ID, Cost: cost(policy, e)}
		switch {
		case vi == winner:
		case e.rejected:
			c.Rejected, c.Reason = true, e.reason
		case laneOf[vi] >= 0:
			c.Rejected, c.Reason = true, "assigned "+string(lanes[laneOf[vi]].ID)+" in this round"
		case winner < 0:
			// Every policy gives a lane to a free eligible vector when one
			// exists, so this is defensive: the trace still names a reason.
			c.Rejected, c.Reason = true, "not selected"
		case policy == domain.PolicyRoundRobin:
			c.Rejected, c.Reason = true, "not next in the rotation"
		case policy == domain.PolicyLowestCost:
			c.Rejected, c.Reason = true, "not in the minimum total cost assignment"
		default:
			c.Rejected, c.Reason = true, fmt.Sprintf("farther than %s: %.0f m vs %.0f m", fleet[winner].Caps.ID, e.transitM, row[winner].transitM)
		}
		cands[vi] = c
	}
	a.Candidates = domain.SortCandidates(cands)

	if winner < 0 {
		a.Rationale = fmt.Sprintf("no vector can take %s: all %d candidate(s) rejected", l.ID, len(fleet))
		return a
	}
	v, e := fleet[winner], row[winner]
	a.Winner = v.Caps.ID
	switch policy {
	case domain.PolicyRoundRobin:
		a.Rationale = fmt.Sprintf("%s is next in the round-robin rotation for %s", v.Caps.ID, l.ID)
	case domain.PolicyLowestCost:
		a.Rationale = fmt.Sprintf("%s takes %s in the minimum total cost assignment", v.Caps.ID, l.ID)
	default:
		a.Rationale = fmt.Sprintf("%s is the nearest capable vector to %s at %.0f m", v.Caps.ID, l.ID, e.transitM)
	}
	a.Rationale += fmt.Sprintf(", needing %.0f of %.0f m range", e.needM, e.rangeM)
	if len(v.Holds) > 0 {
		a.Rationale += ", queued after " + string(v.Holds[len(v.Holds)-1])
	}
	return a
}
