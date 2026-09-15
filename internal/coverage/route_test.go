package coverage

import (
	"errors"
	"math"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
)

// notched is a 5 by 5 raster of 50 m cells, all in the AO but the one at row
// 2, column 2, and everything beyond the raster.
func notched() domain.Grid {
	g := domain.Grid{CellM: 50, Rows: 5, Cols: 5, InAO: make([]bool, 25), Explored: make([]bool, 25)}
	for i := range g.InAO {
		g.InAO[i] = true
	}
	g.InAO[g.CellAt(2, 2)] = false
	return g
}

// The clearance must exceed the arrival radius plus the position error, not
// merely reach it, and a budget that is not a finite positive radius and a
// finite non-negative error is refused rather than checked.
func TestArrivalCheck(t *testing.T) {
	g := notched()
	for _, tc := range []struct {
		name string
		a    Arrival
		want error
	}{
		{"the engine default", Arrival{RadiusM: 5, ErrorM: 5}, nil},
		{"no position error", Arrival{RadiusM: 12, ErrorM: 0}, nil},
		{"the clearance exactly spent", Arrival{RadiusM: 5, ErrorM: 7.5}, ErrClearance},
		{"beyond the clearance", Arrival{RadiusM: 10, ErrorM: 5}, ErrClearance},
		{"no radius", Arrival{RadiusM: 0, ErrorM: 5}, ErrArrival},
		{"a negative error", Arrival{RadiusM: 5, ErrorM: -1}, ErrArrival},
		{"a radius not a number", Arrival{RadiusM: math.NaN(), ErrorM: 5}, ErrArrival},
		{"an infinite error", Arrival{RadiusM: 5, ErrorM: math.Inf(1)}, ErrArrival},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.a.Check(g); !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
		})
	}
}

// The clearance is a quarter cell, and it is checked exactly: a segment
// grazing the corner of the out cell, which every quarter-cell sample would
// have passed, is refused; one a hair beyond the clearance is accepted.
func TestSegmentClearance(t *testing.T) {
	g := notched()
	if c := Clearance(g); c != 12.5 {
		t.Fatalf("clearance %v m, want a quarter of a 50 m cell", c)
	}
	cases := []struct {
		name string
		a, b vec
		want bool
	}{
		{"cell centre next to the notch", vec{75, 125}, vec{75, 125}, true},
		{"row of centres below the notch", vec{25, 75}, vec{225, 75}, true},
		{"through the notch", vec{25, 125}, vec{225, 125}, false},
		{"grazing the notch's corner", vec{75, 75}, vec{175, 175}, false},
		{"12.4 m below the notch", vec{25, 87.6}, vec{225, 87.6}, false},
		{"12.6 m below the notch", vec{25, 87.4}, vec{225, 87.4}, true},
		{"12.4 m from the raster edge", vec{12.4, 25}, vec{12.4, 225}, false},
		{"diagonal clear of the notch corner", vec{75, 60}, vec{140, 125 - 100}, true},
	}
	for _, tc := range cases {
		if got := segmentClear(g, tc.a, tc.b); got != tc.want {
			t.Errorf("%s: clear %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSegmentBoxDistance(t *testing.T) {
	lo, hi := vec{0, 0}, vec{10, 10}
	for _, tc := range []struct {
		a, b vec
		want float64
	}{
		{vec{-5, 5}, vec{15, 5}, 0},     // through
		{vec{10, 12}, vec{20, 12}, 2},   // above a corner's edge
		{vec{13, 14}, vec{13, 14}, 5},   // a point off a corner: 3-4-5
		{vec{-10, 20}, vec{20, -10}, 0}, // across a corner
		{vec{15, -5}, vec{15, 15}, 5},   // alongside
		{vec{10, 10}, vec{20, 20}, 0},   // touching a corner
	} {
		if got := segmentBoxDistance(tc.a, tc.b, lo, hi); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("segment %v to %v: %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// Repair bridges a segment short of the clearance with a detour whose every
// segment keeps it, and keeps the original ends.
func TestRepairKeepsTheClearance(t *testing.T) {
	g := notched()
	r := newRouter(g)
	a, b := vec{25, 125}, vec{225, 125}
	out, ok := r.repair([]vec{a, b})
	if !ok {
		t.Fatal("repair failed on a connected raster")
	}
	if out[0] != a || out[len(out)-1] != b || len(out) < 3 {
		t.Fatalf("repaired route %v, want a detour from %v to %v", out, a, b)
	}
	for i := 1; i < len(out); i++ {
		if !segmentClear(g, out[i-1], out[i]) {
			t.Fatalf("detour segment %v to %v short of the clearance", out[i-1], out[i])
		}
	}
}
