package coverage

import (
	"errors"
	"math"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
)

// Test AOs are written in local metres east and north of a fixed origin and
// converted to WGS84 once, so a shape reads as the shape it is.

const (
	testOriginLat = 45.0
	testOriginLon = 5.0
	testCellM     = 50.0
)

// polyMetres converts a ring of (east, north) metre offsets into a WGS84
// polygon anchored at the test origin.
func polyMetres(pts ...[2]float64) domain.Polygon {
	mLat := domain.MetresPerDegreeLat()
	mLon := domain.MetresPerDegreeLon(testOriginLat)
	ring := make([]domain.Position, len(pts))
	for i, p := range pts {
		ring[i] = domain.Position{
			Lat: testOriginLat + p[1]/mLat,
			Lon: testOriginLon + p[0]/mLon,
		}
	}
	return domain.Polygon{Ring: ring}
}

// rectArea is the phase 1 test rectangle, roughly 945 m by 1000 m.
func rectArea(t *testing.T) domain.Area {
	t.Helper()
	poly := domain.Polygon{Ring: []domain.Position{
		{Lat: 45.0000, Lon: 5.0000},
		{Lat: 45.0000, Lon: 5.0120},
		{Lat: 45.0090, Lon: 5.0120},
		{Lat: 45.0090, Lon: 5.0000},
	}}
	return mustGrid(t, "rect", poly, testCellM)
}

func mustGrid(t *testing.T, name string, poly domain.Polygon, cellM float64) domain.Area {
	t.Helper()
	a, err := NewGrid(name, poly, cellM)
	if err != nil {
		t.Fatalf("NewGrid(%s): %v", name, err)
	}
	return a
}

func TestNewGridRaster(t *testing.T) {
	a := rectArea(t)
	g := a.Grid

	minLat, minLon, maxLat, maxLon := a.Polygon.BBox()
	if g.Origin.Lat != minLat || g.Origin.Lon != minLon {
		t.Fatalf("origin %+v, want the south-west bbox corner", g.Origin)
	}
	if g.RefLat != (minLat+maxLat)/2 {
		t.Fatalf("ref lat %v, want the bbox middle", g.RefLat)
	}
	wantRows := int(math.Ceil((maxLat - minLat) * domain.MetresPerDegreeLat() / testCellM))
	wantCols := int(math.Ceil((maxLon - minLon) * domain.MetresPerDegreeLon(g.RefLat) / testCellM))
	if g.Rows != wantRows || g.Cols != wantCols {
		t.Fatalf("raster %dx%d, want %dx%d", g.Rows, g.Cols, wantRows, wantCols)
	}
	for i := range g.InAO {
		if g.InAO[i] != a.Polygon.Contains(g.Centre(domain.CellID(i))) {
			t.Fatalf("cell %d: InAO %v disagrees with the polygon", i, g.InAO[i])
		}
		if g.Explored[i] {
			t.Fatalf("cell %d explored on a fresh grid", i)
		}
	}
	if _, total := g.Coverage(); total == 0 {
		t.Fatal("no cell in the AO")
	}
}

// The ring's start vertex must not change the raster.
func TestNewGridIndependentOfRingStart(t *testing.T) {
	a := rectArea(t)
	rotated := append(append([]domain.Position(nil), a.Polygon.Ring[2:]...), a.Polygon.Ring[:2]...)
	b := mustGrid(t, "rect", domain.Polygon{Ring: rotated}, testCellM)
	if a.Grid.Origin != b.Grid.Origin || a.Grid.RefLat != b.Grid.RefLat ||
		a.Grid.Rows != b.Grid.Rows || a.Grid.Cols != b.Grid.Cols {
		t.Fatalf("raster changed with the ring start: %+v vs %+v", a.Grid.Origin, b.Grid.Origin)
	}
	for i := range a.Grid.InAO {
		if a.Grid.InAO[i] != b.Grid.InAO[i] {
			t.Fatalf("cell %d differs with the ring start", i)
		}
	}
}

func TestNewGridErrors(t *testing.T) {
	square := polyMetres([2]float64{0, 0}, [2]float64{500, 0}, [2]float64{500, 500}, [2]float64{0, 500})
	nan := polyMetres([2]float64{0, 0}, [2]float64{500, 0}, [2]float64{500, 500})
	nan.Ring[1].Lat = math.NaN()

	// Two 300 m squares joined by a 10 m corridor that no cell centre falls
	// inside: the polygon is connected, its raster is not.
	dumbbell := polyMetres(
		[2]float64{0, 0}, [2]float64{300, 0}, [2]float64{300, 145}, [2]float64{700, 145},
		[2]float64{700, 0}, [2]float64{1000, 0}, [2]float64{1000, 300}, [2]float64{700, 300},
		[2]float64{700, 155}, [2]float64{300, 155}, [2]float64{300, 300}, [2]float64{0, 300},
	)

	cases := []struct {
		name  string
		poly  domain.Polygon
		cellM float64
		want  error
	}{
		{"two vertices", domain.Polygon{Ring: square.Ring[:2]}, testCellM, ErrInvalidArea},
		{"nan vertex", nan, testCellM, ErrInvalidArea},
		{"zero cell", square, 0, ErrInvalidArea},
		{"negative cell", square, -1, ErrInvalidArea},
		{"infinite cell", square, math.Inf(1), ErrInvalidArea},
		{"too many cells", square, 0.1, ErrInvalidArea},
		{"cell larger than the AO", square, 5000, ErrInvalidArea},
		{"disconnected raster", dumbbell, testCellM, ErrDisconnectedAO},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewGrid(tc.name, tc.poly, tc.cellM)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPaint(t *testing.T) {
	a := rectArea(t)
	g := a.Grid.Clone()
	centre := g.Centre(g.CellAt(g.Rows/2, g.Cols/2))

	n := Paint(g, centre, 120)
	if n == 0 {
		t.Fatal("painting the middle of the AO explored nothing")
	}
	for i := range g.InAO {
		id := domain.CellID(i)
		want := g.InAO[i] && domain.HaversineM(centre, g.Centre(id)) <= 120
		if g.Explored[i] != want {
			t.Fatalf("cell %d explored %v, want %v", i, g.Explored[i], want)
		}
	}
	if again := Paint(g, centre, 120); again != 0 {
		t.Fatalf("repainting the same footprint explored %d new cells", again)
	}

	explored, _ := g.Coverage()
	if explored != n {
		t.Fatalf("coverage %d, paint reported %d", explored, n)
	}
	if got := len(Uncovered(g)); got != countInAO(g)-n {
		t.Fatalf("uncovered %d, want %d", got, countInAO(g)-n)
	}

	if Paint(g, centre, 0) != 0 || Paint(g, domain.Position{Lat: 10, Lon: 10}, 120) != 0 {
		t.Fatal("a zero radius or an off-raster position painted something")
	}
	if a.Grid.Explored[g.CellAt(g.Rows/2, g.Cols/2)] {
		t.Fatal("painting a clone changed the original")
	}
}

func TestLaneCoverage(t *testing.T) {
	a := rectArea(t)
	g := a.Grid.Clone()
	lane := domain.Lane{Cells: []domain.CellID{0, 1, 2, 3}}
	g.Explored[1] = true
	g.Explored[3] = true
	explored, total := LaneCoverage(g, lane)
	if explored != 2 || total != 4 {
		t.Fatalf("lane coverage %d/%d, want 2/4", explored, total)
	}
	if got := uncoveredOf(g, lane.Cells); len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("uncovered of lane %v, want [0 2]", got)
	}
}

func countInAO(g domain.Grid) int {
	n := 0
	for _, in := range g.InAO {
		if in {
			n++
		}
	}
	return n
}
