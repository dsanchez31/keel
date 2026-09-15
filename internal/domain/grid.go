package domain

import "math"

// CellID indexes a cell of the AO raster in row-major order, so the natural
// numeric ordering is a total order and "sorted by CellID" needs no comparator.
type CellID uint32

// Grid is the raster covering an AO.
//
// Origin is the south-west corner of cell (0, 0). CellM is the edge length in
// metres, projected at the AO's reference latitude and held fixed for the whole
// mission. InAO and Explored are dense and parallel: index row*Cols+col.
type Grid struct {
	Origin   Position `json:"origin"`
	RefLat   float64  `json:"ref_lat"`
	CellM    float64  `json:"cell_m"`
	Cols     int      `json:"cols"`
	Rows     int      `json:"rows"`
	InAO     []bool   `json:"in_ao"`
	Explored []bool   `json:"explored"`
}

// Count is the number of cells in the raster, in the AO or not.
func (g Grid) Count() int { return g.Cols * g.Rows }

// Valid reports whether id addresses a cell of this grid.
func (g Grid) Valid(id CellID) bool { return int(id) < g.Count() }

// RowCol splits a cell id into its raster coordinates.
func (g Grid) RowCol(id CellID) (row, col int) {
	if g.Cols == 0 {
		return 0, 0
	}
	return int(id) / g.Cols, int(id) % g.Cols
}

// CellAt is the inverse of RowCol.
func (g Grid) CellAt(row, col int) CellID { return CellID(row*g.Cols + col) }

// Centre is the WGS84 position of the centre of a cell.
func (g Grid) Centre(id CellID) Position {
	row, col := g.RowCol(id)
	mLat := MetresPerDegreeLat()
	mLon := MetresPerDegreeLon(g.RefLat)
	return Position{
		Lat: g.Origin.Lat + (float64(row)+0.5)*g.CellM/mLat,
		Lon: g.Origin.Lon + (float64(col)+0.5)*g.CellM/mLon,
	}
}

// CellOf locates the cell containing a position, or false when the position
// falls outside the raster.
func (g Grid) CellOf(p Position) (CellID, bool) {
	mLat := MetresPerDegreeLat()
	mLon := MetresPerDegreeLon(g.RefLat)
	row := int(math.Floor((p.Lat - g.Origin.Lat) * mLat / g.CellM))
	col := int(math.Floor((p.Lon - g.Origin.Lon) * mLon / g.CellM))
	if row < 0 || col < 0 || row >= g.Rows || col >= g.Cols {
		return 0, false
	}
	return g.CellAt(row, col), true
}

// IsInAO reports whether the cell centre lies inside the AO polygon.
func (g Grid) IsInAO(id CellID) bool {
	return g.Valid(id) && g.InAO[id]
}

// IsExplored reports whether the cell has been covered by a sensor footprint.
func (g Grid) IsExplored(id CellID) bool {
	return g.Valid(id) && g.Explored[id]
}

// Clone deep-copies the mutable bitmaps so a State copy cannot alias coverage
// with the state it was copied from. Aliasing here would break I2 in a way
// that only shows up under replay.
func (g Grid) Clone() Grid {
	out := g
	out.InAO = append([]bool(nil), g.InAO...)
	out.Explored = append([]bool(nil), g.Explored...)
	return out
}

// Coverage reports explored and total cell counts restricted to the AO.
// Counting is an integer reduction over a dense slice in index order, so it is
// order independent by construction.
func (g Grid) Coverage() (explored, total int) {
	for i := range g.InAO {
		if !g.InAO[i] {
			continue
		}
		total++
		if g.Explored[i] {
			explored++
		}
	}
	return explored, total
}

// CoveragePct is explored cells over AO cells, in [0, 1]. Zero cells reports 1,
// because an empty AO is trivially covered and reporting 0 would deadlock I8.
func (g Grid) CoveragePct() float64 {
	explored, total := g.Coverage()
	if total == 0 {
		return 1
	}
	return float64(explored) / float64(total)
}

// Area is an AO: its name, its polygon and the raster covering it.
type Area struct {
	Name    string  `json:"name"`
	Polygon Polygon `json:"polygon"`
	Grid    Grid    `json:"grid"`
}

// Clone deep-copies the grid bitmaps. The polygon is immutable once built.
func (a Area) Clone() Area {
	out := a
	out.Grid = a.Grid.Clone()
	return out
}
