---
status: accepted
date: 2026-09-12
---

# A simulated fleet runs faster than real time, the whole closed loop together

## Context and Problem Statement

A mission of the reference world lasts about 38 minutes of real time. That is too long to watch in a demonstration or to film, and too long for an operator exploring a doctrine against the simulator. keeld ticked in real time only, and `keelsim --serve` refused any other speed. The reason was sound: a world running faster than the orchestrator flies every vehicle faster than its declared cruise speed in the orchestrator's mission time. The design then held that "live is live", so the live screen had nothing to speed up. How can a live simulated mission run faster without breaking what the engine believes about its fleet?

## Decision Drivers

* The engine must see a consistent world: every vehicle at its declared cruise speed in mission time, frames at their rate in mission time, silence meaning silence.
* No decision may depend on the pace: the log and its replay stay what they are ([ADR 0001](0001-event-log-as-source-of-truth.md), [ADR 0004](0004-logical-tick-over-wall-clock.md)).
* A real vehicle cannot be sped up, and nothing may pretend otherwise.
* The operator must never mistake an accelerated fleet for real time.
* Return to real time at any moment, from the same screen.

## Considered Options

* Pace the whole closed loop together: keeld's pacer, keelsim's world through a clock endpoint and each SITL through `SIM_SPEEDUP`, set from the live screen
* A faster view in the browser only
* A speed fixed at startup for every process of the stack
* Lockstep: keeld stepping every simulator tick by tick

## Decision Outcome

Chosen option: "pace the whole closed loop together", because it is the one option that keeps the engine's world consistent at any pace and still changes the pace while a mission runs.

`PUT /api/v1/clock` sets a whole number from 1 to 20. keeld sets the simulators first when the pace rises and last when it falls, so the engine never runs ahead of the world. A simulator that fails brings every one back to the previous pace. The pace is refused while a vector no simulator paces is bound. The adapters' link health reads the pacer's scaled clock, so a threshold is the same mission time at any pace. The live stream carries the pace and is thinned by it. The live strip offers *Real time*, then ×2 to ×20, and turns amber and reads *Accelerated ×N* above real time.

### Consequences

* Good, because the reference mission runs in about two minutes at ×20 (122 s measured under Docker), watched live from intent to completion, fault and redecomposition included.
* Good, because nothing below the fence changes: the pacer decides when a tick runs, never what it holds, the pace is not recorded, and a replay is identical whatever pace the mission ran at.
* Good, because the real-vehicle case is closed by construction: a fleet holding one vector that is not time-scalable stays in real time, with the reason shown to the operator.
* Bad, because keeld, keelsim and each SITL keep time apart and change pace within milliseconds of each other rather than on one tick, and at ×20 the host's timer jitter is 20 times larger in mission time (`design.md` section 10, item 23).
* Bad, because a SITL at ×20 flies real dynamics only while the host keeps up.
* Neutral, because a live run at ×20 and the same run at ×1 are different missions, as two live runs at ×1 already are: the log, not the live run, is what reproduces.

### Confirmation

* `internal/daemon/clock_test.go`: the order of the changes (simulators before the pacer when raising, after when lowering), rollback on a refusal, the refusal while a real vehicle is bound, the pace pushed again to a reconnected simulator, a SITL paced through its adapter, the live stream thinned by the pace.
* `internal/pacer/pacer_test.go`: a change of speed skips and repeats no tick, wakes a sleeping wait, and scales the clock the adapters read.
* `adapters/mavlink/adapter_test.go`: `SIM_SPEEDUP` held on first contact, on demand and after a reboot, never sent to a vehicle not configured as a SITL.
* `cmd/keelsim/serve_test.go`: the clock endpoint.
* `go test ./cmd/keelsim -run TestReferenceReplay`: the reference chain is unchanged.

## Pros and Cons of the Options

### Pace the whole closed loop together

* Good, because the world stays consistent in mission time and the pace changes while running.
* Bad, because three independent pacers must agree, and a change is a small distributed operation with an order and a rollback.

### A faster view in the browser only

* Good, because nothing on the server changes.
* Bad, because a view cannot get ahead of the present: live, there is nothing faster to show.

### A speed fixed at startup

* Good, because nothing changes at runtime and no rollback is needed.
* Bad, because returning to real time means restarting the stack, which is the opposite of what was asked.

### Lockstep

* Good, because a simulated live run becomes reproducible at any pace, the simulators stepping on keeld's tick.
* Bad, because it is a new protocol between keeld and keelsim and ArduPilot's JSON SITL interface for the autopilots, far beyond what a demonstration needs. It stays the durable fix if a live simulated run, not only its log, must reproduce (`design.md` section 10, item 23).

## More Information

Supersedes the "real time only" rule of serve mode and the "live is live" wording of the live strip. It extends ADR 0004 and does not supersede it: the logical tick still drives the engine, and only the pacer's wall-clock rate changes. Normative: `spec.md` sections 7.1, 7.4, 8.1, 8.2, 15.4 and 16.5. Reasoning: `design.md` sections 3.3, 4.1, 8.3 and 10 (item 23).
