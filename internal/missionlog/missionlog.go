// Package missionlog is the record layout of one mission on the hash chain:
// what keelsim's closed loop and keeld both write, and what a replay reads.
//
// A mission log is a header, then one group of records per tick: the events
// engine.Step was given, as given, the decisions it made, the commands it
// issued, and a tick summary. Recording the events as given is what makes the
// log replayable: engine.Step fed them again, from the state the header
// describes, reproduces every decision and every command, and the chain
// proves it. One writer for both programs keeps the two logs one format; a
// second copy of this layout would be a replay that verifies one of them
// and not the other.
package missionlog

import (
	"fmt"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
)

// Header is the first record of a mission log: everything a replay needs to
// rebuild the engine's initial state besides the doctrine packs, which the
// plan names by reference.
type Header struct {
	// Name says what recorded the mission: a keelsim scenario's name, a keeld
	// configuration's.
	Name string `json:"name"`
	// Seed seeds the engine's random source (engine.NewState).
	Seed uint64 `json:"seed"`
	// TickIntervalMs is the tick of mission time; AppendHeader sets it.
	TickIntervalMs int64 `json:"tick_interval_ms"`
	// Config is the engine's configuration: thresholds and deadlines are
	// inputs to every decision, as the events are.
	Config  engine.Config    `json:"config"`
	Plan    domain.PlanHash  `json:"plan"`
	Mission domain.MissionID `json:"mission"`
}

// TickSummary is the last record of a tick.
type TickSummary struct {
	Tick     int64               `json:"tick"`
	Explored int                 `json:"explored"`
	Total    int                 `json:"total"`
	State    domain.MissionState `json:"state"`
}

// AppendHeader records the header at mission time 0.
func AppendHeader(log eventlog.Appender, h Header) error {
	h.TickIntervalMs = domain.TickIntervalMs
	if _, err := log.Append(eventlog.RecordHeader, 0, h); err != nil {
		return fmt.Errorf("recording the header: %w", err)
	}
	return nil
}

// AppendTick records one tick: the batch engine.Step was given, the decisions
// and the commands it returned, and the summary of s, the state it returned.
func AppendTick(log eventlog.Appender, s engine.State, batch []domain.Event, decs []domain.Decision, cmds []domain.Command) error {
	at := s.Clock.TickMs
	for _, ev := range batch {
		if _, err := log.Append(eventlog.RecordEvent, at, ev); err != nil {
			return fmt.Errorf("recording an event: %w", err)
		}
	}
	for _, d := range decs {
		if _, err := log.Append(eventlog.RecordDecision, at, d); err != nil {
			return fmt.Errorf("recording a decision: %w", err)
		}
	}
	for _, c := range cmds {
		if _, err := log.Append(eventlog.RecordCommand, at, c); err != nil {
			return fmt.Errorf("recording a command: %w", err)
		}
	}
	explored, total := s.Grid().Coverage()
	sum := TickSummary{Tick: s.Clock.Tick, Explored: explored, Total: total, State: s.Mission.State}
	if _, err := log.Append(eventlog.RecordTick, at, sum); err != nil {
		return fmt.Errorf("recording the tick: %w", err)
	}
	return nil
}
