---
status: accepted
date: 2026-09-12
---

# A logical tick of 100 ms, not the wall clock, drives the engine

## Context and Problem Statement

The engine reacts to telemetry, link verdicts, faults and operator actions arriving asynchronously from the network. When does it decide, and what time does it believe it is? If decisions depend on when events happen to arrive by the wall clock, no run can be reproduced, replayed or tested at more than real speed.

## Decision Drivers

* Replay and simulation must reproduce decisions exactly ([ADR 0001](0001-event-log-as-source-of-truth.md)).
* The DST harness needs to run missions far faster than real time and identically at any speed.
* Duration windows in doctrine (`when: <cond> for 5s`) and deadlines need a clock the decision path can read without I/O.
* Reaction latency must stay bounded and well below what the vehicles need.

## Considered Options

* A fixed logical tick: mission time an `int64` advanced by exactly 100 ms per `Step`, events batched per tick
* Event-driven processing, each event handled on arrival and stamped with the wall clock
* A variable tick, stepped when events arrive, advancing by the elapsed wall time

## Decision Outcome

Chosen option: "a fixed logical tick of 100 ms", because it makes time an input like any other: the daemon drains the inbox into a batch, `Step(state, batch)` advances the clock by one tick and decides, and the log records the batch as given. The wall clock lives only in the pacer (`internal/pacer`), which decides when a tick runs and never what it does.

### Consequences

* Good, because a headless-fast run and a real-time run of one scenario record the same chain, and a replay needs no clock at all.
* Good, because doctrine windows, the stall deadline and the re-send interval are counted in ticks, deterministic by construction.
* Good, because `pacer.RealTime` runs late ticks back to back rather than skipping them, so mission time never falls behind for good, which a `time.Ticker` would not guarantee.
* Bad, because reaction latency is up to one tick (100 ms) after an event arrives.
* Bad, because frames arriving between two ticks are state, not history: the latest wins and the intermediate ones never reach the engine.
* Neutral, because a vehicle that knows no mission time sends `last_seen_ms` 0 and the engine stamps the frame with the tick that accepts it (spec section 4.2).

### Confirmation

* `forbidigo` in `.golangci.yml` bans `time.Now`, `time.Since`, `time.Until`, `time.Tick`, `time.After`, `time.NewTimer` and `time.NewTicker` in the four decision-path packages.
* Invariant I5 (mission time advances by exactly one tick per tick) is asserted by the DST harness at every tick boundary.
* `cmd/keelsim/loop_test.go` asserts identical head hashes for headless-fast and real-time runs.

## Pros and Cons of the Options

### A fixed logical tick

* Good, because the engine is a pure function of state and batch, testable and replayable.
* Bad, because of the one-tick latency floor and batching semantics.

### Event-driven, stamped with the wall clock

* Good, because each event is handled with the lowest possible latency.
* Bad, because the order and timing of decisions depend on network and scheduler timing, so no run reproduces and replay would have to simulate the scheduler.

### A variable tick

* Good, because no work is done when nothing happens.
* Bad, because the step size becomes an input that varies run to run, windows and deadlines accumulate rounding, and a replay must reproduce the step sizes exactly.

## More Information

Recorded retroactively on 2026-09-12: the decision was taken during design and holds in the code as built. Normative: `spec.md` section 3 (mission time), section 12 (requirement 1) and section 15.3 (the pacer). Reasoning: `design.md` sections 3.1 and 4.1.
