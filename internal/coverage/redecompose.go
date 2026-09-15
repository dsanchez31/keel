package coverage

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/dsanchez31/keel/internal/domain"
)

// ErrNoEligibleVector reports uncovered cells to redistribute and no vector
// to redistribute them to.
var ErrNoEligibleVector = errors.New("coverage: no vector to redistribute to")

// Result is a redecomposition: what stays, what goes and what replaces it.
type Result struct {
	// Kept are the assigned lanes, untouched, in their original order.
	Kept []domain.Lane `json:"kept"`
	// Retired are the unassigned lanes whose uncovered cells were pooled, in
	// their original order. Their explored cells stay explored (I2); the
	// lanes themselves are gone.
	Retired []domain.LaneID `json:"retired,omitempty"`
	// New are the lanes the pool was cut into, unassigned, ordered by Index.
	New []domain.Lane `json:"new,omitempty"`
	// Pool is the set of cells redistributed, sorted.
	Pool []domain.CellID `json:"pool,omitempty"`
}

// Redecompose pools the uncovered cells of every unassigned lane and cuts the
// pool into at most k new lanes, one per vector the caller can hand them to.
//
// Assigned lanes are left exactly as they are, so a vector already working
// keeps its lane and its cursor. The pool is cut across its own long axis
// into strips holding equal numbers of cells (ordered by position across the
// axis, then CellID), so no new lane is empty and the work is balanced. New
// lanes are swept by the same pass builder and route repair as Decompose.
//
// New lanes are numbered from first, or after the highest index among lanes
// when that is higher. The caller passes the first index never used in the
// mission: a retired lane is gone from lanes, and numbering after the lanes
// alone would hand its id to a different lane of the same mission.
//
// Every new lane visits the centre of at least one pooled cell, which the
// sensor footprint of the vector flying it then explores. Each round
// therefore strictly shrinks the fog, and repeated redecomposition converges
// instead of reshuffling the same cells: the livelock that I1 and I3 together
// forbid.
func Redecompose(g domain.Grid, lanes []domain.Lane, k, first int, p Params) (Result, error) {
	if err := validateParams(p); err != nil {
		return Result{}, err
	}

	var res Result
	next := max(first, 0)
	for _, l := range lanes {
		next = max(next, l.Index+1)
		if l.Assigned() {
			res.Kept = append(res.Kept, l.Clone())
			continue
		}
		res.Retired = append(res.Retired, l.ID)
		res.Pool = append(res.Pool, uncoveredOf(g, l.Cells)...)
	}
	slices.Sort(res.Pool)
	res.Pool = slices.Compact(res.Pool)
	if len(res.Pool) == 0 {
		return res, nil
	}
	if k < 1 {
		return Result{}, fmt.Errorf("%w: %d uncovered cell(s) pooled", ErrNoEligibleVector, len(res.Pool))
	}
	k = min(k, len(res.Pool), MaxLanes)

	centres := make([]vec, len(res.Pool))
	for i, id := range res.Pool {
		centres[i] = cellLocal(g, id)
	}
	f := newSweepFrame(longAxis(centres))
	cells := projectCells(g, f, res.Pool)
	slices.SortFunc(cells, func(a, b uvCell) int {
		if c := cmp.Compare(a.u, b.u); c != 0 {
			return c
		}
		return cmp.Compare(a.id, b.id)
	})

	r := newRouter(g)
	base, extra := len(cells)/k, len(cells)%k
	start := 0
	for i := range k {
		size := base
		if i < extra {
			size++
		}
		chunk := cells[start : start+size]
		start += size

		owned := make([]domain.CellID, len(chunk))
		for j, c := range chunk {
			owned[j] = c.id
		}
		l, err := buildLane(r, f, next+i, owned, chunk, chunk[0].u, chunk[len(chunk)-1].u, p)
		if err != nil {
			return Result{}, err
		}
		res.New = append(res.New, l)
	}
	return res, nil
}
