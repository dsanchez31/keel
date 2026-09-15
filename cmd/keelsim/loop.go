package main

import (
	"context"
	"fmt"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/missionlog"
	"github.com/dsanchez31/keel/internal/pacer"
	"github.com/dsanchez31/keel/internal/world"
)

// Run is one closed-loop simulated mission: the engine, the world it
// commands, the plan the operator approved and the faults to inject. Its
// pacer decides only when each tick runs: fast and real time are one code
// path, and record the same log.
type Run struct {
	Name     string
	State    engine.State
	World    *world.World
	Plan     domain.ApprovedPlan
	Faults   []world.ScheduledFault
	Log      eventlog.Appender
	Pacer    pacer.Pacer
	MaxTicks int64
	// OnTick, if set, is called after every tick with its result.
	OnTick func(engine.TickResult)
}

// Summary is how a run ended.
type Summary struct {
	Ticks     int64
	MissionMs int64
	State     domain.MissionState
	Explored  int
	Total     int
	Decisions int
	Commands  int
	Injected  []domain.Fault
	Head      eventlog.Digest
}

// Execute runs the mission until it completes, fails, reaches MaxTicks or the
// context ends.
//
// One tick of the loop:
//
//  1. wait for the pacer
//  2. engine.Step over the batch that arrived during the previous tick
//  3. record the batch, the decisions, the commands and a tick summary
//  4. put the commands on the air
//  5. inject every scheduled fault now due, into the world and, as a
//     fault_injected event, into the next batch
//  6. world.Step, whose telemetry is the next batch
//
// The first batch is the fleet joining and the plan being approved. Faults
// are judged on the engine's state after its tick, coverage included, which
// is how "at 60 percent coverage" is observed rather than guessed.
func (r *Run) Execute(ctx context.Context) (Summary, error) {
	log := r.Log
	if err := missionlog.AppendHeader(r.Log, missionlog.Header{
		Name:    r.Name,
		Seed:    r.State.Rand.Seed(),
		Config:  r.State.Config,
		Plan:    r.Plan.Hash,
		Mission: r.Plan.Mission,
	}); err != nil {
		return Summary{}, err
	}

	plan := r.Plan
	batch := append(r.World.Join(), domain.Event{Kind: domain.EventPlanApproved, Plan: &plan})
	pending := append([]world.ScheduledFault(nil), r.Faults...)
	s := r.State
	var sum Summary

	for tick := int64(1); tick <= r.MaxTicks; tick++ {
		if err := r.Pacer.Wait(ctx, tick); err != nil {
			return r.finish(sum, s, log), err
		}
		next, cmds, decs := engine.Step(s, batch)
		s = next
		if err := missionlog.AppendTick(r.Log, s, batch, decs, cmds); err != nil {
			return r.finish(sum, s, log), err
		}
		sum.Ticks, sum.Decisions, sum.Commands = tick, sum.Decisions+len(decs), sum.Commands+len(cmds)
		if r.OnTick != nil {
			r.OnTick(engine.TickResult{State: s, Commands: cmds, Decisions: decs})
		}
		if s.Mission.State.Terminal() {
			break
		}

		r.World.Send(cmds)
		var injected []domain.Event
		explored, total := s.Grid().Coverage()
		coverage := 0.0
		if total > 0 {
			coverage = float64(explored) * 100 / float64(total)
		}
		kept := pending[:0]
		for _, f := range pending {
			if !f.Due(r.World.NowMs()+engine.TickIntervalMs, coverage) {
				kept = append(kept, f)
				continue
			}
			if err := r.World.Inject(f.Fault); err != nil {
				return r.finish(sum, s, log), fmt.Errorf("injecting %s on %s: %w", f.Fault.Kind, f.Fault.Vector, err)
			}
			fault := f.Fault
			injected = append(injected, domain.Event{Kind: domain.EventFaultInjected, Vector: fault.Vector, Fault: &fault})
			sum.Injected = append(sum.Injected, fault)
		}
		pending = kept
		batch = append(r.World.Step(), injected...)
	}
	return r.finish(sum, s, log), nil
}

func (r *Run) finish(sum Summary, s engine.State, log eventlog.Appender) Summary {
	sum.MissionMs = s.Clock.TickMs
	sum.State = s.Mission.State
	sum.Explored, sum.Total = s.Grid().Coverage()
	sum.Head = log.Head()
	return sum
}
