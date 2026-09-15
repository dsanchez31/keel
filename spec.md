# KEEL Specification

Version 0.1.0, status: draft.

This document defines *what* KEEL does. `design.md` defines *how* and *why*.

---

## 1. Purpose and scope

KEEL orchestrates a fleet of heterogeneous autonomous systems performing systematic reconnaissance of an area, under operator intent expressed in natural language, in an environment where communications degrade and vehicles fail.

**In scope:**

- Compiling natural language intent into an inspectable, validated mission plan.
- Deterministic execution of that plan, with reactive adaptation governed by a versioned doctrine.
- Integration of third party vehicles through a single interface, over more than one transport.
- Full recording and replay of a mission, with cryptographic proof that replay reproduces the original decisions.

**Out of scope:** weapons, effectors, strike coordination, authentication, multi tenancy, and any domain other than aerial and ground.

---

## 2. Glossary

| Term | Definition |
|---|---|
| **Vector** | Any autonomous system participating in a mission. Aerial or ground. Named after LE_VECTOR's own usage. |
| **Capability** | A declared property of a vector (`aerial`, `ground`, `camera`, `radio_mesh`) used to match it to a role. |
| **Area of Interest (AO)** | A closed WGS84 polygon defining where a mission takes place. |
| **Cell** | One square of the raster grid covering the AO. Carries an `explored` flag. |
| **Fog of war** | The set of unexplored cells in an AO. |
| **Lane** | A contiguous strip of the AO assigned to exactly one vector, with an ordered waypoint sequence. |
| **Intent** | Free text from the operator, for example "Grid-search the unexplored area". |
| **Plan IR** | The intermediate representation the planner emits. Declares tactic, never geometry. |
| **Plan** | A Plan IR expanded into concrete lanes and waypoints, validated and content addressed. |
| **Doctrine** | A versioned pack of roles, constraints and reactive rules. |
| **Decision** | A record of one orchestration choice, with its full rationale. |
| **Tick** | One step of the logical clock. Fixed at 100 ms of simulated time. |

---

## 3. Coordinate and unit conventions

- Positions are WGS84: `lat` and `lon` in decimal degrees, `alt_m` in metres above the WGS84 ellipsoid.
- MGRS is a **display format only**. It never appears in stored state or on the wire.
- Distances in metres, speeds in metres per second, bearings in degrees clockwise from true north in `[0, 360)`.
- Battery is an integer percentage in `[0, 100]`.
- Durations on the wire are integer milliseconds of **mission time**, never wall clock.
- Mission time starts at 0 on the first tick and advances by exactly one tick interval per tick.

---

## 4. Domain model

### 4.1 Vector

```go
type VectorID string

type Capabilities struct {
    ID           VectorID  // stable, unique within a mission
    Domain       Domain    // "aerial" | "ground"
    Tags         []string  // sorted; e.g. ["camera", "gps"]
    CruiseSpeed  float64   // m/s
    MaxRangeM    float64   // m, at cruise on a full battery
    SensorRadiusM float64  // m, ground footprint radius
}
```

`Tags` MUST be sorted lexicographically. Two capability sets with the same members in a different order are the same set, and canonical encoding depends on it.

### 4.2 Vector state

```go
type VectorState struct {
    ID         VectorID
    Position   Position
    Heading    float64     // degrees
    Speed      float64     // m/s
    BatteryPct int
    Link       LinkState   // "ok" | "degraded" | "lost"
    Mode       VectorMode  // "idle" | "transit" | "scanning" | "rtb" | "down"
    LastSeenMs int64       // mission time of the last accepted telemetry
    AckSeq     uint64      // last command Seq the vehicle applied, 0 before any
}
```

**`LastSeenMs` 0 means the vehicle knows no mission time.** An autopilot, or a simulator whose clock started apart from the engine's, sends 0, and the engine stamps the frame with the tick that accepts it, before comparing it with the frame it holds. A frame carrying an explicit time older than the one held is dropped: a duplicated or reordered frame cannot walk a vector backwards.

**`AckSeq` is the vehicle's word, not a maximum.** Every frame carries it as the vehicle states it, and the engine and the adapter read it afresh from the latest frame: a vehicle that restarts reports 0 again, and the commands standing for it are re-sent (section 7.2).

**Physical and orchestration modes.** Telemetry reports what the vehicle is physically doing: `idle`, or `transit` toward a commanded point. The engine derives the rest at the start of every tick, because only it knows what the point is for:

- `scanning`: the vector is moving and its active lane (section 4.4) is **engaged**, meaning that under its current assignment it has reached a waypoint of the lane while inside an in-AO cell. Before that it is in `transit`, so a vector crossing open ground from a GCS outside the AO to its lane does not breach the geofence.
- `rtb`: the vector's standing command is `rtb`, whatever its frames say. A frame sent before the command arrived cannot make it available for work again.
- `down`: set by the engine on a kill or a vector leaving, never by telemetry.

A frame reporting `scanning` is read as `transit`.

### 4.3 Mission

```go
type Mission struct {
    ID        MissionID
    Intent    string
    Area      Area          // the AO polygon plus its raster grid
    Doctrine  DoctrineRef   // name and version, e.g. recon-standard@2.1.0
    Plan      PlanHash      // the approved plan
    State     MissionState  // "planning" | "awaiting_approval" | "running" | "complete" | "failed"
    StartedMs int64
}
```

### 4.4 Lane

```go
type Lane struct {
    ID        LaneID
    Index     int          // 0-based, ordered along the sweep axis
    Cells     []CellID     // sorted
    Waypoints []Position   // ordered, the boustrophedon path
    AssignedTo VectorID    // empty when unassigned
}
```

The cells of all lanes are pairwise disjoint (I1). A vector may hold several lanes: it works them in ascending `Index` order, the later ones queued behind the first. Redecomposition (section 6.5.1) is what hands a busy vector a second lane.

- A vector's **active lane** is the lowest-`Index` lane it holds with a waypoint left. Only the active lane is driven; a vector whose lanes are all walked is sent `hold`.
- Each lane has a cursor, the index of the next waypoint. It advances past every waypoint within `ArrivalRadiusM` of the vector's position, which bounds how far a vector cuts a corner. `ArrivalRadiusM` plus `PositionErrorM`, the horizontal error the engine budgets on a reported position (5 m and 5 m by default), must stay below the route clearance of the plan's grid (section 5.5 step 7): gate 3 refuses an AO that does not leave room for them (section 5.3), and the engine refuses the approval of such a plan, the mission staying `planning` and the decision giving the clearance and both terms (section 4.5). Releasing a lane keeps its cursor, so whoever takes it next starts where its previous owner stopped.
- Lane ids are never reused within a mission: the engine carries the first index never used, and redecomposition numbers from it.

### 4.5 Engine events

Everything that reaches the engine is an `Event`: `vector_joined`, `telemetry`, `link_changed`, `fault_injected`, `vector_left`, `plan_approved`, `doctrine_swap`, `operator_abort`.

- **`vector_joined` carries `Capabilities` and the vector's initial `VectorState`, both required.** A join without a state, or with a position that is not finite, is refused with a decision. A vector the engine knows nothing about would otherwise sit at zero battery on the point (0, 0), and doctrine would react to it. A known vector joining again is re-admitted with the state it carries: that is how a vector written off comes back.
- **A tick's events are applied in a declared kind precedence**, then by vector id, stably: `vector_joined`, `telemetry`, `link_changed`, `fault_injected`, `vector_left`, `plan_approved`, `doctrine_swap`, `operator_abort`. What vehicles report comes first, the transport's verdict and the operator's injections override it. A frame already in flight when a link was cut cannot undo the loss, nor a frame sent before a battery collapse undo the drain.
- **A `plan_approved` whose grid leaves no room for the arrival budget is refused**: its route clearance must exceed `ArrivalRadiusM` plus `PositionErrorM` (section 4.4). Gate 3 checked it against the budget it was given; this is the budget the mission would run under.
- **Doctrine is pinned by hash.** A `plan_approved` carries the pack's expected hash in the plan (`DoctrineHash`, section 5.6), a `doctrine_swap` beside the reference (`DoctrineHash` on the event). Both are required. A reference the registry does not resolve, an empty expected hash, or a registry pack whose hash differs is refused: for a plan the mission stays `planning`, and the decision names the reference and, for a mismatch, both hashes. Replayed against a registry whose pack was edited under the same version, a log therefore stops at a refusal naming the pack instead of diverging silently.
- A `kill` fault and a `vector_left` release the vector's lanes. A vector in mode `down` is outside doctrine evaluation, so no rule would ever release them.
- **An `operator_abort` fails the mission and stops what it set in motion.** Every vector whose standing command is a `goto` is sent an `abort` in the same tick, and the `mission_state` decision names them. A vector holding is stopped already; one returning to base keeps returning, since stopping it would strand it on the battery it went home to save; one in mode `down` is left alone. A failed mission issues nothing more, the abort included: it is not re-sent.

---

## 5. Plan IR

The planner (an LLM) emits Plan IR. It is deliberately small, declarative, and **contains no coordinates**.

### 5.1 Schema

```yaml
apiVersion: keel.plan/v1        # required, exact match
intent: string                  # required, the operator's raw text, echoed back
doctrine: string                # required, "<name>@<semver>"
mission:
  type: enum                    # required: systematic_reconnaissance | perimeter_patrol
  priority: enum                # required: low | normal | high
  area: string                  # required, a named AO known to the server
tactic:
  pattern: enum                 # required: parallel_lanes | spiral | perimeter
  orientation: enum             # required for parallel_lanes: north_south | east_west | long_axis
  lanes: integer                # required for parallel_lanes, 1..16
  overlap_pct: integer          # optional, 0..50, default 10
assignment:
  policy: enum                  # required: nearest_capable | round_robin | lowest_cost
  requires: [string]            # required, capability tags, sorted
  gcs: string                   # optional, a named ground station
rationale: string               # required, non-empty, operator-facing prose
```

- Every string (`intent`, `doctrine`, `area`, `gcs`, `rationale`, each tag) holds at least one non-space character.
- `requires` holds at least one tag and no duplicate. It is a set: it is sorted and deduplicated when the plan is decoded, so the order the model wrote it in never reaches the plan or its hash.
- `orientation` and `lanes` are required for `parallel_lanes` and optional for `spiral` and `perimeter`, which have no expansion (section 5.5).
- `intent` equals the operator's text, leading and trailing whitespace aside. A rewritten intent fails gate 2: the human gate shows the plan against the words the operator actually typed.
- The canonical schema is one JSON Schema 2020-12 document, in `internal/planir/schema.go`. The backends are given a **model schema derived from it** in two steps. First, `tactic.pattern` is narrowed to the patterns expansion supports (section 5.5, today `parallel_lanes`): offering the others only spends an attempt on a gate 3 refusal, and they return to the model once expanded. With one pattern left, the conditional that tests it is decided, so `orientation` and `lanes` become plain required properties. Second, the keywords the backends' structured-output modes reject are removed (`minimum`, `maximum`, `minLength`, `pattern`, `uniqueItems`, `if`/`then` and similar), every bound restated in its property's description. Gate 1 always validates against the canonical schema.

### 5.2 Hard constraints on the planner

These are enforced by the validator and are the reason the architecture is safe:

- **`area` and `gcs` are names, not geometry.** The server resolves them against its own registry. A name it does not know is a validation failure, never an improvisation.
- **No coordinate may appear anywhere in Plan IR.** The schema has no field capable of carrying one. This is structural, not advisory.
- **`lanes` is bounded** to `1..16` regardless of what the model proposes.
- **`rationale` is required and non-empty.** A plan with no stated reasoning is rejected.
- **Unknown fields are rejected.** The schema is closed.

### 5.3 Validation pipeline

A Plan IR passes through four gates in order. The first gate that fails stops the pipeline and reports **every** violation it found, as structured diagnostics sorted by pointer, code, subject and message. The same input yields byte-identical diagnostics.

```go
type Diagnostic struct {
    Gate         Gate        // "schema" | "resolution" | "feasibility" | "doctrine"
    Code         string      // stable snake_case identifier, e.g. "unknown_area"
    Pointer      string      // RFC 6901 location in the Plan IR, when one applies
    Message      string
    Subject      string      // the lane, vector or constraint concerned
    Alternatives []string    // sorted names that would have resolved (gate 2)
    Candidates   []Candidate // every vector considered for an unassignable lane (gate 3)
}
```

The names resolve against the **world**: a `keel.world/v1` YAML document holding the named AOs (polygon, `cell_m`, `scan_alt_m`), the named ground stations and a snapshot of the fleet (capabilities and last state). Its schema is closed, like a doctrine pack's, and every AO is rasterised when the world loads, so an AO whose raster is empty or disconnected is refused there. The reference world is `examples/worlds/reference.yaml`.

1. **Schema.** Closed JSON Schema validation of the reply's bytes, before any decoding. A reply that is not one JSON document fails with `invalid_json`. Every other violation yields the offending JSON pointer and, as its code, the keyword violated. A missing or unexpected property points at the property itself.
2. **Resolution.** `doctrine`, `mission.area` and `assignment.gcs` must resolve, and `intent` must echo the operator's text. Failure names the unresolved reference and lists valid alternatives. The pack `doctrine` resolves to is pinned in the plan by its hash (section 5.6).
3. **Feasibility.** Computed after deterministic expansion (section 5.5) over the fleet snapshot:
   - The AO's route clearance, a quarter of its `cell_m`, exceeds the arrival budget of the engine the plan would run under, `ArrivalRadiusM` plus `PositionErrorM` (section 4.4), checked first: `clearance_too_small` at `/mission/area`. The validator is given that budget; one that is not a finite positive radius and a finite non-negative error is `invalid_arrival_budget`, never read as none.
   - At least one available vector (`Link != lost`, `Mode` neither `down` nor `rtb`) satisfies `assignment.requires`, and at least `tactic.lanes` of them do.
   - Expansion succeeds: a pattern with no expansion, a lane holding no cell and an invalid tactic are each a diagnostic.
   - Every waypoint lies in an in-AO cell.
   - **The allocator of section 5.7 runs with the plan's policy**, stations limited to the named GCS, and every lane gets a vector. An unassignable lane is a diagnostic carrying every candidate with its rejection reason. Gate 3 and launch therefore apply the same range check, `MaxRangeM` derated by the pack's `params.battery_reserve_pct`, and a plan that passes cannot leave a lane unassigned against the snapshot it was validated on.
4. **Doctrine constraints.** Every constraint in the active pack is evaluated for every vector of the fleet not in mode `down` (the scope of I4), against the projected initial state: the snapshot, the lanes assigned as gate 3 allocated them, the mission `awaiting_approval`, nothing explored. A violation names the constraint and the vector.

### 5.4 Triage and repair loop

**Triage.** Before the first attempt, one short call to the same backend asks whether the intent asks for a mission at all: an area to reconnoitre, search, sweep, survey or patrol, named or not. A question, a greeting, a status request or an order to a single vehicle is not one. Its conversation is its own (a fixed system prompt carrying no world, the intent as the only turn) and so is its closed schema, `{reason, mission}` in that order, so the model states what is asked before it decides.

- `mission: false` ends the compilation: no plan, no attempt, the outcome's `triage` carrying the model's `reason`. Declining is fail closed: nothing moves, and the operator reads why.
- A reply that is not that closed document, or that declines without a reason, is recorded as `unreadable` and the intent is compiled like a mission. In doubt, the guarded path: the gates and the human gate.
- The triage is not an attempt and proposes no plan. A backend failure during it ends the compilation like any other (below).

**Repair loop.** On failure at gates 1 to 4, the diagnostic is fed back to the planner as a structured message and a new Plan IR is requested. **At most 3 attempts.** After the third failure the operator sees the accumulated diagnostics and no plan is produced. The repair loop never relaxes a constraint to force success.

- The conversation grows by two turns per repair: the refused reply as the model's turn, then the diagnostics in their canonical encoding (section 9) as the next user turn.
- Every attempt is validated against the same world, packs and intent. Only the reply can change.
- **A backend failure ends the compilation**: network error, timeout, refusal after the backend's own fallbacks, truncated output. It is not a plan defect, so it spends no attempt, and the attempts made so far are returned with the error.
- **Thinking is off by default** (`planner.think`, section 16.1): Ollama is asked not to think, and Claude thinks adaptively at low effort rather than not at all. The validator judges the reply either way; thinking buys a better first attempt at the price of latency.

### 5.5 Expansion

Expansion is **deterministic and LLM free**. Given a Plan IR and a resolved AO, `coverage.Decompose` produces the same lanes and the same waypoints on every invocation, on every machine.

**The raster.** The AO is covered by square cells of a fixed edge length. The origin is the south-west corner of the polygon's bounding box and the reference latitude is the middle of that box, so neither depends on the vertex the ring starts from. A cell is in the AO when its centre lies inside the polygon. The in-AO cells MUST form one 4-connected region; a polygon whose raster does not is refused when the grid is built.

**The swath and the altitude.** Expansion takes two parameters the Plan IR does not carry. `SwathM`, the maximum spacing between adjacent passes, is twice the smallest `SensorRadiusM` among the available vectors satisfying `assignment.requires`, so whichever of them flies a lane leaves no gap between passes; a zero footprint fails gate 3. The waypoint altitude is the AO's `scan_alt_m` from the world. Geometry stays out of the model's hands. Both are fixed in the approved plan as `SwathM` and `ScanAltM`, and redecomposition (section 6.5.1) sweeps with them: lanes cut mid mission have the geometry the human approved, not one derived from what the fleet has become.

For `pattern: parallel_lanes`:

1. Work in a local planar frame: metres east and north of the grid origin, equirectangular at the reference latitude.
2. Take the lane direction from `orientation`: north for `north_south`, east for `east_west`, and for `long_axis` the longer side of the minimum-area rectangle enclosing the AO's convex hull (ties to the lowest hull edge index). The direction is canonicalised to point north, or east when horizontal. The across-lane normal points east, or north when vertical, and lane 0 is the one with the lowest coordinate along it.
3. Project the AO onto the normal and divide the span into `lanes` equal strips. A cell belongs to the lowest-index strip whose closed interval contains its centre, so the lanes partition the AO and no cell has two owners (I1). A strip holding no cell centre fails expansion with an error naming the lane.
4. `overlap_pct` widens each strip by `overlap_pct / 2` percent of its width on each side, clipped to the span. It widens what the lane's passes sweep, never what the lane owns.
5. Cut the widened strip into the fewest equal bands no wider than `SwathM`. Each band is one pass. The in-AO cells of a band, ordered along the lane by position then `CellID`, split into runs wherever two consecutive cells are more than 1.5 cells apart, which is where a pass crosses a concavity. Each run is swept so that **every cell of the run lies within `SwathM / 2` of the path**, the reach of the narrowest footprint:
   - The pass runs along the band's centre line, which every cell of the band is within half the band's width of. The line is walked from the run's first cell to its last (the lowest `CellID` among those furthest along) in steps of an eighth of a cell, and every stretch at least one cell long that keeps the route clearance (step 7) becomes a leg of the pass, entered and left at its ends. A run whose centre line has no such stretch is swept from its first cell to its last instead.
   - Where the AO boundary crosses the band at a slant, cells before the first leg or after the last fall out of reach. Each such cell, the farthest from the path first (lowest `CellID` among equals), is added as a waypoint at its place along the lane, until none is farther than `SwathM / 2`.
   - A lane visits the centre of at least one cell it owns: when its passes visit none, the owned cell nearest the end of the path is appended (the progress guarantee of section 6.5.1).
6. Passes alternate direction. A lane with an even `Index` starts in the lane direction and an odd one against it, so consecutive lanes are traversed in opposite senses.
7. **Every waypoint lies in an in-AO cell, and every segment keeps a clearance of a quarter cell from every cell outside the AO**, the space beyond the raster counting as outside. The clearance is checked exactly, segment against cell square, never by sampling: a segment grazing the corner of an outside cell passes every sample and fails the check. A segment short of the clearance is replaced by the shortest 8-connected path over in-AO cell centres (a diagonal step only where both cells it passes between are in the AO, neighbours tried in the order N, E, S, W, NE, SE, SW, NW), whose steps keep at least half a cell, then shortened by line of sight under the same check. A vector cuts corners by up to its arrival radius and reports its position with some error; the clearance is the budget for both, so the geofence (I4) never trips on the system's own route. The engine's `ArrivalRadiusM` plus its `PositionErrorM` MUST stay below it (5 m plus 5 m by default against 12.5 m on 50 m cells, so cells of 40 m and under are refused), which gate 3 and the engine at approval both check through one rule, `coverage.Arrival.Check` (section 4.4). The default error is a 95 percent horizontal bound for consumer GNSS: 2.5 m CEP on u-blox M8-class receivers, R95 about 2.08 times CEP.
8. Lanes are identified `lane-00`, `lane-01` and so on, with `Cells` sorted.

Every tie is broken by lane index, then by `CellID`. There is no randomness anywhere in expansion.

`spiral` and `perimeter` are valid Plan IR values with no expansion yet. Expansion fails with an unsupported pattern error, which gate 3 reports like any other infeasibility.

### 5.6 Content addressing

An approved plan is serialised with the canonical encoder (section 9) and hashed with SHA-256. `PlanHash` is the lowercase hex digest. Two plans with the same hash are byte identical.

- The hash covers every field of the plan except `Hash` itself and `Mission`, the mission id, which is assigned when the plan is approved. `SwathM` and `ScanAltM` are covered.
- `DoctrineHash`, the hash of the pack gate 2 resolved `Doctrine` to (section 6.1), is covered. The plan names the rules it was validated against, not only their version: another pack under the same reference is another plan, and the engine refuses to start it (section 4.5).
- The plan's lanes are **unassigned**. The engine allocates them at launch against the fleet as it is then. The allocation gate 3 projected is returned beside the plan for display and is not part of it or of its hash.

### 5.7 Assignment

Allocation matches lanes to vectors. One allocation round gives each lane at most one vector and each vector at most one lane of that round. A vector already holding lanes may take one more, queued after them (section 4.4). Rounds run:

- **After every redecomposition**, over the new lanes, under the action's policy.
- **On every tick of a running mission with an unassigned lane**, over those lanes, under the plan's policy. On the tick the plan is approved this is the launch round. Afterwards it reassigns what a vector leaving, a kill or a rule without `redecompose` released, as soon as a vector can take it, which is what bounds I3. A lane whose cells are all explored, or whose waypoints are all walked (section 6.5.1), is not offered.

The launch round records `assignment` decisions, every later round `reassignment` decisions. A lane no vector can take is recorded once per stretch of being unassigned, not on every tick it is retried.

Eligibility is checked in this order, and the first failure is the reason the vector is rejected:

| Check | Rejection reason |
|---|---|
| `Link == lost` | `link lost` |
| `Mode == down` | `vector down` |
| `Mode == rtb` | `returning to base` |
| No finite position | `no known position` |
| A tag of `assignment.requires` not declared | `missing capability <tag>`, the first missing tag in sorted order |
| Range need above derated range | `insufficient range: needs <n> m, has <m> m above the <r>% reserve` |

The **range need** is the distance the vector still owes to the lanes it holds, plus the transit from where it becomes free (its position, or the last waypoint of its last held lane) to the lane's first waypoint, plus the lane's length, plus the return from the lane's last waypoint to the nearest GCS (back to where it became free when the plan names no GCS). The **derated range** is `MaxRangeM × (BatteryPct − reserve) / 100`, floored at 0, with the reserve taken from the active pack's `params.battery_reserve_pct` (section 6.1). Distances are great-circle metres. This is the same quantity gate 3 checks.

| Policy | Rule | Candidate cost |
|---|---|---|
| `nearest_capable` | Lanes in `Index` order, each to the eligible vector with the shortest transit, ties on `VectorID` | Transit |
| `round_robin` | Eligible vectors in id order take lanes in turn; a vector that cannot fly a lane passes its turn | Transit |
| `lowest_cost` | The assignment minimising the summed range need over the round, solved exactly (Kuhn-Munkres on integer millimetres); it first maximises the number of lanes assigned | Range need |

Every vector of the fleet appears among a lane's candidates. Only the winner is accepted. A loser that passed eligibility carries one of: `assigned <lane> in this round`, `farther than <winner>: <x> m vs <y> m`, `not next in the rotation`, `not in the minimum total cost assignment`, or `not selected`, which only a policy leaving a lane unassigned while a free eligible vector exists would give: no shipped policy does, and the reason keeps the trace complete if one ever did. A lane no vector can take stays unassigned and says so.

**The stall.** A running mission is being worked while an eligible vector (the first three checks and the capability check above) holds a lane with a waypoint left. A tick in which none does is a stall tick, whatever the reason: no vector eligible, or pending work the range check refuses to every eligible vector. After `StallDeadlineTicks` consecutive stall ticks (60 s by default) the mission fails, the `mission_state` decision giving the eligible count and the lanes still pending. The stall waits while any lane is being worked: a mission that can still explore is not failed for the lanes it cannot, and one left with work none of its fleet can reach fails a minute after its last worked lane rather than at the tick ceiling.

---

## 6. Doctrine language

### 6.1 Pack structure

```yaml
apiVersion: keel.doctrine/v1
name: recon-standard
version: 2.1.0                  # semver, required
params:
  battery_reserve_pct: 15       # required, 0..100
roles:
  - id: scanner
    requires: [aerial, camera]  # capability tags, a set
constraints:
  - id: battery-reserve
    rule: agent.battery_pct >= agent.rtb_cost_pct + doctrine.battery_reserve_pct
  - id: geofence
    rule: agent.mode != SCANNING or agent.position within mission.area
rules:
  - id: reassign-on-link-loss
    when: agent.link_state == LOST for 5s
    then: release_lanes(agent), redecompose(policy=nearest_capable)
    priority: 100
```

- **The schema is closed.** An unknown field, a duplicate key or a second YAML document is rejected.
- `apiVersion` is exactly `keel.doctrine/v1`. `name` is lowercase kebab-case.
- `version` is strict SemVer 2.0.0: `MAJOR.MINOR.PATCH` with an optional pre-release, no `v` prefix, no build metadata, no shorthand. Versions order by SemVer precedence. A pack is referenced as `<name>@<version>`.
- `params` is a closed set of typed values. Code (allocation, gate 3) and expressions (`doctrine.<param>`) read the same value, so a number like the battery reserve has one source. `battery_reserve_pct` is required, in `[0, 100]`.
- Every `id` of roles, constraints and rules is lowercase kebab-case and unique across all three.
- A role's `requires` is non-empty, sorted and deduplicated at construction.
- Constraints need `id` and `rule`. Rules need `id`, `when`, `then` and an integer `priority`.
- **Every expression and action list is compiled and type checked at load.** A pack that loads cannot fail at evaluation. An error names the section, the id, the YAML line and the column.
- **Content address.** Roles, constraints and rules are sorted by id and expressions kept as source text. The pack hash is the lowercase hex SHA-256 of the canonical encoding (section 9) of that document. Comments, whitespace and list order do not change it; any change of meaning does.
- Shipped packs live in `doctrine-packs/<name>-v<version>.yaml`.

### 6.2 Expression grammar

```
expr      := or_expr
or_expr   := and_expr ( "or" and_expr )*
and_expr  := cmp_expr ( "and" cmp_expr )*
cmp_expr  := sum ( ("=="|"!="|"<"|"<="|">"|">=") sum )
           | sum "within" sum
           | "violates" "(" ident ")"
sum       := operand ( ("+"|"-") operand )*
operand   := path | number | string | enum_literal
path      := ident ( "." ident )*
condition := expr ( "for" duration )?
```

- `ident` is `[a-z][a-z0-9_]*`. `and`, `or`, `within`, `violates` and `for` are reserved.
- `number` is `[0-9]+(\.[0-9]+)?`. There is no unary minus.
- `string` is double quoted. The only escapes are `\"` and `\\`.
- `enum_literal` is `[A-Z][A-Z0-9_]*`, the upper-case spelling of an enum value (`LOST` is `lost`).
- `duration` is `<integer>(ms|s|m)`, positive and a whole number of ticks (100 ms). It is valid only after `for`.
- There are no parentheses and no negation. `and` binds tighter than `or`. No user defined functions, no loops, and no arithmetic beyond `+` and `-` on numbers. The language is deliberately not Turing complete.

**Vocabulary.** Every path an expression may read:

| Path | Type |
|---|---|
| `agent.id` | string |
| `agent.domain` | enum `AERIAL`, `GROUND` |
| `agent.battery_pct` | number |
| `agent.rtb_cost_pct` | number: battery percent needed to reach the nearest GCS, `ceil(distance × 100 / MaxRangeM)`; 0 when the plan names no GCS |
| `agent.link_state` | enum `OK`, `DEGRADED`, `LOST` |
| `agent.mode` | enum `IDLE`, `TRANSIT`, `SCANNING`, `RTB`, `DOWN` |
| `agent.position` | position |
| `agent.speed` | number |
| `mission.area` | area |
| `mission.coverage_pct` | number, `[0, 100]` |
| `mission.state` | enum `PLANNING`, `AWAITING_APPROVAL`, `RUNNING`, `COMPLETE`, `FAILED` |
| `lane.id` | string |
| `lane.index` | number |
| `lane.progress_pct` | number: waypoints reached over waypoints, `[0, 100]` |
| `doctrine.battery_reserve_pct` | number |

`agent` is the vector under evaluation, `lane` the lowest-`Index` lane it holds, `doctrine` the pack's `params`.

**Typing, checked at load.**

- `+` and `-` take numbers.
- `<`, `<=`, `>`, `>=` compare numbers. `==` and `!=` compare two values of the same type. Positions and areas are not comparable.
- An enum literal is compared with `==` or `!=` against a path of an enum it belongs to, never against another literal.
- `within` takes a position on its left and an area on its right. It holds when the position lies in a raster cell of the AO (section 5.5): the geofence is checked at the resolution coverage routes are guaranteed at, so the system's own routes never break it at a concave corner.
- `violates(<id>)` holds when the agent does not satisfy constraint `<id>` of the same pack. It is allowed in a rule's `when` only, never in a constraint, so the language has no recursion.

**Absent values.** An agent holding no lane has no `lane.*`. `agent.position` is absent when not finite, and `agent.rtb_cost_pct` when the position is not finite or the vector declares no range. Every comparison reading an absent value is false, `!=` included, and so is `within`. A condition on `lane.*` therefore never holds for a free agent.

Numbers compare exactly. A sum is evaluated left to right in source order.

### 6.3 Duration windows

`when: <cond> for <duration>` fires only once `<cond>` has held continuously for `<duration>` of **mission time**, measured from the tick it was first observed true: observed true at `t`, a `5s` rule fires at the first tick `≥ t + 5000`. A rule with no `for` fires on the tick its condition becomes true.

**A rule fires on an edge.** It fires once for a stretch of truth, and again only after its condition has been false for at least one tick.

The evaluator keeps, per `(rule, agent)` pair whose condition holds, the mission time it first held and whether the rule has fired. Any tick where the condition is false drops the entry, which clears the window and re-arms the rule. The state is a list sorted by rule id then agent, carried in engine state; entries for agents no longer evaluated and for rules the pack does not declare are dropped.

### 6.4 Priority and shadowing

Doctrine runs on every tick of a running mission, after the tick's events and before allocation. Constraints are evaluated first, then rules, for every agent in id order: every vector that has joined and is not in mode `down`, the scope of gate 4 and I4. **Priority is resolved per agent**: two agents firing on the same tick do not shadow each other.

Each firing is recorded as a `doctrine_rule` decision naming the agent, the rule, every rule it shadowed, and what each action did. The decisions its actions produced (redecomposition, reassignments, operator notices) follow it, so a log reads cause before effect.

Of the rules ready to fire for one agent, sorted by `priority` descending then `id` ascending, the first executes. Every other ready rule is recorded in the decision trace as **shadowed**, with the id of the rule that shadowed it. Shadowing is reported, never silent. A shadowed rule consumes its edge: it does not fire on the next tick in the winner's place.

### 6.5 Actions

`then` is a comma-separated list of calls, executed in the order written, each action at most once per rule. Every call is checked against its signature at load.

| Action | Effect |
|---|---|
| `release_lanes(agent)` | Marks the agent's lanes unassigned, retaining explored cells |
| `redecompose(policy=<policy>)` | Pools all unassigned uncovered cells and redistributes them across eligible vectors. `policy` is required: `nearest_capable`, `round_robin` or `lowest_cost` |
| `return_to_base(agent)` | Sets mode `rtb` and sends an `rtb` command whose waypoint is the nearest GCS, no waypoint when the plan names none. A vector in mode `rtb` takes no more work (section 5.7) |
| `notify_operator(message="...")` | Emits an operator-facing event, no state change |

The agent an action applies to is the agent the rule fired for. The doctrine evaluator returns actions as descriptors; the engine carries them out, because their effects touch lanes, cursors and commands only engine state holds.

#### 6.5.1 Redecomposition

- **The pool** is the set of uncovered cells of every unassigned lane. Those lanes are retired: their explored cells stay explored (I2), the lanes themselves are gone. Assigned lanes are left exactly as they are, so a working vector keeps its lane and its progress along it.
- **The pool is cut into `k` new lanes**, `k` being the number of eligible vectors, capped at the pool size and at 16. The cut runs across the pool's own long axis (the minimum-area rectangle of the pooled cell centres, as in section 5.5 step 2) into strips of equal cell count, give or take one, ordered by position across the axis then by `CellID`. No new lane is empty and the work is balanced. There is no overlap.
- **Each new lane is swept** as in section 5.5 steps 5 to 8, with the plan's `SwathM` and `ScanAltM`, and numbered from the first lane index never used in the mission (section 4.4).
- **Every new lane visits the centre of at least one pooled cell**, which the sensor footprint of the vector flying it explores. Each round therefore strictly shrinks the fog, and repeated redecomposition converges instead of reshuffling: the livelock I1 and I3 together forbid cannot occur.
- The new lanes are then allocated (section 5.7) under the action's `policy`.
- With no eligible vector the redecomposition is deferred: the pool stays unassigned and a decision says so.

**Residue of a walked lane.** Expansion puts every cell within half a swath of its lane's path (section 5.5 step 5), which is the footprint's edge itself: a position reported a metre off, or a detour around a concavity, can leave a cell just outside every footprint. A lane walked to its last waypoint with cells still uncovered is released, with an operator notice naming it, and once a vector is eligible the engine redecomposes under the plan's policy. The redecomposition names the walked lanes as its reason. By the property above the residue strictly shrinks, which is what keeps I8 from resting on the geometry being gapless.

### 6.6 Hot swap

A pack may be replaced while a mission is running. Semantics:

- The swap takes effect at a **tick boundary**, never mid tick.
- The mission, its plan, its lane assignments and its coverage state all survive. A swap MUST NOT restart or drop a mission.
- Duration window state is keyed by `(rule id, agent)` and is **retained**, start time and fired flag both, when a rule of the same id exists in the new pack, so a condition that has held for 3 seconds does not restart its clock. A retained window fires at its original start plus the new pack's duration.
- Retention looks at the id only, not at the condition. A rule whose condition changes under the same id keeps the start observed under the old condition, and can fire before the new condition has held for its window. A pack author narrowing a windowed condition gives the rule a new id (design section 10).
- Duration window state for rules absent from the new pack is discarded.
- The swap emits a `doctrine_swap` decision whose `Swap` field carries both references, both hashes and the retained and discarded windows, so replay reproduces it exactly.
- The `doctrine_swap` event carries the target pack by reference and by expected hash, both required (section 4.5). keeld fills the hash from its registry when it queues the swap; the REST body stays `{name, version}` (section 8.1), since the registry is loaded once and cannot change under a running daemon. The pin guards replay.
- A swap to a reference the registry does not know, with no expected hash, to a registry pack whose hash differs, or with no pack active, is refused with an `operator_notice` decision and changes nothing. Every `doctrine_swap` decision in a log is a swap that happened.

---

## 7. Vector interface contract

```go
type Vector interface {
    Describe() Capabilities
    Telemetry() <-chan VectorState
    Execute(Command) error
    Health() HealthStatus
    io.Closer
}
```

The interface is public, in package `github.com/dsanchez31/keel/vector`, its types aliases of the domain types (section 4). The same package carries `Agent`, a toolkit implementing the mechanics below once: an adapter is a transport plus an `Agent`.

### 7.1 Semantics

- **`Describe`** MUST be pure, cheap, and stable for the adapter's lifetime. Capabilities changing mid mission is not supported.
- **`Telemetry`** returns a receive-only channel the adapter owns. It lives from construction to `Close`, and only `Close` closes it: a disconnect is reported by `Health`, and frames resume on the same channel once the adapter has reconnected. Delivery never blocks the adapter: telemetry is state, not a log, so a full channel drops its oldest frame for the newest.
- **Frames** carry the vector's own id, finite position, heading and speed, a battery in `[0, 100]`, a physical mode (`idle`, `transit`, `rtb`, section 4.2), and a `LastSeenMs` that never decreases. An adapter does not deliver a frame breaking this (C2). Every frame also carries the vehicle's `AckSeq` (section 4.2), which an adapter whose protocol carries no `Seq` fills itself.
- **`Execute`** MUST be non blocking. It returns once the command is accepted for transmission, not once it completes. Completion is observed through telemetry. It refuses with an error, never silently:
  - a command type the adapter does not declare, or that does not exist (`ErrUnsupported`, C8). Each adapter declares the subset of the four command types it executes;
  - a malformed command: addressed to another vector, `Seq` 0, a `goto` without a waypoint, a waypoint that is not finite (`ErrInvalidCommand`);
  - any command after `Close` (`ErrClosed`).
- **`Health`** MUST NOT perform I/O. It reports the adapter's cached view: `lost` before the first frame, after a disconnect until the next frame, and after `Close`; otherwise `degraded` once no frame has arrived for 5 s and `lost` at 10 s, the engine's link aging (`StaleTelemetryTicks`, 50 ticks by default, lost at twice that) measured in wall clock by the adapter, or on the clock it is given: keeld gives its pacer's scaled clock, so the thresholds are mission seconds at any pace (section 16.5). Both thresholds are adapter configuration.
- **`Close`** closes the telemetry channel and stops every goroutine the adapter started. Calling it again returns `nil`.

### 7.2 Commands

```go
type Command struct {
    Vector   VectorID    // the vector addressed; an adapter refuses another's
    Seq      uint64      // monotonic per vector, starts at 1
    Type     CommandType // "goto" | "hold" | "rtb" | "abort"
    Waypoint *Position   // required for "goto", the station for "rtb" when one is known
    Lane     LaneID      // the lane a "goto" or "hold" belongs to
}
```

Commands are idempotent by `Seq`. Re-delivering a `Seq` already applied MUST be a no-op, not an error. Adapters MUST tolerate gaps in `Seq` caused by loss.

**An adapter filters on what the vehicle acknowledged, not on what it sent.** A re-send (below) exists because the first copy may have been lost on the link, so an adapter dropping every `Seq` it already transmitted would defeat it. The adapter forwards a `Seq` above the latest it sent, whatever the gap, and a re-send of that latest `Seq` until a frame's `AckSeq` reaches it. A `Seq` at or below the latest frame's `AckSeq`, or below the latest sent (superseded), is a no-op returning `nil`.

**The engine re-sends what is not acknowledged.** A lost `goto` leaves a vehicle holding where the cursor never advances. A command standing for `ResendTicks` (engine configuration, 20 ticks by default) without being superseded, and above the `AckSeq` of the vector's latest frame, is sent again **with its original `Seq`**, which idempotency makes harmless when the first copy arrived. A command the vehicle acknowledged is not re-sent, however long it stands. A vector whose link is lost is skipped and re-sent to on the first tick its link is back. A command toward a lane the vector no longer holds is never re-sent.

### 7.3 Native protocol

`adapters/native` speaks KEEL's own vehicle protocol, JSON over WebSocket. It is the reference for a third party: a vehicle serving it is driven by the native adapter with no code on KEEL's side. The same package carries the vehicle side, `native.Server`, for a vehicle written in Go.

- **Connection.** The adapter dials the vehicle, one connection per vector, at `/v1/vectors/{id}`, negotiating the WebSocket subprotocol `keel.native.v1`. A peer that does not negotiate it is closed with status 1002, whichever end notices. An unknown id is answered 404 at the upgrade.
- **Messages.** One JSON text message per envelope `{"type": ..., "data": ...}`, at most 32 KiB. `data` is the domain type under its own JSON tags: `Hello` for `hello`, `VectorState` (section 4.2) for `telemetry`, `Command` (section 7.2) for `command`.
- **Hello.** The vehicle's first message, and only its first: `{"capabilities": Capabilities, "supports": [CommandType]}`. `supports` is required and never empty: the command types the vehicle executes, the declaration C8 holds it to. There is no implicit "everything". `tags` and `supports` are sets, order and duplicates carrying no meaning. The first hello fixes `Describe` and the declared types for the adapter's lifetime: a reconnect declaring anything else is closed with status 1008, and the adapter keeps redialling.
- **Direction.** After the hello, `telemetry` flows vehicle to adapter and `command` adapter to vehicle. Nothing else.
- **Strict decoding.** A field the Go type declares without `omitempty` is required, and `null` counts as absent. An unknown field, a key differing only in case, a missing required field, an unknown type, a message out of place and a binary message are violations: the receiver closes the connection with status 1003 for a binary message, 1002 for the rest, the reason naming the field or the type. A telemetry frame that decodes but breaks C2 (section 7.1) is closed with 1008, and so is a command addressed to another vector or of an undeclared type. The adapter then reports the reason through `Health`, lost until the next accepted frame.
- **Liveness.** A connection carrying no message for the idle timeout, the lost threshold of section 7.1 by default, is dropped and redialled: a half-open connection is otherwise never noticed. Redials wait between attempts, exponentially from 250 ms to 5 s with full jitter.
- **Commands.** `Execute` leaves the command in a single-slot mailbox, latest wins, drained by a writer: a newer `Seq` replaces one not yet written, and the slot survives a disconnect, so the command standing when the link dropped leaves on the next connection. The vehicle receives the engine's re-sends too; applying them idempotently by `Seq` is its part of section 7.2.
- **Telemetry on the vehicle side** is latest wins as well: a frame not yet written is superseded by the next, and the latest one published while no adapter was connected is the first sent after the next hello.
- **One connection per vehicle.** A newer connection replaces the current one, which is closed with status 1001: an adapter redialling after a half-open connection gets in at once. KEEL is a single orchestrator (design section 10); two adapters on one vehicle replace each other in turn.

### 7.4 MAVLink adapter

`adapters/mavlink` drives ArduPilot vehicles over MAVLink v2, dialect `ardupilotmega`: ArduCopter as `aerial`, ArduRover as `ground`. Other autopilots and vehicle types are out of scope.

- **Link.** A `Link` is one MAVLink node over one or more endpoints (a UDP port a SITL or MAVProxy pushes to, a telemetry radio). It routes frames from component 1 (`MAV_COMP_ID_AUTOPILOT1`) by system id to one `Adapter` per vehicle, and writes a vehicle's commands on the channel it was last heard on. The orchestrator's identity defaults to system 255, component 190, with its own heartbeat at 1 Hz. One adapter per system id per link.
- **Capabilities** come from the adapter's configuration: MAVLink declares none. The adapter executes all four command types.
- **Vehicle check.** Frames are published only from a heartbeat with autopilot `MAV_AUTOPILOT_ARDUPILOTMEGA`, a `MAV_TYPE` of a rotorcraft (aerial) or `GROUND_ROVER` (ground) matching the declared domain, a `SYS_STATUS` received, a known battery (`battery_remaining` not -1), and an absolute position estimate from the latest `EKF_STATUS_REPORT`, ArduCopter's own rule (`Copter::ekf_has_absolute_position`) for both vehicle types: disarmed, `EKF_POS_HORIZ_ABS` or `EKF_PRED_POS_HORIZ_ABS`; armed, `EKF_POS_HORIZ_ABS` and not `EKF_CONST_POS_MODE`. A relative position is not one: it is not a WGS-84 position. ArduPilot sends `GLOBAL_POSITION_INT` whether or not it knows where it is, at (0, 0) before its first estimate. Otherwise no frame is published and no command is executed, the reason in `Health`. The check holds at every frame: an estimate lost in flight withholds frames, and the silence reports the link degraded then lost (section 7.1). An autopilot reboot forgets the flags until the next report.
- **Altitude datum.** ArduPilot speaks AMSL. The adapter is configured with the geoid separation at the operating area (EGM96, metres): ellipsoid height = AMSL + separation, both ways.
- **Frames.** One per `GLOBAL_POSITION_INT`: position, speed the horizontal norm of `vx` and `vy`, heading `hdg` (unknown keeps the previous), battery from the latest `SYS_STATUS`, link `ok`, `LastSeenMs` 0 (the engine stamps the tick it accepts the frame on, section 4.2), `AckSeq` as below. The adapter asks for `GLOBAL_POSITION_INT`, `SYS_STATUS`, `EXTENDED_SYS_STATE` and `EKF_STATUS_REPORT` at 4 Hz with `MAV_CMD_SET_MESSAGE_INTERVAL`, and for `HOME_POSITION` with `MAV_CMD_REQUEST_MESSAGE`, on the first heartbeat (position frames already flowing at the autopilot's own rates included), again after an autopilot reboot and on each heartbeat while position frames stay silent.
- **Physical mode**, as the simulator (section 15.2): `rtb` while the autopilot is in RTL, SMART_RTL, AUTO_RTL or LAND (its own return or a failsafe) and while flying to an `rtb` station, `idle` once landed or arrived; `idle` disarmed or on the ground (`EXTENDED_SYS_STATE.landed_state`); `transit` in GUIDED while the applied command is a `goto` (station keeping on arrival included) and during a launch once armed; `idle` after `hold` or `abort`, and in any mode KEEL did not set. Between the acknowledgements of an applied command that sent `COMMAND_INT`s and the next heartbeat, whose arming and mode may predate them (1 Hz on ArduPilot), the command alone gives the mode: `transit` for a `goto`, `rtb` for an `rtb` not arrived, `idle` otherwise.
- **Commands** are `COMMAND_INT`, one at a time per vehicle, each awaiting its `COMMAND_ACK` (1.5 s by default):
  - `goto`: `MAV_CMD_DO_REPOSITION` with `MAV_DO_REPOSITION_FLAGS_CHANGE_MODE`, frame `MAV_FRAME_GLOBAL`, altitude AMSL, then for a copter `MAV_CMD_DO_CHANGE_SPEED` to the cruise speed (a rover reads it from the reposition). A copter on the ground is launched first: `MAV_CMD_DO_SET_MODE` GUIDED, `MAV_CMD_COMPONENT_ARM_DISARM`, `MAV_CMD_NAV_TAKEOFF` in `MAV_FRAME_GLOBAL_RELATIVE_ALT` to the goto's height above the ground (at least 3 m), then the reposition once 95 percent of that height is reached. A disarmed rover is armed first.
  - `hold` and `abort`: `MAV_CMD_DO_REPOSITION` to the current position. A vehicle on the ground already holds: applied without sending.
  - `rtb` with a station: `MAV_CMD_DO_REPOSITION` to the station at the higher of the current and the station altitude; within the station radius (5 m by default) a copter is sent `MAV_CMD_NAV_LAND`. Without a station: `MAV_CMD_NAV_RETURN_TO_LAUNCH`. A vehicle on the ground is not launched to return: applied without sending.
- **`AckSeq`.** A command is applied once every `COMMAND_INT` it sent is answered `MAV_RESULT_ACCEPTED`; `AckSeq` is its `Seq`. A refusal or a missing acknowledgement abandons the command, unacknowledged, with the command and the result in `Health`: the adapter never retries on its own, the engine's re-send (section 7.2) does. A re-send of the command under way is ignored. `time_boot_ms` going backwards is an autopilot reboot: `AckSeq` returns to 0.
- **Liveness.** No heartbeat from the vehicle for the lost threshold of section 7.1 reports the link down; the next heartbeat and frame resume on the same telemetry channel. MAVLink over UDP has no connection to redial.
- **SITL pace.** An adapter configured as an ArduPilot SITL holds the autopilot's `SIM_SPEEDUP` to the pace last asked for, 1 until then: `PARAM_SET` (REAL32), confirmed by the matching `PARAM_VALUE`, set again at most once per ack timeout until confirmed, on first contact and after a reboot (ArduPilot applies a runtime change of the parameter). A vehicle not configured as a SITL is never sent it, and asking for its pace is an error: a real autopilot has no such parameter. Every timing of the adapter (the ack timeout, the heartbeat silence, the stream retry) reads the clock it is given, so under keeld's scaled clock (section 16.5) they count in the SITL's own time.

---

## 8. Wire protocol

### 8.1 REST

| Method | Path | Purpose | Success |
|---|---|---|---|
| `POST` | `/api/v1/plans` | Compile intent. Body `{intent, backend?}`. Returns the outcome: the triage, Plan IR, expanded plan with its hash, projected assignments, every attempt with its diagnostics | `200` |
| `POST` | `/api/v1/plans/{hash}/approve` | The human gate. Hands the pending plan to the engine, applied at the next tick. Returns `{mission, plan}` and a `Location` to the mission | `202` |
| `DELETE` | `/api/v1/plans/{hash}` | Discard a pending plan | `204` |
| `GET` | `/api/v1/missions/{id}` | The mission view: mission and its area with the explored raster, active pack hash, lanes with cursors, vectors with capabilities and state, stations, coverage, log head | `200` |
| `POST` | `/api/v1/missions/{id}/doctrine` | Hot swap, applied at the next tick boundary. Body `{name, version}` | `202` |
| `GET` | `/api/v1/doctrine` | Registered packs: reference, hash, normalised document | `200` |
| `GET` | `/api/v1/world` | The world file keeld loaded, the names an intent may use (section 5.1): `{name, areas, stations}`, an area as `{name, polygon, cell_m, scan_alt_m, area_km2}` (its outline, cell size, scan altitude and the surface its raster covers, never the raster), a station as in the mission view | `200` |
| `GET` | `/api/v1/fleet` | Every bound vector (section 16.2) in id order with its adapter's health now: `[{id, health, detail?}]`, `health` the `Health` kind (`ok`, `degraded`, `lost`), `detail` why. A vector that has not joined the engine yet is listed with the reason (not dialled yet, an autopilot's frames withheld, section 7.4), where the mission view does not know it exists. The adapter's view, not engine state: never on the stream, never recorded | `200` |
| `POST` | `/api/v1/faults` | Inject a fault, applied at the next tick. Body `{vector, kind, magnitude?, duration_ms?}`, the fields of a scenario fault (section 15.1) | `202` |
| `GET` | `/api/v1/replays/{id}` | Stream a mission's recorded log, `application/x-ndjson`, one record per line as on disk (section 10.1) | `200` |
| `GET` | `/api/v1/replays/{id}/frames?from=&to=` | A window of the finished mission replayed and verified (section 10.3), as stream frames: `{from_ms, to_ms, frames, verified_ms, divergence?, end?}` | `200` |
| `GET` | `/api/v1/clock` | The pace: `{speed, max_speed, fixed?}`, how many times faster than real time the fleet moves, the fastest pace accepted, and why the pace stays real time when it must (section 16.5) | `200` |
| `PUT` | `/api/v1/clock` | Set the pace of keeld and of every simulator of its fleet. Body `{speed}`, a whole number in [1, 20]. Returns the pace in force | `200` |

- **No `area` in the compile body.** The model names the AO from the names the prompt lists and gate 2 resolves it (section 5.2); the human gate shows the resolved AO before approval.
- **A failed compilation is a valid outcome, not a server error.** `POST /api/v1/plans` returns `200` with every attempt's diagnostics when compilation fails after the repair budget, and `200` with `triage.mission` false, its `reason` and no attempt when the intent asks for no mission (section 5.4). A backend failure (section 5.4) is `502`, the attempts made before it carried in the problem's `outcome` member.
- **Accepted, not applied.** Approval, hot swap and fault injection reach the engine as events of its next tick; their result (a mission started or a refusal, section 4.5) is a decision on the stream.
- **The pace is applied, not accepted.** `PUT /api/v1/clock` answers once keeld and every simulator run at it, from the next tick on; it is idempotent, the pace in force changing nothing. A speed outside [1, 20] is `422`; any pace but 1 on a fleet holding a vector no simulator paces, or on a daemon not paced in real time, `409` naming why; a simulator refusing it or not confirming it within 5 s `502`, the pace left as it was (section 16.5).
- **A fault is held to the rule of a scenario fault** (section 15.1): a vector, a known kind, a finite `magnitude` that is not negative and is positive for `battery_drain` and `gps_drift`, a `duration_ms` of zero or whole ticks. Else `422`, naming what is wrong. One rule for every source of a fault: the scenario loader, the world, this endpoint and the simulator's.
- **A replay window is the replay of section 10.3, projected as the stream is.** keeld replays the finished log, verifying, through the tick at or past `to`, and projects every tick from `from` with the stream's own projection (section 8.2): the first frame, `seq` 1, is the mission view after the last tick before `from`, the next ones every tick from `from` to `to`, numbered without a gap. Telemetry and `tick` frames are thinned to one tick in ten (1 Hz of mission time), the window's last tick excepted; every other frame is kept. `verified_ms` is the mission time of the last tick rebuilt to the recorded digests, `divergence` the first record that was not (`{seq, tick_ms, recorded?, replayed?}`, the window ending at its tick), `end` how the recording ends once the window reached it (`{tick_ms, recorded, replayed}`, both heads), a window whose `to` is the last tick's time reaching it. A window past the end holds the final view. `from` and `to` are whole milliseconds of mission time, absent or unreadable `400`; `from` negative, `to` before it or more than 30 minutes after it `422`. The running mission is `409`, as a log whose chain is broken or that is not a mission log.
- **Faults are simulated.** A fault reaches a simulated vector through the simulator serving it, `keelsim --serve`'s `POST /v1/faults` (section 15.4), and `fault_injected` is recorded only once the simulator answered `202`: the log never claims a fault the vehicle did not undergo. A vector bound to no simulator (an autopilot, section 16.2) or unbound is `422`, as is a fault the simulator refuses, with its detail; a simulator unreachable or failing is `502`. It is how the reference demo loses DRONE-02.
- **Bodies are strict.** `application/json` (else `415`), at most 1 MiB (else `413`), one JSON value, keys matched exactly, no unknown field, every required field present with `null` counting as absent (else `400`, naming the field). A well formed request whose content is refused (a blank intent, an unknown vector) is `422`.
- **Errors are RFC 9457 problem details**, `application/problem+json`: `title` the status text, `status`, `detail`. `404` for an unknown plan, mission or log, `409` for a request the current state contradicts (an approval while a mission runs, a hot swap for a mission that exists and is not running). A `500` does not disclose its cause.
- **Cross-origin writes are refused.** The API carries no authentication (section 1), so a state-changing request from a browser page of another origin is `403`, judged by `Sec-Fetch-Site`, then `Origin` against `Host`. The daemon is configured with the origins it trusts besides its own (a frontend dev server, a reverse proxy). Reads stay open and carry no CORS header.
- Every JSON answer goes through the canonical encoder (section 9).

`keelctl plan compile` draws the same line in its exit status: `0` a plan was produced, `2` none was, every attempt refused or the intent declined (the outcome, with its diagnostics or the triage's reason, is still printed), `1` any other error, a backend failure included. `--think` is `planner.think`.

### 8.2 WebSocket

Single endpoint `/api/v1/stream`, server to client. One frame per WebSocket text message, canonical JSON (section 9):

```json
{"data":{},"seq":481,"t":12300,"type":"telemetry"}
```

`t` is the mission time in milliseconds of the tick the frame belongs to, `seq` numbers the frames of one connection from 1.

| Type | Data | Sent |
|---|---|---|
| `mission` | The mission view of `GET /api/v1/missions/{id}`, with the pace `speed` on the live stream | First frame of every connection; again when a tick changes the mission, its plan, its active pack, or which lanes exist and who holds them |
| `event` | One engine event (section 4.5), telemetry aside | For every event of the tick, in the order the engine applied them |
| `decision` | One decision (section 10.2) | For every decision of the tick, in order |
| `doctrine` | The reference, hash and document of the pack that became active | When the active pack changes |
| `telemetry` | Every vector's state as the engine holds it, derived modes included: an array in vector id order | Every tick with a vector; faster than real time, thinned (below) |
| `coverage` | `{cells}`, the cells the tick explored, ascending | When the tick explored a cell, unless a `mission` frame of the tick carries a new raster |
| `tick` | `{tick, head, state, explored, total, cursors, speed?}`, the log head after the tick, every lane's cursor, and on the live stream the pace | Last frame of every tick; faster than real time, thinned (below) |

- **The frames' types are generated from the Go types** (`tools/sdkgen`) into `@keel/sdk`, which types a frame's `data` by its `type` as this table does. A change of a body or a frame is a change of the Go type, then `make generate`; `make check-sdk` fails on a stale output.
- **A client holds the mission view by folding frames into the last `mission` frame**: `telemetry` replaces every vector's state (a vector joined since arrives first as a `vector_joined` event carrying its capabilities), `coverage` marks its cells explored, `tick` sets the tick, the head, the mission state, the coverage count, every cursor and the pace (removed by a tick without one), `doctrine` the active pack's hash. A lane's `engaged` flag travels in `mission` frames only. `applyFrame` in `@keel/sdk` is this fold.
- **A connection starts from a snapshot.** Its first frame is the `mission` view after tick N, its next frames tick N+1's: the snapshot and each tick's frames are published together, so a client joins between two ticks, never inside one. A client connecting before the first tick receives the first tick's view as its first frame.
- **The live stream carries the pace and is thinned by it.** `speed` in `mission` and `tick` frames is keeld's pace (section 16.5); a replay window's frames carry none, the log not recording the pace. At a pace `s` above 1, `telemetry` and `tick` frames are sent for the ticks whose number is a multiple of `s` and for every tick changing the mission or its state: about ten of each per second of wall clock whatever the pace, the rate a browser draws, where every tick at ×20 would be two hundred. They only replace what the next ones replace again; `event`, `decision`, `doctrine`, `mission` and `coverage` frames are never thinned, a dropped one being a state that never was.
- **Frames are dropped, never queued without bound.** Each connection has a bounded buffer. A client too slow to drain it loses frames, and each lost frame still consumes its `seq`.
- **A client that observes a gap in `seq` MUST resynchronise** rather than interpolate: by reconnecting, which starts from a snapshot, or from `GET /api/v1/missions/{id}`.
- A data message from the client closes the connection with status `1008`. The server pings every 30 s and drops a connection that cannot take a frame within 10 s. Shutting down closes every connection with `1001`.
- The handshake refuses an `Origin` other than the server's own or a trusted one, the same list as the REST side's, through the WebSocket library's own origin check: a handshake is a `GET`, which `Sec-Fetch-Site` does not guard. A request carrying no `Origin`, which no browser sends, passes, as on the REST side.

---

## 9. Canonical encoding

Everything hashed or compared uses one encoder. It is the foundation of the determinism claim.

- JSON, UTF-8, no insignificant whitespace.
- Object keys sorted by Unicode code point.
- Integers rendered without exponent. Floats rendered with `strconv.FormatFloat(f, 'g', 17, 64)`, which round trips exactly.
- No `NaN`, no infinities. Their presence is a bug and MUST panic in tests.
- Absent optional fields are omitted, never emitted as `null`.
- Slices preserve order, and every slice that represents a set is sorted at construction.

---

## 10. Event log and hash chain

### 10.1 Records

```go
type Record struct {
    Seq      uint64
    TickMs   int64
    Kind     RecordKind
    Payload  []byte  // canonical encoding of the event or decision
    PrevHash [32]byte
    Hash     [32]byte
}
```

`Hash = SHA256(canonical(Seq, TickMs, Kind, Payload, PrevHash))`. The first record uses a zero `PrevHash`.

**A mission log** is one chain, in one directory of segments, written the same way by `keelsim` and `keeld`:

1. a `header` record at 0 ms: `{name, seed, tick_interval_ms, config, plan, mission}`, the engine's seed and configuration being as much inputs to its decisions as the events;
2. then, for every tick, stamped with its mission time: one `event` record per event `Step` was given, as given and in the order given; one `decision` record per decision, in order; one `command` record per command, in order; a `tick` record `{tick, explored, total, state}`.

The events are recorded before the engine sorts them, so a replay feeds `Step` exactly what the live loop did. A replay starts from `engine.NewState(seed, config, packs)`, the packs resolved from the plan's doctrine reference (section 10.3).

### 10.2 Decision records

Every decision carries its full rationale, which is what makes the system explainable rather than merely observable:

```go
type Decision struct {
    TickMs     int64
    Kind       DecisionKind     // see below
    Subject    string           // the lane, vector, plan or pack concerned
    RuleFired  string           // the rule that fired or that caused this decision
    Shadowed   []ShadowedRule   // rule id plus the id that shadowed it
    Candidates []Candidate      // vector id, cost score, and rejection reason if any
    Winner     VectorID
    Rationale  string
    Swap       *DoctrineSwap    // doctrine_swap only: refs, hashes, retained and discarded windows
}
```

`Candidates` MUST list every vector considered, including rejected ones with the reason. A decision that reports only its winner is incomplete.

| Kind | Recorded when | Carries |
|---|---|---|
| `assignment` | a lane is allocated in the launch round | every vector of the fleet as a candidate, the winner, the rationale |
| `reassignment` | a lane is allocated in any later round | the same, plus `RuleFired` when a rule's redecomposition started the round |
| `redecompose` | a redecomposition runs or is deferred | the pool, the retired and the new lanes in the rationale, the causing rule or the engine's reason |
| `doctrine_rule` | a rule fires for an agent | the agent as subject, the rule, every shadowed rule with its shadowing id, what each action did |
| `doctrine_swap` | a hot swap happens | the full swap record |
| `mission_state` | a mission starts, completes, fails, or an approval is refused | the plan or mission |
| `operator_notice` | `notify_operator`, a fault injected, lanes released, a refused join or swap | the message |

### 10.3 Replay

A replay reads a mission log (section 10.1) and feeds `engine.Step`, from `engine.NewState(seed, config, packs)`, the batches it holds. It rebuilds the chain through the same writer the live loops record with.

- **The chain is checked as it is read** (I6): every record carries the next `Seq`, links to the previous digest and carries the digest its content implies. A break is an error: nothing read after it can be trusted.
- **The layout is checked as it is read.** The first record is the header at 0 ms, recorded under the engine's tick interval. Every payload decodes strictly, an unknown or missing field refused, as at every boundary. A record kind the layout does not write, or a log ending inside a tick (records after the last `tick` record, as a crash mid-write leaves it), is an error.
- The packs are the registry the plan and every hot swap resolve against, pinned by hash (section 4.5). A pack edited since the recording is not an error: the replay refuses the approval or the swap on its pin, and diverges there.
- **A verifying replay compares every rebuilt record with the recorded one**, by digest, tick by tick, and stops at the first that differs: its `Seq`, its mission time, both records. No divergence means one head for both chains (I7).
- The replay streams: it holds one tick of the recording and of the rebuilt chain at a time.
- **A replay may stop part way**, after the first tick at or past a given mission time, and it hands every tick it ran over as it goes: the state before and after it, its batch, what it decided and commanded, and the rebuilt head before and after its records. That is what keeld projects a replay window from (section 8.1).

`keelctl replay <log>` reads a log file, a segment directory (keeld's `data_dir/missions/MSN-NNN`) or `-` for standard input, such as what `GET /api/v1/replays/{id}` serves. `--doctrine-dir` names the packs (default `doctrine-packs`). It prints every decision the replay makes (mission time, kind, subject, rationale), then the header, the counts, the final mission state and coverage, and the rebuilt head. `--verify-hash` also compares, prints both heads and, on a divergence, both records, a payload cut at 1 KiB. Exit status: 0 when the replay ran and, with `--verify-hash`, reproduced the recording; 2 when `--verify-hash` found a divergence or a broken chain; 1 for any other error, a broken chain without `--verify-hash` included.

### 10.4 Doctrine diff

`keelctl doctrine diff <log> <name@version> <name@version>` replays a mission log twice in lockstep, reading it once (section 10.3), and reports the ticks on which the two replays differ.

- **Only the starting pack is substituted.** Every `plan_approved` of the log starts, in the first replay, under the first pack and, in the second, under the second, each pinned to its registry hash (section 4.5). The plan keeps its recorded hash: the diff compares doctrine, not plans. It is not validated again. A log that approves no plan is an error.
- **Hot swaps are replayed as recorded.** A recorded swap to a pack happens in both replays at its tick, each carrying its own windows across (section 6.6).
- **A tick differs** when a decision one replay made has no equal among the other's, or when the commands differ. Decisions are paired by canonical encoding, modulo the starting pack's identity: the approval names its pack, a swap leaving it names it as `From`, and there the second pack is read as the first. A swap's target is left as it is, since it is the same pack in both replays.
- **The replay is open loop.** The recorded telemetry answers the commands the recording issued. After the first divergence the diff shows what each pack decides on the same inputs, not what the mission would have done under it (design section 10).

Every differing tick prints its mission time, `-` for each decision only the first pack made and `+` for each only the second made (kind, subject, rationale), and a line when the commands differ. A summary follows: the header, each pack with its decision and command counts, final mission state and coverage, the number of ticks compared and diverging, the first divergence. `--doctrine-dir` names the packs both references resolve in (default `doctrine-packs`); the log is read as by `keelctl replay`. Exit status: 0 when the packs decide the same on every tick, 2 when they diverge, 1 for any other error.

---

## 11. System invariants

These hold at every tick boundary and are asserted by the deterministic simulation test harness.

| ID | Invariant |
|---|---|
| **I1** | No cell is assigned to two vectors simultaneously |
| **I2** | Coverage is monotonic: an explored cell never becomes unexplored |
| **I3** | Every lane offered for allocation (unassigned, with a waypoint left and a cell uncovered) is assigned within `redecompose_deadline` ticks of becoming unassigned while the allocator, asked the engine's own question over the pending lanes, gives it a vector. Work the allocator refuses to every vector is bounded by the stall instead (section 5.7) |
| **I4** | No vector stays at work outside its constraint envelope: at every tick boundary, every vector not in mode `down` that violates a constraint of the active pack (battery reserve, geofence) stands on an `rtb` command. A fault can break the envelope at once, a battery collapse or a GPS drift; what the engine guarantees is the reaction, within the tick. It holds under a pack with a rule sending home, without a window, a vector violating each of its constraints: the shipped 2.1.0 and 2.2.0, not 2.0.0, which declares a geofence and no rule reacting to it. The geofence bounds lane work: the shipped packs check it in mode `scanning` only, since transit to and from a GCS outside the AO is not lane work |
| **I5** | Mission time advances by exactly one tick interval per tick, never varying |
| **I6** | The hash chain is unbroken: `record[n].PrevHash == record[n-1].Hash` |
| **I7** | Replaying a log produces an identical chain of decisions and an identical head hash |
| **I8** | The mission never deadlocks: it reaches `complete` or `failed` in bounded time. A walked lane's residue is redecomposed (section 6.5.1), so completion does not depend on the geometry being gapless, and a mission no eligible vector works fails on the stall deadline (section 5.7) |
| **I9** | A doctrine hot swap never changes `Mission.State` and never clears coverage |

**The harness** (`internal/engine/dst_test.go`, `TestDST`) runs the reference mission (section 14) through the closed loop of section 15.3 once per seed. The seed seeds the engine and the world, and draws a schedule from a stream of its own: one to four world faults (section 15.1) at ticks up to 8000, at most two kills, links and telemetry lost for 3 to 30 s or for good, batteries drained by 20 to 70 percent, GPS drifting at 1 to 10 m/s for 5 to 30 s; a pinned hot swap to a pack satisfying I4's condition on half the seeds; and, on the wire between the world and the engine, every telemetry frame dropped, duplicated or held back up to 3 s at 0.5 percent each and one batch in 50 reordered. Operator events are never touched. The invariants are asserted on the engine's state at every tick boundary, I1, I2, I5 and I9 as written, I4 on the constraints evaluated as the engine evaluates them, and I3 against the allocator asked the engine's own question over the pending lanes: a lane pending past the deadline that it gives a vector at two boundaries in a row is a violation, one boundary being a tick newer than the engine's round. I8 holds when coverage or the stall ends the mission, not the tick ceiling. I6 and I7 hold when the recorded log replays, verifying, to the same chain (section 10.3). A failure prints the violation, the seed's schedule and the command reproducing it, `go test ./internal/engine -run 'TestDST$' -count=1 -dst.seed=<seed>`. `-dst.seeds` (8 by default, 2 under `-short`) and `-dst.from` (1) choose the sweep.

---

## 12. Determinism requirements

Normative. Any violation is a defect.

1. The decision path MUST NOT call `time.Now()` or read any wall clock.
2. The decision path MUST NOT iterate a Go map. Keys are collected and sorted first.
3. All randomness comes from an explicitly seeded `math/rand/v2` source carried in state. The global source is forbidden.
4. `Step` MUST NOT start goroutines, and MUST NOT read from channels.
5. Floating point accumulation MUST be order independent, achieved by sorting before reduction.
6. Given the same seed and the same input event sequence, `Step` MUST produce byte identical output on any platform Go supports.
7. In the decision path, a floating point product that feeds an addition or a subtraction MUST be rounded explicitly: `float64(a*b) + c`, never `a*b + c`. The Go specification lets a compiler fuse the latter into one FMA instruction that rounds once instead of twice, several backends do (arm64 among them) while amd64 does not, and the last bit then differs across architectures. An explicit conversion forbids the fusion. Functions of package `math` are compiled under the same rules and are outside this rule's reach, which is why requirement 6 is verified only where it is tested (design section 10.2).

---

## 13. Adapter conformance

`keelctl vector validate` runs this suite. A manufacturer adapter is conformant when all cases pass.

| ID | Case | Pass condition |
|---|---|---|
| **C1** | Capability declaration | `Describe()` returns sorted tags, non-zero speed and range, valid domain |
| **C2** | Telemetry schema | 100 consecutive frames validate, monotonic `LastSeenMs`, positions finite, modes physical only (`idle`, `transit`, `rtb`, section 4.2) |
| **C3** | Command acknowledgement | A `goto` is reflected in telemetry (mode `transit`) within 2 s |
| **C4** | Idempotency | Re-sending an applied `Seq` changes nothing and returns no error |
| **C5** | Sequence gaps | Skipping a `Seq` is tolerated, the later command still applies |
| **C6** | Timeout | With telemetry withheld 10 s, `Health()` reports degraded (at 5 s) then lost (at 10 s) |
| **C7** | Reconnect | After a forced disconnect the adapter re-establishes and resumes telemetry within 30 s |
| **C8** | Capability honesty | A command of a type the adapter does not declare, or of no known type, is rejected with `ErrUnsupported`, not silently ignored |
| **C9** | Clean shutdown | On close, the telemetry channel closes and no goroutine leaks |

C8 is the one that matters most in practice: an adapter that accepts commands it cannot perform is worse than one that refuses them, because the orchestrator will keep assigning work that never happens.

**How the suite runs** (`internal/conformance`):

- **The suite owns the transport.** It opens the adapter itself through a proxy: TCP for a native vehicle (the adapter dials the vehicle's URL unchanged, Host and TLS server name included, routed through the proxy), UDP for a MAVLink vehicle (the proxy listens on the address the vehicle pushes to, in the ground station's place). Withholding pauses the vehicle's TCP stream, never drops bytes of it, and drops its datagrams; a cut closes and refuses TCP connections and drops datagrams both ways until restored. C6 and C7 therefore run against any vehicle without its cooperation.
- **Order**: C1, C2, C3, C4, C5, C8, C6, C7, C9. The cases that need a healthy link come first, the ones that break it after, `Close` last.
- **Motion needs consent.** C3, C4 and C5 command the vehicle, and a landed copter takes off (section 7.4): they run only when motion is allowed (`keelctl vector validate --allow-motion`), and are skipped otherwise. A skipped case is not a pass: conformance is all nine passing. `validate` exits `0` when conformant, `2` when a case failed or was skipped, `1` when the suite could not run.
- **`Seq` starts above the vehicle's `AckSeq`.** A vehicle that has run under another orchestrator filters any lower `Seq` (section 7.2).
- **C3** sends a `goto` 50 m north of the vehicle, 10 m higher for an aerial one. **C4** waits up to 30 s for its acknowledgement, re-sends it, and requires `AckSeq` and mode unchanged over the next second. **C5** skips one `Seq` and sends a `hold`, which leaves the vehicle stopped. **C8** sends a command of no known type, and every type the adapter does not declare when it exposes its declared set.
- **C6** holds `Health` to the adapter's thresholds, 5 s and 10 s by default and configured on the adapter and the suite alike (`--degraded-after`, `--lost-after`), each observed within 1 s, degraded seen before lost. The link then returns and telemetry must resume before C7.
- **C9** requires the telemetry channel closed within 5 s of `Close`, a second `Close` returning `nil`, and the process's goroutines back to their count before the adapter opened.

---

## 14. Reference scenario

The scenario the demo and the acceptance test both run.

- **AO**: `fog_of_war_east`, roughly 14.3 km².
- **Fleet**: 4 aerial vectors with `[aerial, camera, gps]`, 1 ground vector with `[ground, camera, gps]`, 1 relay with `[aerial, radio_mesh]`.
- **Intent**: "Grid-search the unexplored area to lift the fog of war".
- **Expected plan**: `parallel_lanes`, 4 lanes, orientation `long_axis`, policy `nearest_capable`, about 7 passes and 25 to 35 waypoints per lane, the corner cells of the slanted edges included.
- **Injected fault**: link loss on `DRONE-02` at 60 percent coverage.
- **Expected reaction**: after 5 s of mission time, `reassign-on-link-loss` fires, `DRONE-02`'s lanes are released, uncovered cells are redistributed across the 3 survivors, coverage reaches 100 percent.
- **Acceptance**: coverage completes, invariants I1 to I9 hold throughout, and replay reproduces the head hash exactly.
- **Simulated run**: `examples/sims/reference.yaml` (section 15), `keelsim --scenario examples/sims/reference.yaml`, under `recon-standard@2.2.0`. It completes in about 38 minutes of mission time, with the link loss at 60 percent coverage and its reaction as the only doctrine firing.

---

## 15. Simulator

`keelsim` runs a mission end to end without vehicles: the engine below the fence, a simulated world it commands, a plan validated through the four gates, and scheduled faults. It records the hash-chained log and prints its head.

### 15.1 Scenario

A `keel.sim/v1` YAML document. Its schema is closed like a world's or a pack's: an unknown field, a duplicate key or a second document is rejected, and every bound is checked at load.

```yaml
apiVersion: keel.sim/v1
name: reference
seed: 42                        # required, seeds the engine and the world
mission: MSN-042                # required, the id the plan is approved under
world: ../worlds/reference.yaml # required, relative to this file unless absolute
plan: ../plans/reference.json   # required, a Plan IR, validated through gates 1 to 4
kinematics:
  climb_rate_mps: 5             # required, > 0, aerial vertical speed cap
comms:                          # optional, a perfect link when absent
  range_m: 15000                # from the nearest station, 0 = unlimited
  loss_pct: 2                   # per frame and per command, [0, 100]
  jitter_ms: 200                # extra delay drawn in [0, jitter_ms], whole ticks
blackout_zones:                 # optional, no link inside
  - {name: ridge-shadow, center: {lat: 45.056, lon: 5.06}, radius_m: 600}
terrain:                        # optional
  cell_m: 25                    # default 25
  blocked:                      # polygons no ground vehicle enters
    - {name: canal, polygon: [{lat: .., lon: ..}, ...]}
sensors:
  gps_noise_m: 1.5              # bound of the uniform error per horizontal axis
battery:
  hover_drain_pct_per_min: 0.5  # airborne and not moving
faults:
  - at_coverage_pct: 60         # exactly one of at_coverage_pct, at_ms (whole ticks)
    vector: DRONE-02
    kind: link_loss             # kill | link_loss | battery_drain | gps_drift | stale_telemetry
    magnitude: 0                # battery_drain: percent; gps_drift: m/s; required > 0 for both
    duration_ms: 0              # whole ticks, 0 = for the rest of the run
    description: ...
```

The world file and the Plan IR are referenced, not embedded: the planner never sees a blackout zone, and a scenario never redefines the AO. A fault naming a vector outside the world's fleet is refused at load.

### 15.2 World semantics

- **Time.** The world advances one tick of 100 ms per step, driven by its caller, never by a clock. All randomness comes from one PCG source seeded by the scenario; vehicles are visited in id order and every draw happens in that order, whether or not its outcome matters (a lost frame still draws its noise), so a seed fixes the run.
- **Fleet.** The world's fleet, each vehicle joining with its capabilities and its world-file state (section 4.5). A vehicle whose world-file mode is `down` does not join.
- **Kinematics.** An aerial vehicle flies straight at `CruiseSpeed` while climbing or descending at most at `climb_rate_mps`, both at once, and stops exactly on its target. A ground vehicle drives at `CruiseSpeed` on the ground, along the shortest 8-connected path over open terrain cells shortened by line of sight; a destination on blocked ground, or cut off, leaves it idle where it is.
- **Commands.** A command is delivered at the start of a later step, after its jitter, when the vehicle had a link at send time and the loss draw spared it. A `Seq` not above the last applied one is ignored (section 7.2). `goto` flies to the waypoint, `hold` and `abort` stop, `rtb` flies to its waypoint, or to the vehicle's initial position without one, and ends in mode `idle` on arrival. Telemetry reports physical modes only: `idle`, `transit` (toward a commanded point, station keeping on arrival included), `rtb`.
- **Link.** Up when the vehicle is within `range_m` of a station, outside every blackout zone, and not cut by a fault. A frame or a command is judged when it is sent.
- **Battery.** Covering `d` metres costs `d × 100 / MaxRangeM` percent, the allocator's model (section 5.7). An airborne vehicle covering no distance pays `hover_drain_pct_per_min`. Reported as the floor of the charge. Empty, the vehicle is down.
- **Reported position.** The true position plus uniform noise per horizontal axis and the offset of an active GPS drift.
- **Faults.** `kill`: no frame leaves the vehicle and no command reaches it. `link_loss`: the link is cut both ways for `duration_ms`. `battery_drain`: `magnitude` percent gone at once. `gps_drift`: the reported position walks away at `magnitude` m/s in a direction drawn at injection, and recovers when the fault ends. `stale_telemetry`: the vehicle repeats the last frame it sent before the fault, timestamp included.

### 15.3 The loop

One tick: the pacer waits; `engine.Step` runs over the batch that arrived during the previous tick; the batch, the decisions, the commands and a tick summary are appended to the log; the commands are sent; every scheduled fault now due is injected into the world and, as a `fault_injected` event, into the next batch; the world steps and its telemetry is the next batch. The first batch is the fleet joining and the plan approved. A coverage trigger is judged on the engine's state after its tick.

**Headless-fast and real time are one code path.** A pacer decides only when a tick runs: `Fast` never waits, `RealTime` waits for tick `n`'s wall-clock deadline, `(n − 1) × 100 ms / speed` after the first tick, without skipping ticks to catch up. Both produce the same log to the bit.

`keelsim` exits `0` when the mission completed, `2` when it failed or did not finish (tick ceiling, interrupt), `1` on any other error. `--out` records into an empty directory, the log is kept in memory otherwise.

### 15.4 Serve mode

`keelsim --serve` runs the scenario's world with no engine and serves each fleet vector over the native protocol (section 7.3), for an orchestrator to dial at `ws://LISTEN/v1/vectors/{id}`, and takes faults at `http://LISTEN/v1/faults`. `--listen` defaults to `127.0.0.1:8090`: neither carries authentication.

- **Paced by its orchestrator.** The pacer is `RealTime`, starting at speed 1; `--speed` other than 1, `--out` and `--max-ticks` are refused. The orchestrator sets the pace with `PUT /v1/clock`, body `{speed}`: a whole number in [1, 20], answered `200 {speed}` and in force from the next tick, `422` outside, `409` for a loop not paced in real time; `GET` answers the pace, any other method `405` with `Allow: GET, PUT`; bodies, errors and cross-origin writes as the fault endpoint's. A world paced apart from its orchestrator would fly every vehicle faster or slower than its declared cruise speed in the orchestrator's mission time: keeld paces both together (section 16.5). It runs until interrupted, exiting `0`.
- **One tick**: the commands the vehicles received since the last tick are sent through the simulated radio, sorted by vector then `Seq`; every time-triggered fault now due is injected, then every fault posted since the last tick, in arrival order; the world steps and each telemetry frame that reached the ground is sent to its vehicle's connection. The radio stays in the loop: what the world loses is lost, whatever the WebSocket delivered.
- **Each vehicle declares the four command types** in its hello, the ones the world executes (section 15.2).
- **Frames carry `LastSeenMs` 0.** keelsim's mission clock and the orchestrator's start apart, so the engine stamps the tick it accepts a frame on (section 4.2). A `stale_telemetry` fault therefore re-sends a frozen frame the engine takes as current: a stuck sensor, not a stale timestamp.
- **Faults.** `at_ms` faults are injected on the world's clock. `at_coverage_pct` faults are skipped with a notice pointing at the fault endpoint: coverage is the engine's to observe, and the engine runs in the orchestrator. The world does not announce a fault: the orchestrator sees its consequences (silence after a `kill` or a `link_loss`), as it would a real vehicle's.
- **Fault endpoint.** `POST /v1/faults` on the `--listen` address, beside the vehicles. The body is a fault, `{vector, kind, magnitude?, duration_ms?}` as in `POST /api/v1/faults` (section 8.1), held to the rule of section 15.1 and to the fleet. Answered `202` without a body once queued, injected at the next tick. Bodies and errors follow section 8.1: strict JSON (`415`, `413`, `400`), `422` for a fault the rule refuses or a vector outside the fleet, problem details, cross-origin writes `403`. At most 64 faults wait for a tick, beyond which `503` with `Retry-After: 1`. Any other method on the path is `405` with `Allow: POST`.

---

## 16. Daemon

`keeld` is the orchestrator: it binds a fleet, compiles intent against it, runs the engine on the plan an operator approved, records every mission and serves sections 8.1 and 8.2 until interrupted.

### 16.1 Configuration

A `keel.daemon/v1` YAML document, `--config`. Its schema is closed like a world's: an unknown field, a duplicate key or a second document is rejected, every value is checked at load, and paths are relative to the file.

```yaml
apiVersion: keel.daemon/v1
name: reference                 # required, recorded in every mission header
world: ../worlds/reference.yaml # required: areas, stations, the fleet's declared capabilities
doctrine_dir: ../../doctrine-packs  # required, every *.yaml a pack
data_dir: ../../run/keeld       # required, missions recorded under missions/MSN-NNN/
listen: 127.0.0.1:8080          # default loopback: the API carries no authentication
trusted_origins: [http://localhost:5173]  # browser origins allowed to write (section 8.1)
seed: 42                        # optional, every mission's engine seed; absent, drawn per mission
planner:
  backend: ollama               # default ollama | claude, a compile request may name the other
  model: qwen3:14b              # optional, the configured backend's model
  ollama_url: http://gpu:11434  # optional, else OLLAMA_HOST, else the default
  timeout_ms: 300000            # bound on one compilation, every attempt included
  think: false                  # default false: let the model reason first, either backend (section 5.4)
mavlink:                        # required when a vector is bound over MAVLink
  listen: 127.0.0.1:14550       # the UDP port every autopilot pushes to
  geoid_separation_m: 0         # [-200, 200], section 7.4
vectors:                        # at least one, one binding per id of the world's fleet
  - id: DRONE-01
    mavlink: {sysid: 1, sitl: true}  # sysid [1, 255], bound once; sitl: SIM_SPEEDUP follows keeld's pace
  - id: DRONE-02
    native:
      url: "ws://127.0.0.1:8090/v1/vectors/DRONE-02"
      faults: "http://127.0.0.1:8090/v1/faults"  # optional, the simulator's fault endpoint
      clock: "http://127.0.0.1:8090/v1/clock"    # optional, the simulator's clock endpoint
```

`--listen` overrides `listen`. A bound id outside the world's fleet is refused at startup; a fleet vector left unbound is not flown.

### 16.2 Bindings

- **Native** (section 7.3). The capabilities are the ones the hello declares, and a hello naming another id than the binding's is refused: a misrouted vehicle would fly under another's name. keeld dials in the background with the adapter's backoff until the vehicle answers, so a simulator may start after it. `faults` is the fault endpoint of the simulator serving the vehicle, without which it takes no fault; `clock` its clock endpoint, without which the vehicle keeps real time, and keeld with it (section 16.5).
- **MAVLink** (section 7.4). One link on `mavlink.listen` serves every autopilot, routed by system id. MAVLink carries no capabilities: the world's fleet entry for the id provides them. An autopilot takes no fault. `sitl: true` declares an ArduPilot SITL, whose `SIM_SPEEDUP` follows keeld's pace (section 7.4); without it the vehicle keeps real time, and keeld with it.

### 16.3 Engines and missions

- **Between missions, an idle engine.** It is fed the fleet (joins, frames, link verdicts) so the operator sees it before any plan, and is not recorded: its view carries the zero head of an empty chain.
- **A vector joins an engine** with its latest frame, the link set to its adapter's verdict (`Health` kind), then sends each new frame and each change of verdict as `link_changed`. Between two ticks, frames are state: the latest wins. Until its first frame a bound vector is in no engine, and `GET /api/v1/fleet` (section 8.1) is where the operator sees it and why: an ArduPilot autopilot sends none until its EKF has an absolute position, about 40 s after it boots for the SITL of the reference topology.
- **Compilation uses the live fleet.** Every vector the engine holds, neither `down` nor with its link `lost`, with its declared capabilities and latest state, replaces the world file's `fleet` in the prompt and in gates 1 to 4; the areas and stations are the world file's. A plan is validated against the fleet that will fly it. At most 16 compiled plans wait for approval, the oldest dropped.
- **Approval** is refused while a mission runs (`409`). It names the mission `MSN-NNN`, after the highest already under `data_dir/missions`, creates its log directory and records its header at once. At the next tick a fresh engine starts, seeded by `seed` or by a value drawn for the mission and recorded in the header, its first batch the join of every vector with a frame plus `plan_approved`: the mission log of section 10.1, as `keelsim` writes it.
- **A mission no longer running** (complete, failed, or its approval refused by the engine) has its last tick recorded and its log closed; the idle engine resumes. `GET /api/v1/missions/{id}` answers the running mission and the final view of the missions this process ran, `404` before a mission's first tick and for others. `GET /api/v1/replays/{id}` streams a finished mission's log, `409` for the running one, whose tail may be a record cut in two.
- **A recording that fails stops the daemon.** The tick's commands are dispatched first, since they may be the stop the vehicles need; no mission runs unrecorded.

### 16.4 Shutdown

On `SIGINT` or `SIGTERM`, a running mission gets a last tick carrying `operator_abort` "daemon shutting down", which stops every vector it set in motion (section 4.5). keeld waits up to 3 s for those vectors to acknowledge, closes the log, closes every stream with `1001`, stops the HTTP server (forcibly after 5 s), then closes every adapter. An approval not started yet is dropped with its header-only log. Exit status `0` after a clean shutdown, `1` on any error.

### 16.5 Pace

keeld starts in real time. `PUT /api/v1/clock` (section 8.1) sets how many times faster than real time the whole closed loop runs, a whole number in [1, 20], 1 being real time.

- **Only a simulated fleet changes pace.** Every bound vector must be time-scalable: a native binding with a `clock` endpoint, a MAVLink binding with `sitl`. Otherwise any pace but 1 is `409`, naming the first vector that is not, and `GET /api/v1/clock` gives the reason in `fixed`. A real vehicle does not fly faster than real time.
- **The whole loop changes pace together**: keeld's pacer, each clock endpoint once, each SITL. Raising the pace sets the simulators first, then the pacer; lowering it, the pacer first, then the simulators. The engine never runs faster than the world: a world left slower would fly its vehicles slower than their cruise speed in mission time, and their frames would read as silence. A simulator refusing the pace or not confirming it within 5 s brings back the ones already changed, itself included, to the previous pace, and the request is `502`. Changes are serialised.
- **The pace is pushed again.** When a native vehicle with a clock endpoint turns `ok` (first contact, a simulator restarted in real time), keeld puts its pace to that endpoint; a SITL's adapter sets `SIM_SPEEDUP` again after a reboot. A keeld restarted in real time thus brings back a simulator an earlier one left fast.
- **The pace changes no decision.** The pacer decides when a tick runs, never what it holds (section 15.3). The pace is not recorded, and a replay runs the recorded batches whatever pace they ran at. The adapters' health thresholds (section 7.1) read the pacer's scaled clock, wall time multiplied by the pace in force at each moment, so a link degraded after 5 s means 5 s of mission time at any pace. Transport timeouts (the native dial, its backoff and idle redial) stay wall clock, being properties of the transport.
- **The live stream carries the pace**, and faster than real time is thinned by it (section 8.2).
