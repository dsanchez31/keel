---
status: accepted
date: 2026-09-12
---

# MAVLink v2 to ArduPilot as the first real-vehicle adapter

## Context and Problem Statement

KEEL claims an open integration layer: any vehicle behind one `Vector` contract, proven by a conformance suite. KEEL's own protocol (`keel.native.v1`) proves the contract can be served, but only against a peer written to fit it. Which real protocol and autopilot should the first adapter target, to show the contract holds against a system that was not designed for KEEL?

## Decision Drivers

* A protocol the industry actually flies, against an autopilot that really arms, takes off and flies to a point.
* Testable without hardware: a simulator of the real autopilot, runnable in `docker compose`.
* No extra runtime or middleware between KEEL and the autopilot.
* The contract's hard parts must be exercised: acknowledgement, idempotency by `seq`, liveness, capability honesty.

## Considered Options

* MAVLink v2 straight to ArduPilot (ArduCopter, ArduRover) through `github.com/bluenviron/gomavlib/v4`, SITL in `docker-compose.yml`
* MAVSDK, through its gRPC server
* ROS 2
* The native protocol only

## Decision Outcome

Chosen option: "MAVLink v2 straight to ArduPilot through gomavlib", because it is the binary protocol ArduPilot and PX4 speak, ArduPilot's SITL runs the real autopilot code under Docker, and speaking it directly puts every gap between MAVLink and the `Vector` contract in KEEL's code, where it is decided explicitly and tested.

Every command decision was checked against ArduPilot's own handlers rather than the MAVLink documentation alone: `COMMAND_INT` for every command, `DO_REPOSITION` with the mode-change flag for `goto`, one command in flight per vehicle, `AckSeq` from `COMMAND_ACK` with `MAV_RESULT_ACCEPTED`, no retry in the adapter (the engine re-sends), frames withheld rather than guessed when the autopilot or battery is unknown.

### Consequences

* Good, because the same conformance suite validates keelsim, an ArduPilot SITL and hardware through the same adapter keeld runs.
* Good, because a fake ArduPilot (`adapters/mavlink/mavlinktest`) serves the adapter's tests and the suite's without SITL.
* Bad, because only ArduPilot is supported; PX4 and other autopilots are out of scope.
* Bad, because the adapter arms and takes off a landed copter on a `goto`, without operator consent: acceptable against SITL, not against hardware. The durable fix is an `AutoLaunch` option, false by default (`design.md` section 10, item 14).
* Bad, because heights are converted from AMSL with one configured geoid separation per link, wrong by metres far from the operating area (`design.md` section 10, item 15).
* Neutral, because acknowledgement is per command, not per effect: an autopilot leaving guided mode after acknowledging is not sent the command again (`design.md` section 10, item 11).

### Confirmation

* `internal/conformance/suite_test.go` runs the nine cases against the MAVLink adapter through a UDP fault proxy and `mavlinktest`, and against the native adapter.
* `keelctl vector validate --mavlink 127.0.0.1:14550 --sysid 1 --vector DRONE-01 --allow-motion` runs them against the SITL of `docker-compose.yml`.

## Pros and Cons of the Options

### MAVLink v2 directly, through gomavlib

* Good, because a maintained pure-Go implementation (v2 framing and signing, generated `ardupilotmega` dialect, UDP, TCP and serial endpoints behind one node) with no runtime dependency.
* Good, because one link routing by system id matches how a radio or a router carries a fleet.
* Bad, because every MAVLink quirk (no `seq`, AMSL heights, modes per vehicle type) is KEEL's to translate.

### MAVSDK, through its gRPC server

* Good, because higher-level actions (arm, takeoff, goto) are provided and tested.
* Bad, because a second process to ship and supervise beside keeld, one server per vehicle system, and MAVSDK's semantics (arming as an explicit action) sitting between the contract and the autopilot.

### ROS 2

* Good, because a large robotics ecosystem and a standard middleware.
* Bad, because a heavy runtime and build system for one adapter, and a middleware layer rather than a vehicle protocol: MAVLink would still be underneath for ArduPilot.

### The native protocol only

* Good, because the least work.
* Bad, because a contract proven only against peers written for it proves nothing about integration.
