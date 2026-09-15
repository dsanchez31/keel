package domain

import (
	"math"
	"slices"
	"strings"
	"testing"
)

func TestFaultKindsSorted(t *testing.T) {
	if k := FaultKinds(); !slices.IsSorted(k) || len(slices.Compact(slices.Clone(k))) != len(k) {
		t.Fatalf("FaultKinds %v is not a sorted set", k)
	}
}

func TestFaultCheck(t *testing.T) {
	for _, f := range []Fault{
		{Kind: FaultKill, Vector: "DRONE-02"},
		{Kind: FaultLinkLoss, Vector: "DRONE-02", DurationMs: 5000},
		{Kind: FaultStaleTelemetry, Vector: "DRONE-02", DurationMs: 100},
		{Kind: FaultBatteryDrain, Vector: "DRONE-02", Magnitude: 40},
		{Kind: FaultGPSDrift, Vector: "DRONE-02", Magnitude: 0.5, DurationMs: 10000},
	} {
		if err := f.Check(); err != nil {
			t.Errorf("%+v refused: %v", f, err)
		}
	}

	cases := []struct {
		name  string
		fault Fault
		want  string
	}{
		{"no vector", Fault{Kind: FaultKill}, "vector is required"},
		{"blank vector", Fault{Kind: FaultKill, Vector: " "}, "vector is required"},
		{"unknown kind", Fault{Kind: "meteor", Vector: "V"}, `unknown kind "meteor", valid: battery_drain, gps_drift, kill, link_loss, stale_telemetry`},
		{"drain without magnitude", Fault{Kind: FaultBatteryDrain, Vector: "V"}, "battery_drain needs a finite positive magnitude"},
		{"drift at infinity", Fault{Kind: FaultGPSDrift, Vector: "V", Magnitude: math.Inf(1)}, "gps_drift needs a finite positive magnitude"},
		{"NaN magnitude", Fault{Kind: FaultKill, Vector: "V", Magnitude: math.NaN()}, "not finite"},
		{"negative magnitude", Fault{Kind: FaultLinkLoss, Vector: "V", Magnitude: -1}, "negative"},
		{"negative duration", Fault{Kind: FaultLinkLoss, Vector: "V", DurationMs: -TickIntervalMs}, "whole number of ticks"},
		{"part of a tick", Fault{Kind: FaultLinkLoss, Vector: "V", DurationMs: 150}, "whole number of ticks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fault.Check()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
