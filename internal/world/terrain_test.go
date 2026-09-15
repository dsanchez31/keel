package world

import (
	"errors"
	"reflect"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
)

// rect is a blocked rectangle from (e0, n0) to (e1, n1) metres.
func rect(name string, e0, n0, e1, n1 float64) BlockedArea {
	return BlockedArea{Name: name, Polygon: domain.Polygon{Ring: []domain.Position{
		at(e0, n0, 0), at(e1, n0, 0), at(e1, n1, 0), at(e0, n1, 0),
	}}}
}

// wall is a terrain with a wall 400 m north of the origin, east to west from
// -1000 m to 1000 m, with a 100 m gap at 600 m east.
func wall(t *testing.T) *Terrain {
	t.Helper()
	tr, err := NewTerrain(TerrainSpec{CellM: 25, Blocked: []BlockedArea{
		rect("wall-west", -1000, 400, 550, 450),
		rect("wall-east", 650, 400, 1000, 450),
	}}, []domain.Position{at(-1000, -500, 0), at(1000, 1500, 0)})
	if err != nil {
		t.Fatalf("terrain: %v", err)
	}
	return tr
}

func TestNoBlockedAreaMeansStraightLines(t *testing.T) {
	tr, err := NewTerrain(TerrainSpec{CellM: 25}, []domain.Position{at(0, 0, 0)})
	if err != nil || tr != nil {
		t.Fatalf("terrain %v err %v, want none", tr, err)
	}
	route, err := tr.Route(at(0, 0, 0), at(500, 500, 0))
	if err != nil || len(route) != 1 || route[0] != at(500, 500, 0) {
		t.Fatalf("route %v err %v, want the destination alone", route, err)
	}
}

func TestRouteGoesThroughTheGap(t *testing.T) {
	tr := wall(t)
	from, to := at(0, 0, 0), at(0, 900, 0)
	route, err := tr.Route(from, to)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if route[len(route)-1] != to {
		t.Fatalf("route ends at %v, want %v", route[len(route)-1], to)
	}
	prev := from
	through := false
	for _, p := range route {
		if !tr.lineOpen(prev, p, -1) {
			t.Fatalf("segment %v to %v crosses the wall", prev, p)
		}
		if e, n := enu(at(0, 0, 0), p); e > 540 && e < 660 && n > 350 && n < 500 {
			through = true
		}
		prev = p
	}
	if !through {
		t.Fatalf("route %v does not pass through the gap", route)
	}
	if len(route) > 6 {
		t.Fatalf("route of %d points, line of sight should leave a handful", len(route))
	}

	again, _ := tr.Route(from, to)
	if !reflect.DeepEqual(route, again) {
		t.Fatal("the same route request gave two routes")
	}
}

func TestUnreachableDestinations(t *testing.T) {
	tr := wall(t)
	if _, err := tr.Route(at(0, 0, 0), at(0, 425, 0)); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("destination inside the wall: err %v", err)
	}
	boxed, err := NewTerrain(TerrainSpec{CellM: 25, Blocked: []BlockedArea{
		rect("n", -200, 100, 200, 200), rect("s", -200, -200, 200, -100),
		rect("e", 100, -200, 200, 200), rect("w", -200, -200, -100, 200),
	}}, []domain.Position{at(-500, -500, 0), at(500, 500, 0)})
	if err != nil {
		t.Fatalf("terrain: %v", err)
	}
	if _, err := boxed.Route(at(0, 0, 0), at(400, 400, 0)); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("start boxed in: err %v", err)
	}
	if !boxed.Open(at(0, 0, 0)) || boxed.Open(at(150, 0, 0)) {
		t.Fatal("open and blocked cells swapped")
	}
}
