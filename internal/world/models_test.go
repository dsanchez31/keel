package world

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
)

func TestBatteryPaysForDistanceAndHover(t *testing.T) {
	b := battery{pct: 100}
	// A tenth of the range costs a tenth of the charge.
	b.spend(6000, 60000, false, 0.5, 0.1)
	if math.Abs(b.pct-90) > 1e-9 || b.reported() != 90 {
		t.Fatalf("after 6 km of 60: %v%% reported %d", b.pct, b.reported())
	}
	// Flying costs distance only: staying aloft at cruise is in MaxRangeM.
	b.spend(0.6, 60000, true, 0.5, 0.1)
	if math.Abs(b.pct-89.999) > 1e-9 {
		t.Fatalf("a tick of flight cost %v%%, want the distance alone", 90-b.pct)
	}
	b.pct = 90
	// A minute of hovering costs the hover drain, a minute on the ground
	// costs nothing.
	for range 600 {
		b.spend(0, 60000, true, 0.5, 0.1)
	}
	if math.Abs(b.pct-89.5) > 1e-9 || b.reported() != 89 {
		t.Fatalf("after a minute of hover: %v%% reported %d", b.pct, b.reported())
	}
	for range 600 {
		b.spend(0, 60000, false, 0.5, 0.1)
	}
	if math.Abs(b.pct-89.5) > 1e-9 {
		t.Fatalf("a minute on the ground cost %v%%", 89.5-b.pct)
	}
	b.drain(200)
	if !b.empty() || b.reported() != 0 {
		t.Fatalf("drained past zero: %v%%", b.pct)
	}
}

func TestCommsReach(t *testing.T) {
	c := comms{
		cfg:       Comms{RangeM: 1000},
		stations:  []domain.Station{{Name: "gcs", Position: at(0, 0, 0)}},
		blackouts: []Blackout{{Name: "shadow", Center: at(500, 0, 0), RadiusM: 100}},
	}
	for _, tc := range []struct {
		p    domain.Position
		want bool
	}{
		{at(0, 900, 100), true},
		{at(0, 1100, 100), false},
		{at(500, 50, 100), false},
		{at(500, 150, 100), true},
	} {
		if got := c.reach(tc.p); got != tc.want {
			t.Fatalf("reach at %v: %v, want %v", tc.p, got, tc.want)
		}
	}
	if !(comms{}).reach(at(1e6, 0, 0)) {
		t.Fatal("no range and no zone: the link is everywhere")
	}
}

// Loss happens at about the configured rate, delays stay within the jitter,
// and the same seed draws the same fates.
func TestCommsTransit(t *testing.T) {
	c := comms{cfg: Comms{LossPct: 10, JitterMs: 300}}
	draw := func() (int, []int64) {
		rng := rand.New(rand.NewPCG(7, 7))
		lost := 0
		var delays []int64
		for range 10000 {
			l, d := c.transit(rng)
			if l {
				lost++
			}
			delays = append(delays, d)
		}
		return lost, delays
	}
	lost, delays := draw()
	if lost < 900 || lost > 1100 {
		t.Fatalf("%d of 10000 lost, want about 1000", lost)
	}
	if slices.Min(delays) != 0 || slices.Max(delays) != 3 {
		t.Fatalf("delays in [%d, %d] ticks, want [0, 3]", slices.Min(delays), slices.Max(delays))
	}
	lost2, delays2 := draw()
	if lost2 != lost || !slices.Equal(delays, delays2) {
		t.Fatal("the same seed drew different fates")
	}
}

func TestChannelDeliversInDueThenSendOrder(t *testing.T) {
	var ch channel[string]
	ch.push(3, "late")
	ch.push(1, "first")
	ch.push(1, "second")
	ch.push(2, "middle")
	if got := ch.pop(0); len(got) != 0 {
		t.Fatalf("delivered %v before anything was due", got)
	}
	if got := ch.pop(2); !slices.Equal(got, []string{"first", "second", "middle"}) {
		t.Fatalf("delivered %v", got)
	}
	if got := ch.pop(10); !slices.Equal(got, []string{"late"}) {
		t.Fatalf("delivered %v, want the late one alone", got)
	}
}

func TestReportedPositionNoiseAndDrift(t *testing.T) {
	s := sensors{gpsNoiseM: 1.5}
	rng := rand.New(rand.NewPCG(1, 2))
	truth := at(0, 0, 120)
	for range 1000 {
		p := s.report(truth, 0, 0, rng)
		e, n := enu(truth, p)
		if math.Abs(e) > 1.5+1e-6 || math.Abs(n) > 1.5+1e-6 || p.AltM != 120 {
			t.Fatalf("reported %v, %v m off with a 1.5 m bound", e, n)
		}
	}
	p := sensors{}.report(truth, 30, -40, rng)
	if e, n := enu(truth, p); math.Abs(e-30) > 1e-6 || math.Abs(n+40) > 1e-6 {
		t.Fatalf("drift applied as %v, %v, want 30, -40", e, n)
	}
}
