// Package coverage owns the geometry of a mission: the raster covering the
// AO, the fog of war painted onto it, the decomposition of the AO into lanes
// and the redecomposition of what is left when a vector is lost.
//
// It is in the decision path. Everything here is a pure function over its
// arguments: no I/O, no clock, no randomness, no map iteration. Given the same
// polygon and the same tactic, Decompose produces the same lanes and the same
// waypoints on every run, which is what lets an approved plan be regenerated
// and re-hashed instead of trusted.
package coverage

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/dsanchez31/keel/internal/domain"
)

// MaxCells bounds the raster. A cell size that is tiny against the AO would
// otherwise allocate without limit, and the allocation would happen inside the
// decision path.
const MaxCells = 1 << 22

var (
	// ErrInvalidArea reports a polygon or a cell size a raster cannot be
	// built from.
	ErrInvalidArea = errors.New("coverage: invalid area")
	// ErrDisconnectedAO reports an AO whose in-AO cells do not form one
	// 4-connected region. Route repair needs a path between any two in-AO
	// cells, so a disconnected raster is refused where it is built rather than
	// discovered halfway through a mission.
	ErrDisconnectedAO = errors.New("coverage: AO raster is not connected")
)

// NewGrid rasterises an AO polygon into square cells of cellM metres.
//
// The origin is the south-west corner of the polygon's bounding box and the
// reference latitude is the middle of that box, so neither depends on which
// vertex the ring happens to start from. A cell is in the AO when its centre
// lies inside the polygon.
func NewGrid(name string, poly domain.Polygon, cellM float64) (domain.Area, error) {
	if len(poly.Ring) < 3 {
		return domain.Area{}, fmt.Errorf("%w: polygon has %d vertices, need at least 3", ErrInvalidArea, len(poly.Ring))
	}
	for i, v := range poly.Ring {
		if !v.Finite() {
			return domain.Area{}, fmt.Errorf("%w: vertex %d is not finite", ErrInvalidArea, i)
		}
	}
	if !(cellM > 0) || math.IsInf(cellM, 0) {
		return domain.Area{}, fmt.Errorf("%w: cell size %v m is not a finite positive number", ErrInvalidArea, cellM)
	}

	minLat, minLon, maxLat, maxLon := poly.BBox()
	refLat := (minLat + maxLat) / 2
	mLat := domain.MetresPerDegreeLat()
	mLon := domain.MetresPerDegreeLon(refLat)
	rowsF := math.Ceil((maxLat - minLat) * mLat / cellM)
	colsF := math.Ceil((maxLon - minLon) * mLon / cellM)
	if rowsF < 1 || colsF < 1 {
		return domain.Area{}, fmt.Errorf("%w: polygon has no extent", ErrInvalidArea)
	}
	if rowsF*colsF > MaxCells {
		return domain.Area{}, fmt.Errorf("%w: %v x %v cells exceeds the %d cell bound", ErrInvalidArea, rowsF, colsF, MaxCells)
	}
	rows, cols := int(rowsF), int(colsF)

	g := domain.Grid{
		Origin:   domain.Position{Lat: minLat, Lon: minLon},
		RefLat:   refLat,
		CellM:    cellM,
		Rows:     rows,
		Cols:     cols,
		InAO:     make([]bool, rows*cols),
		Explored: make([]bool, rows*cols),
	}
	inAO := 0
	for i := range g.InAO {
		if poly.Contains(g.Centre(domain.CellID(i))) {
			g.InAO[i] = true
			inAO++
		}
	}
	if inAO == 0 {
		return domain.Area{}, fmt.Errorf("%w: no cell centre lies inside the polygon at %v m cells", ErrInvalidArea, cellM)
	}
	if reached := reachable(g, firstInAO(g)); reached != inAO {
		return domain.Area{}, fmt.Errorf("%w: %d of %d cells reachable from cell %d", ErrDisconnectedAO, reached, inAO, firstInAO(g))
	}

	return domain.Area{
		Name:    name,
		Polygon: domain.Polygon{Ring: slices.Clone(poly.Ring)},
		Grid:    g,
	}, nil
}

// Paint marks every AO cell whose centre lies within radiusM of p as
// explored, and returns how many cells it newly explored.
//
// It mutates g.Explored in place: the caller owns a clone (engine.Step clones
// its state before painting). Coverage only ever goes from false to true,
// which is invariant I2 held by construction rather than by assertion. Cells
// are visited in ascending row then column order over a bounded window around
// p, so the work is a function of the position and nothing else.
func Paint(g domain.Grid, p domain.Position, radiusM float64) int {
	if g.Count() == 0 || !(radiusM > 0) {
		return 0
	}
	centre, ok := g.CellOf(p)
	if !ok {
		return 0
	}
	span := int(math.Ceil(radiusM / g.CellM))
	row0, col0 := g.RowCol(centre)
	painted := 0
	for dr := -span; dr <= span; dr++ {
		row := row0 + dr
		if row < 0 || row >= g.Rows {
			continue
		}
		for dc := -span; dc <= span; dc++ {
			col := col0 + dc
			if col < 0 || col >= g.Cols {
				continue
			}
			id := g.CellAt(row, col)
			if !g.InAO[id] || g.Explored[id] {
				continue
			}
			if domain.HaversineM(p, g.Centre(id)) <= radiusM {
				g.Explored[id] = true
				painted++
			}
		}
	}
	return painted
}

// Uncovered lists the AO cells not yet explored, in ascending order. It is the
// fog of war.
func Uncovered(g domain.Grid) []domain.CellID {
	var out []domain.CellID
	for i := range g.InAO {
		if g.InAO[i] && !g.Explored[i] {
			out = append(out, domain.CellID(i))
		}
	}
	return out
}

// LaneCoverage counts the explored cells of a lane and the lane's AO cells.
// Integer counting over a sorted set, so it is order independent.
func LaneCoverage(g domain.Grid, l domain.Lane) (explored, total int) {
	for _, id := range l.Cells {
		if !g.IsInAO(id) {
			continue
		}
		total++
		if g.Explored[id] {
			explored++
		}
	}
	return explored, total
}

// uncoveredOf filters a sorted cell set down to its unexplored AO cells,
// preserving order.
func uncoveredOf(g domain.Grid, cells []domain.CellID) []domain.CellID {
	var out []domain.CellID
	for _, id := range cells {
		if g.IsInAO(id) && !g.Explored[id] {
			out = append(out, id)
		}
	}
	return out
}

// firstInAO is the lowest in-AO cell, or 0 when there is none.
func firstInAO(g domain.Grid) domain.CellID {
	for i, in := range g.InAO {
		if in {
			return domain.CellID(i)
		}
	}
	return 0
}

// reachable counts the in-AO cells 4-connected to start, start included.
func reachable(g domain.Grid, start domain.CellID) int {
	if !g.IsInAO(start) {
		return 0
	}
	seen := make([]bool, g.Count())
	seen[start] = true
	queue := []domain.CellID{start}
	count := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		count++
		row, col := g.RowCol(id)
		for _, d := range orthogonal {
			r, c := row+d[0], col+d[1]
			if r < 0 || c < 0 || r >= g.Rows || c >= g.Cols {
				continue
			}
			n := g.CellAt(r, c)
			if g.InAO[n] && !seen[n] {
				seen[n] = true
				queue = append(queue, n)
			}
		}
	}
	return count
}

// orthogonal is the fixed neighbour order for 4-connected walks: north, east,
// south, west, as (row, column) offsets.
var orthogonal = [4][2]int{{1, 0}, {0, 1}, {-1, 0}, {0, -1}}
