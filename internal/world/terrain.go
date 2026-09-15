package world

import (
	"errors"
	"fmt"
	"math"

	"github.com/dsanchez31/keel/internal/domain"
)

// ErrUnreachable reports a ground destination no traversable path leads to.
var ErrUnreachable = errors.New("world: destination unreachable over the terrain")

// maxTerrainCells bounds the traversability grid, so a scenario with a tiny
// cell over a large extent is refused rather than allocating without limit.
const maxTerrainCells = 1 << 22

// terrainMarginM is how far the traversability grid extends beyond every
// position the scenario mentions, so a ground vehicle can go around a blocked
// area that touches the edge of the AO.
const terrainMarginM = 1000

// Terrain is the traversability grid ground vehicles route over: square
// cells in a local planar frame, open unless their centre lies in a blocked
// polygon. A position outside the grid is open ground the scenario says
// nothing about, crossed in a straight line.
type Terrain struct {
	origin     domain.Position // south-west corner
	refLat     float64
	cellM      float64
	rows, cols int
	open       []bool
}

// NewTerrain rasterises the blocked areas over the bounding box of extent,
// widened by a margin. With no blocked area it returns nil: every route is a
// straight line.
func NewTerrain(spec TerrainSpec, extent []domain.Position) (*Terrain, error) {
	if len(spec.Blocked) == 0 {
		return nil, nil
	}
	pts := append([]domain.Position(nil), extent...)
	for _, b := range spec.Blocked {
		pts = append(pts, b.Polygon.Ring...)
	}
	minLat, minLon := math.Inf(1), math.Inf(1)
	maxLat, maxLon := math.Inf(-1), math.Inf(-1)
	for _, p := range pts {
		minLat, maxLat = math.Min(minLat, p.Lat), math.Max(maxLat, p.Lat)
		minLon, maxLon = math.Min(minLon, p.Lon), math.Max(maxLon, p.Lon)
	}
	refLat := (minLat + maxLat) / 2
	mLat, mLon := domain.MetresPerDegreeLat(), domain.MetresPerDegreeLon(refLat)
	t := &Terrain{
		origin: domain.Position{Lat: minLat - terrainMarginM/mLat, Lon: minLon - terrainMarginM/mLon},
		refLat: refLat,
		cellM:  spec.CellM,
	}
	t.rows = int(math.Ceil(float64((maxLat-minLat)*mLat)/t.cellM + 2*terrainMarginM/t.cellM))
	t.cols = int(math.Ceil(float64((maxLon-minLon)*mLon)/t.cellM + 2*terrainMarginM/t.cellM))
	if t.rows <= 0 || t.cols <= 0 || t.rows*t.cols > maxTerrainCells {
		return nil, fmt.Errorf("%w: terrain grid of %d by %d cells", ErrInvalidScenario, t.rows, t.cols)
	}
	t.open = make([]bool, t.rows*t.cols)
	for i := range t.open {
		c := t.centre(i)
		t.open[i] = true
		for _, b := range spec.Blocked {
			if b.Polygon.Contains(c) {
				t.open[i] = false
				break
			}
		}
	}
	return t, nil
}

// cellOf returns the index of the cell holding p, false outside the grid.
func (t *Terrain) cellOf(p domain.Position) (int, bool) {
	north := float64((p.Lat - t.origin.Lat) * domain.MetresPerDegreeLat())
	east := float64((p.Lon - t.origin.Lon) * domain.MetresPerDegreeLon(t.refLat))
	row, col := int(math.Floor(north/t.cellM)), int(math.Floor(east/t.cellM))
	if row < 0 || col < 0 || row >= t.rows || col >= t.cols {
		return 0, false
	}
	return row*t.cols + col, true
}

func (t *Terrain) centre(i int) domain.Position {
	row, col := i/t.cols, i%t.cols
	return domain.Position{
		Lat: t.origin.Lat + float64((float64(row)+0.5)*t.cellM)/domain.MetresPerDegreeLat(),
		Lon: t.origin.Lon + float64((float64(col)+0.5)*t.cellM)/domain.MetresPerDegreeLon(t.refLat),
	}
}

// Open reports whether a ground vehicle may be at p. Outside the grid the
// scenario says nothing, so the ground is open.
func (t *Terrain) Open(p domain.Position) bool {
	if t == nil {
		return true
	}
	i, ok := t.cellOf(p)
	return !ok || t.open[i]
}

// neighbours8 is the fixed neighbour order, orthogonal first: N, E, S, W,
// NE, SE, SW, NW, the order coverage routes with. A fixed order is what makes
// the shortest path chosen among equals the same on every run.
var neighbours8 = [8][2]int{{1, 0}, {0, 1}, {-1, 0}, {0, -1}, {1, 1}, {-1, 1}, {-1, -1}, {1, -1}}

// Route returns the points a ground vehicle drives through from from to to,
// ending exactly on to. It is the shortest 8-connected path over open cells,
// a diagonal step allowed only where both cells it cuts between are open,
// then shortened by line of sight. The start cell counts as open, so a
// vehicle placed on blocked ground can leave it. A destination on blocked
// ground, or cut off from the start, is ErrUnreachable.
func (t *Terrain) Route(from, to domain.Position) ([]domain.Position, error) {
	if t == nil {
		return []domain.Position{to}, nil
	}
	start, sok := t.cellOf(from)
	goal, gok := t.cellOf(to)
	if !gok || !sok {
		// Beyond the grid the scenario blocks nothing: a straight line,
		// unless the line itself crosses a blocked cell.
		if t.lineOpen(from, to, -1) {
			return []domain.Position{to}, nil
		}
		return nil, fmt.Errorf("%w: %v to %v leaves the terrain grid across blocked ground", ErrUnreachable, from, to)
	}
	if !t.open[goal] {
		return nil, fmt.Errorf("%w: %v is on blocked ground", ErrUnreachable, to)
	}
	if start == goal || t.lineOpen(from, to, start) {
		return []domain.Position{to}, nil
	}

	prev := make([]int32, len(t.open))
	for i := range prev {
		prev[i] = -1
	}
	prev[start] = int32(start)
	queue := []int{start}
	for len(queue) > 0 && prev[goal] < 0 {
		cur := queue[0]
		queue = queue[1:]
		row, col := cur/t.cols, cur%t.cols
		for _, d := range neighbours8 {
			r, c := row+d[0], col+d[1]
			if !t.passable(r, c, start) {
				continue
			}
			if d[0] != 0 && d[1] != 0 && (!t.passable(row+d[0], col, start) || !t.passable(row, col+d[1], start)) {
				continue
			}
			n := r*t.cols + c
			if prev[n] >= 0 {
				continue
			}
			prev[n] = int32(cur)
			queue = append(queue, n)
		}
	}
	if prev[goal] < 0 {
		return nil, fmt.Errorf("%w: no open path from %v to %v", ErrUnreachable, from, to)
	}

	var cells []int
	for c := goal; c != start; c = int(prev[c]) {
		cells = append(cells, c)
	}
	// cells runs goal to start; walk it backwards, keeping only the cells
	// line of sight cannot skip.
	var out []domain.Position
	at, atCell := from, start
	for i := len(cells) - 1; i > 0; i-- {
		next := t.centre(cells[i-1])
		if !t.lineOpen(at, next, atCell) {
			at, atCell = t.centre(cells[i]), cells[i]
			out = append(out, at)
		}
	}
	if !t.lineOpen(at, to, atCell) {
		out = append(out, t.centre(goal))
	}
	return append(out, to), nil
}

func (t *Terrain) passable(row, col, start int) bool {
	if row < 0 || col < 0 || row >= t.rows || col >= t.cols {
		return false
	}
	i := row*t.cols + col
	return t.open[i] || i == start
}

// lineOpen reports whether the straight segment from a to b crosses only open
// ground, sampling every quarter cell. The cell except may be blocked: it is
// the start cell a vehicle is leaving.
func (t *Terrain) lineOpen(a, b domain.Position, except int) bool {
	e, n := enu(a, b)
	d := math.Hypot(e, n)
	steps := int(math.Ceil(d / (t.cellM / 4)))
	for k := 0; k <= steps; k++ {
		f := 1.0
		if steps > 0 {
			f = float64(k) / float64(steps)
		}
		p := offset(a, float64(e*f), float64(n*f))
		if i, ok := t.cellOf(p); ok && !t.open[i] && i != except {
			return false
		}
	}
	return true
}

// enu returns b relative to a in metres east and north, equirectangular at a's
// latitude. Over the distances a vehicle covers in one tick, or a ground route
// covers between two of its points, the error is far below a cell.
func enu(a, b domain.Position) (east, north float64) {
	east = float64((b.Lon - a.Lon) * domain.MetresPerDegreeLon(a.Lat))
	north = float64((b.Lat - a.Lat) * domain.MetresPerDegreeLat())
	return east, north
}

// offset moves a by east and north metres, in the same frame as enu.
func offset(a domain.Position, east, north float64) domain.Position {
	return domain.Position{
		Lat:  a.Lat + north/domain.MetresPerDegreeLat(),
		Lon:  a.Lon + east/domain.MetresPerDegreeLon(a.Lat),
		AltM: a.AltM,
	}
}
