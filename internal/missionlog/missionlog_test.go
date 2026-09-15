package missionlog

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
)

func TestAppendHeader(t *testing.T) {
	log := eventlog.NewMemLog()
	cfg := engine.DefaultConfig()
	if err := AppendHeader(log, Header{Name: "reference", Seed: 42, Config: cfg, Plan: "ab12", Mission: "MSN-042"}); err != nil {
		t.Fatal(err)
	}
	recs := log.Records()
	if len(recs) != 1 || recs[0].Kind != eventlog.RecordHeader || recs[0].TickMs != 0 {
		t.Fatalf("records %+v, want one header at 0 ms", recs)
	}
	var h Header
	if err := json.Unmarshal(recs[0].Payload, &h); err != nil {
		t.Fatal(err)
	}
	want := Header{Name: "reference", Seed: 42, TickIntervalMs: domain.TickIntervalMs, Config: cfg, Plan: "ab12", Mission: "MSN-042"}
	if h != want {
		t.Fatalf("header %+v, want %+v", h, want)
	}
}

func TestAppendTick(t *testing.T) {
	caps := domain.Capabilities{ID: "D-1", Domain: domain.DomainAerial, Tags: []string{"gps"}, CruiseSpeed: 15, MaxRangeM: 30000, SensorRadiusM: 60}
	st := domain.VectorState{ID: "D-1", Position: domain.Position{Lat: 45, Lon: 5}, BatteryPct: 100, Link: domain.LinkOK, Mode: domain.ModeIdle}
	batch := []domain.Event{
		{Kind: domain.EventVectorJoined, Vector: "D-1", Caps: &caps, Telemetry: &st},
		{Kind: domain.EventOperatorAbort, Reason: "test"},
	}
	s, cmds, decs := engine.Step(engine.NewState(1, engine.DefaultConfig(), nil), batch)
	if len(decs) == 0 {
		t.Fatal("the batch made no decision to record")
	}

	log := eventlog.NewMemLog()
	if err := AppendTick(log, s, batch, decs, cmds); err != nil {
		t.Fatal(err)
	}
	var kinds []eventlog.RecordKind
	for _, r := range log.Records() {
		if r.TickMs != s.Clock.TickMs {
			t.Fatalf("record %d at %d ms, want the tick's %d ms", r.Seq, r.TickMs, s.Clock.TickMs)
		}
		kinds = append(kinds, r.Kind)
	}
	var want []eventlog.RecordKind
	for range batch {
		want = append(want, eventlog.RecordEvent)
	}
	for range decs {
		want = append(want, eventlog.RecordDecision)
	}
	for range cmds {
		want = append(want, eventlog.RecordCommand)
	}
	want = append(want, eventlog.RecordTick)
	if !slices.Equal(kinds, want) {
		t.Fatalf("record kinds %v, want %v", kinds, want)
	}

	recs := log.Records()
	var first domain.Event
	if err := json.Unmarshal(recs[0].Payload, &first); err != nil {
		t.Fatal(err)
	}
	if first.Kind != domain.EventVectorJoined || first.Caps == nil || first.Caps.ID != "D-1" {
		t.Fatalf("first record %+v, want the join as given", first)
	}
	var sum TickSummary
	if err := json.Unmarshal(recs[len(recs)-1].Payload, &sum); err != nil {
		t.Fatal(err)
	}
	if sum != (TickSummary{Tick: 1, State: s.Mission.State}) {
		t.Fatalf("tick summary %+v", sum)
	}
}
