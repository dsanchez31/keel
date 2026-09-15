# Integrating a vehicle with KEEL

KEEL drives every vehicle through one contract, the `Vector` interface of `spec.md` section 7, and trusts a vehicle once it passes the nine conformance cases of section 13. This document is the integrator's guide: which path to take, what the vehicle must do, and how to get `keelctl vector validate` to print `conformant`.

The spec is the normative text; this guide quotes it where a detail matters and otherwise points at it. `design.md` section 7 explains why the contract is shaped as it is.

---

## 1. Choose a path

| Path | For | Code on KEEL's side | Validated with |
|---|---|---|---|
| **1. Serve the native protocol** | A vehicle, a simulator or a bridge to a vendor SDK that you control, in any language | None: keeld dials it with the native adapter | `keelctl vector validate --native ws://...` |
| **2. MAVLink** | ArduCopter and ArduRover, SITL or hardware | None: configuration only | `keelctl vector validate --mavlink ... --sysid ...` |
| **3. A new transport inside KEEL** | A protocol neither of the above can reach, contributed to this repository | An adapter package, a daemon binding, a conformance subject | The same command, once the subject exists |

**Start with path 1** unless the vehicle already speaks MAVLink. The native protocol (`keel.native.v1`, JSON over WebSocket) exists so that integration needs no change to KEEL: a bridge process translating between a vendor's SDK and the protocol is the usual shape, and `adapters/native` ships the vehicle side for a bridge written in Go.

**Path 3 is a contribution, not a plugin.** The public package `github.com/dsanchez31/keel/vector` lets anyone write a Go type implementing `Vector`, but keeld binds only the transports its configuration knows (`native`, `mavlink`), and the conformance suite lives in `internal/conformance`, which Go forbids importing from another module. An adapter maintained outside this repository therefore cannot be bound or validated as is; the way to reach KEEL from outside is to serve the native protocol (path 1).

---

## 2. What every vehicle must do

Whatever the path, the contract is the same. The adapter enforces most of it; these are the parts only the vehicle can get right.

**Declare itself honestly.** Capabilities (`spec.md` section 4.1) are stable for a connection's lifetime:

| Field | Meaning | Rule |
|---|---|---|
| `id` | Stable, unique within a mission | Must match the id keeld binds, and the world's fleet entry |
| `domain` | `aerial` or `ground` | Nothing else is in scope |
| `tags` | A set, such as `["aerial", "camera", "gps"]` | What plans require (`requires` in a plan) is matched against it |
| `cruise_speed` | m/s | Positive |
| `max_range_m` | Metres at cruise on a full battery | Positive; the allocator refuses lanes beyond it |
| `sensor_radius_m` | Metres of ground footprint | What coverage paints as explored |

Declare only the command types the vehicle executes (`supports` in the native hello). An undeclared type is refused by the adapter with `ErrUnsupported` (C8) and the engine plans around it; a declared type the vehicle ignores is the silent failure the contract exists to prevent.

**Report state as frames** (`spec.md` section 4.2), at a steady rate:

| Field | Rule |
|---|---|
| `id` | The vehicle's own id |
| `position` | `{lat, lon, alt_m}`, WGS84 degrees and ellipsoid height in metres (`spec.md` section 3), finite |
| `heading`, `speed` | Degrees clockwise from true north and m/s, finite, speed not negative |
| `battery_pct` | Integer in `[0, 100]` |
| `link` | `ok`. A frame that arrives is the proof of a link; the adapter's `Health` and the engine's own aging decide when it degrades |
| `mode` | The **physical** mode only: `idle`, `transit` (moving toward a commanded point, station keeping on arrival included) or `rtb`. The engine derives `scanning` and sets `down` itself: a frame reporting either breaks C2 and the adapter refuses it (a native link closes with 1008) |
| `last_seen_ms` | `0`: the vehicle does not know KEEL's mission time, and the engine stamps the frame with the tick that accepts it. Never decreasing |
| `ack_seq` | The `seq` of the last command the vehicle applied, `0` before any. After a restart, `0` again: the engine re-sends what stands |

Send frames at **2 Hz at least**: C2 wants 100 consecutive frames within a minute, and a vehicle silent for 5 s is degraded, for 10 s lost. keelsim sends at 10 Hz and the MAVLink adapter asks ArduPilot for 4 Hz.

**Apply commands idempotently by `seq`** (`spec.md` section 7.2). Commands carry `vector`, `seq` (from 1, increasing per vector), `type`, a `waypoint` for `goto` (and for `rtb` when a station is known) and the `lane` they belong to.

- A `seq` at or below the last applied one is a no-op, not an error. The engine re-sends a command it has seen no acknowledgement for every 2 s, **with the same `seq`**.
- Gaps are normal: a lost or superseded command leaves one. Apply the newer `seq`.
- Report the applied `seq` as `ack_seq` in the next frame. That is the acknowledgement: nothing else stops the re-sends.

| Command | The vehicle |
|---|---|
| `goto` | Flies or drives to `waypoint` and keeps station there, reporting `transit` throughout |
| `hold` | Stops where it is, `idle` |
| `abort` | Stops where it is, `idle`: sent when an operator aborts the mission or keeld shuts down |
| `rtb` | Goes to `waypoint` (the station) or, without one, to its home, reporting `rtb`, then `idle` on arrival |

---

## 3. Path 1: serve the native protocol

The full protocol is `spec.md` section 7.3. In short:

**The adapter dials the vehicle.** One WebSocket connection per vehicle at `/v1/vectors/{id}`, subprotocol `keel.native.v1`. An unknown id answers 404 at the upgrade; a peer that does not negotiate the subprotocol is closed with 1002. The vehicle is the server because the end that dials is the end that can reconnect (C7), and keeld owns reconnection.

**One JSON text message per envelope**, `{"type": ..., "data": ...}`, at most 32 KiB.

The vehicle's first message, and only its first, is its `hello`:

```json
{"type":"hello","data":{"capabilities":{"cruise_speed":15,"domain":"aerial","id":"SCOUT-01","max_range_m":40000,"sensor_radius_m":80,"tags":["aerial","camera","gps"]},"supports":["abort","goto","hold","rtb"]}}
```

Then the vehicle sends `telemetry`:

```json
{"type":"telemetry","data":{"ack_seq":6,"battery_pct":87,"heading":92.5,"id":"SCOUT-01","last_seen_ms":0,"link":"ok","mode":"transit","position":{"alt_m":120,"lat":45.0301,"lon":5.0412},"speed":15}}
```

and receives `command`:

```json
{"type":"command","data":{"lane":"lane-02","seq":7,"type":"goto","vector":"SCOUT-01","waypoint":{"alt_m":120,"lat":45.0318,"lon":5.0433}}}
```

**Decoding is strict, both ways.** A field the domain type declares without `omitempty` is required (`null` counts as absent), and an unknown field, a key differing only in case, an unknown type or a message out of place closes the connection with its reason:

| Close status | Cause |
|---|---|
| 1002 | Protocol violation: missing or unknown field, unknown message type, message out of place, subprotocol not negotiated |
| 1003 | A binary message |
| 1008 | A frame that decodes but breaks C2 (another id, a position not finite, a battery outside `[0, 100]`, a non-physical mode, `last_seen_ms` going back), a command for another vehicle or of an undeclared type, a reconnect whose hello differs from the first |
| 1001 | A newer connection replaced this one |

The adapter then reports the reason through `Health` and redials, from 250 ms to 5 s with jitter. A vehicle that keeps failing keeps saying why.

**Behaviour the adapter expects:**

- **The hello never changes.** The first one fixes the capabilities and declared types for the adapter's lifetime; a reconnect declaring anything else is closed with 1008.
- **The newest connection wins.** Accept a new connection and close the old one with 1001: an adapter redialling after a half-open connection must get in at once.
- **Telemetry and commands are latest wins.** A frame is state, superseded by the next; send the latest frame first after a new hello. Do not queue frames while disconnected.
- **Stay talkative.** A connection carrying no message for 10 s is dropped and redialled, so a vehicle with nothing new to report still sends its frame.

**In Go**, `adapters/native` carries the vehicle side, `native.Server`, the one `keelsim --serve` serves its simulated fleet through. A sketch of a bridge, `vehicle` standing for the vendor side:

```go
srv, err := native.NewServer(native.ServerOptions{})
if err != nil {
    return err
}
scout, err := srv.Add(vector.Capabilities{
    ID: "SCOUT-01", Domain: vector.DomainAerial, Tags: []string{"aerial", "camera", "gps"},
    CruiseSpeed: 15, MaxRangeM: 40000, SensorRadiusM: 80,
}, []vector.CommandType{vector.CommandGoto, vector.CommandHold, vector.CommandRTB, vector.CommandAbort})
if err != nil {
    return err
}
go func() { _ = http.ListenAndServe("127.0.0.1:8091", srv) }() // ws://127.0.0.1:8091/v1/vectors/SCOUT-01

for {
    select {
    case c := <-scout.Commands(): // re-sends included: apply by Seq, idempotently
        applied = vehicle.Apply(c, applied)
    case <-ticker.C: // 4 Hz or more
        _ = scout.Send(vehicle.Frame(applied)) // AckSeq: applied
    }
}
```

`Server` handles the handshake, the subprotocol, strict decoding, the hello, newest-wins connections and latest-wins telemetry; the bridge applies commands and publishes frames. `Send` never waits on the network, and refuses a frame for another vehicle or one holding a value that is not finite.

**Bind it in keeld.** The id must be in the world's fleet (`examples/worlds/reference.yaml`), which gives the planner its areas and stations; the capabilities keeld uses are the ones the hello declares. Then, in the `keel.daemon/v1` configuration (`spec.md` section 16.1):

```yaml
vectors:
  - id: SCOUT-01
    native: {url: "ws://127.0.0.1:8091/v1/vectors/SCOUT-01"}
```

keeld dials in the background until the vehicle answers, so start order does not matter. A hello naming another id is refused: a misrouted vehicle would fly under another's name. `faults` and `clock` are for simulators only: keelsim's `POST /v1/faults` and `PUT /v1/clock` (`spec.md` section 15.4). A real vehicle has neither. keeld refuses to inject faults into it, and keeps the whole fleet in real time while it is bound: nothing flies a real vehicle faster than real time (`spec.md` section 16.5).

The protocol carries no authentication. Serve it on loopback or on a network you trust, as keelsim does by default (`127.0.0.1:8090`).

---

## 4. Path 2: MAVLink

`adapters/mavlink` drives ArduCopter (aerial) and ArduRover (ground) over MAVLink v2, `ardupilotmega` dialect (`spec.md` section 7.4). The vehicle needs nothing KEEL-specific, but MAVLink carries no capabilities, so they come from the world's fleet entry for the id:

```yaml
mavlink:
  listen: 127.0.0.1:14550      # where every autopilot pushes its stream
  geoid_separation_m: 0        # EGM96 at the operating area, metres; 0 matches the SITL of docker-compose.yml
vectors:
  - id: DRONE-01
    mavlink: {sysid: 1}
```

- **One UDP port for the whole fleet**, routed by system id: give every autopilot its own `SYSID_THISMAV`.
- **Heights**: ArduPilot speaks AMSL, KEEL ellipsoid heights. The separation is one constant per link, adequate across an area of a few kilometres (`design.md` section 10, item 15).
- **Frames are withheld, not guessed**: no frame until a heartbeat from an ArduPilot autopilot of the declared domain, a `SYS_STATUS` with a known battery and an `EKF_STATUS_REPORT` giving an absolute position estimate. A vehicle waiting for its GPS lock therefore joins late rather than at (0, 0), and one losing its estimate in flight falls silent. The reason shows in `keelctl vector describe`.
- **The adapter arms and takes off on its own.** A `goto` reaching a landed copter arms it and climbs before repositioning (`design.md` section 10, item 14). That is acceptable against SITL and not against hardware without a safety pilot.
- **A SITL declares itself**: `mavlink: {sysid: 1, sitl: true}`. The adapter then holds the autopilot's `SIM_SPEEDUP` to keeld's pace, so the fleet may run faster than real time (`spec.md` section 16.5). Never set it on hardware: the parameter does not exist there, and the binding would keep a real vehicle out of the real-time guarantee.

`docker-compose.yml` runs one ArduCopter and one ArduRover SITL pushing to `127.0.0.1:14550`; `examples/keeld/reference.yaml` binds them as DRONE-01 and UGV-01.

---

## 5. Path 3: a new transport inside KEEL

When a vehicle can be reached neither by a native bridge nor by MAVLink, the adapter joins this repository. `adapters/native` and `adapters/mavlink` are the two worked examples; the steps:

1. **The adapter**, in `adapters/<name>/`, is a transport plus a `vector.Agent` (`vector/agent.go`), which implements the contract's mechanics once: the telemetry channel, the `Seq` filter on the acknowledged `Seq`, refusal of undeclared types, the cached `Health`, the C2 frame checks.
   - `vector.NewAgent(caps, transmit, vector.Options{Supports: ...})` checks the capabilities against C1. `transmit` hands a command to the transport and must not block.
   - Feed it from the transport: `Publish` for every frame, `AckSeq` filled with what the vehicle applied (the adapter's own bookkeeping when the protocol has no `seq`, as MAVLink's `COMMAND_ACK`), and `Disconnected(reason)` when the link drops.
   - `Close` stops the adapter's goroutines first, then calls `Agent.Close`.
   - No package logs: what the adapter has to say goes in `Health`.
2. **A binding** in `internal/daemon/config.go` (a key of `keel.daemon/v1` beside `native` and `mavlink`) and a member kind in `internal/daemon/fleet.go`, with `spec.md` sections 16.1 and 16.2.
3. **A conformance subject** in `internal/conformance/subject.go`: the suite opens the adapter itself, through a proxy it can withhold and cut (TCP or UDP in `internal/conformance/proxy.go`), and `internal/conformance/suite_test.go` runs the nine cases against it in-process. A fake vehicle for the tests is a package, as `adapters/mavlink/mavlinktest` is, so the adapter's tests and the suite share it.
4. **`keelctl vector`** gains a flag for the new target in `cmd/keelctl/vector.go`.
5. **`spec.md` section 7** gains the adapter's section, as 7.3 and 7.4 did, and `design.md` section 7 its reasoning.

---

## 6. Passing the nine conformance cases

```sh
keelctl vector validate --native ws://127.0.0.1:8091/v1/vectors/SCOUT-01 --allow-motion
keelctl vector validate --mavlink 127.0.0.1:14550 --sysid 1 --vector DRONE-01 --allow-motion
```

`keelctl vector describe` (capabilities, declared types, health and the first frame) and `keelctl vector plug` (frames and health changes until interrupted) take the same target flags and are the first things to run against a new vehicle.

**How the suite runs** (`spec.md` section 13):

- **It owns the transport.** The suite opens the adapter itself, through a proxy: TCP for a native vehicle, UDP for a MAVLink one, where it listens on the address the vehicle pushes to (stop keeld first, nothing else may hold the port). Withholding telemetry and cutting the link then need nothing from the vehicle.
- **Order**: C1, C2, C3, C4, C5, C8, C6, C7, C9. Cases needing a healthy link first, the ones breaking it after, `Close` last.
- **Motion needs consent.** C3, C4 and C5 command the vehicle, and a landed copter takes off. They run only with `--allow-motion`, and a skipped case is not a pass.
- **`seq` starts above the vehicle's `ack_seq`**, so a vehicle that ran under another orchestrator is not handed commands it would filter.
- **`--degraded-after` and `--lost-after`** (5 s and 10 s) configure the adapter and C6 alike.
- **Exit status**: `0` all nine passed, `2` a case failed or was skipped, `1` the suite could not run.

One line per case, then a verdict: `conformant: all 9 cases passed`, or `not conformant:` with the cases that failed or were skipped.

| Case | What the suite does | Usual cause of a failure |
|---|---|---|
| **C1** Capability declaration | Reads `Describe()` | A zero or negative speed or range, a domain other than `aerial` or `ground`, a hello that does not decode |
| **C2** Telemetry schema | Checks 100 consecutive frames within a minute: finite positions, physical modes, `last_seen_ms` never decreasing | Frames below 2 Hz; `scanning` or `down` reported by the vehicle; a mission clock of the vehicle's own that restarts (send `0`) |
| **C3** Command acknowledgement | Sends a `goto` 50 m north (10 m higher for an aerial vehicle) and waits for mode `transit` | The vehicle reports `idle` while moving, or takes more than 2 s to start |
| **C4** Idempotency | Waits up to 30 s for `ack_seq` to reach the `goto`, re-sends it, and requires `ack_seq` and mode unchanged for a second | `ack_seq` never set; a re-sent `seq` applied again, restarting the manoeuvre or reported as an error |
| **C5** Sequence gaps | Skips one `seq` and sends a `hold`, which must be acknowledged within 30 s | The vehicle waits for the missing `seq`, or rejects the gap |
| **C8** Capability honesty | Sends a command of no known type and every undeclared type | Only fails if the adapter accepts one; a native vehicle's part is a `supports` that states what it really executes |
| **C6** Timeout | Withholds telemetry: `Health` must report `degraded` at 5 s and `lost` at 10 s, each within 1 s, then telemetry must resume when released | Telemetry not resuming after the pause (a vehicle that closed the connection on backpressure) |
| **C7** Reconnect | Cuts the link: the adapter must reconnect and telemetry resume within 30 s | The vehicle refusing a new connection while it holds the old one; a hello that changed between connections |
| **C9** Clean shutdown | Closes the adapter: the telemetry channel must close within 5 s, a second `Close` return `nil`, and no goroutine leak | An adapter bug (path 3); nothing the vehicle does |

For a native vehicle, C1, C6, C8 and C9 exercise KEEL's adapter as much as the vehicle, which is the point: the same suite validates keelsim, an ArduPilot SITL and hardware through the same adapters keeld runs. For a path 3 adapter, all nine are its own.

**Against keelsim**, the reference to compare a vehicle with: `keelsim --serve` serves the reference fleet on `127.0.0.1:8090`.

```sh
go run ./cmd/keelsim --serve
go run ./cmd/keelctl vector validate --native ws://127.0.0.1:8090/v1/vectors/DRONE-02 --allow-motion
```
