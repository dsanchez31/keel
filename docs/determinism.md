# Determinism in KEEL

KEEL claims that its decisions are reproducible: given the same seed and the same events, the engine makes the same decisions, issues the same commands and writes the same log, byte for byte, and a single SHA-256 head hash proves it. This document is a guide to checking that claim rather than taking it on trust: what each mechanism is, where it is enforced, and the commands that exercise it.

It is not the normative text. The rules themselves live in `spec.md` (section 9, canonical encoding; section 10, the log, the chain and replay; section 11, invariants; section 12, determinism requirements), and the reasoning behind them in `design.md` (section 2, the fence; section 4, determinism). Where this document and the spec disagree, the spec wins and this document has a defect.

---

## 1. The claim, and its scope

**Same seed, same event sequence, byte-identical output.** `engine.Step` given the same state and the same batch returns the same state, commands and decisions, and a mission log written from those is identical down to the last hash.

Two consequences follow, and they are different things:

- **A simulated mission is reproducible from its seed.** `keelsim` closes the loop over a seeded world (`internal/world`), so the scenario file and its seed determine everything, the telemetry included. Two runs print the same head.
- **A live mission is reproducible from its log.** `keeld` drives real or simulated vehicles over a network, and which frame lands in which tick depends on packet timing. That is input, not a decision: the log records every batch as `Step` received it, and replaying the log reproduces every decision and every hash. The seed of a live mission is recorded in its header, drawn per mission unless the configuration fixes it.

**Scope.** The claim holds within one Go version on one platform, and is tested on `linux/amd64` with Go 1.26.0. Cross-architecture equality is designed for (section 3, fused multiply-add) but has not been run; `design.md` section 10, item 2 records the gap, and section 9, step 8 below is the command that would close it.

---

## 2. The fence: where non-determinism is allowed

A language model cannot be made deterministic, so KEEL puts it where it does not need to be.

```
intent  ->  PLANNER  ->  Plan IR  ->  EXPANDER  ->  VALIDATOR  ->  HUMAN GATE
                                                                       |
======================================================================= the fence
                                                                       |
                                                        ENGINE  ->  commands
```

**Above the fence, output may vary.** Two compilations of one intent may yield different Plan IR, even at temperature 0. That is survivable: the validator refuses what is wrong, the repair loop retries at most three times, and the operator approves or discards what remains.

**What crosses the fence is a value, not a process.** The Plan IR carries four tactical scalars and a rationale, never a coordinate. Expansion (`coverage.Decompose`) turns them into lanes and waypoints deterministically, and the resulting `ApprovedPlan` is content addressed: its hash is the SHA-256 of its canonical encoding. The approval event records the whole expanded plan, grid and waypoints included, and the header names its hash:

```json
{"kind":"header","payload":{"config":{...},"mission":"MSN-042","name":"reference","plan":"ac208ba81396...","seed":42,"tick_interval_ms":100},...}
{"kind":"event","payload":{"kind":"plan_approved","plan":{"area":{"grid":{"cell_m":50,"cols":116,...
```

**Below the fence, nothing is uncertain.** A replay reads the plan from the log. It never calls a model, never resolves an area name against a world file, never runs expansion again: the plan the engine flew is the plan in the log.

The parts above the fence that can be deterministic are, and are tested as such: the prompt a backend receives (`TestOpeningIsDeterministic` in `internal/planner`), schema diagnostics (`TestSchemaDiagnosticsAreDeterministic`) and validation (`TestValidateIsDeterministic`) in `internal/planir`, decomposition against a golden fixture (`TestDecomposeGolden`).

---

## 3. The decision path and the sources of non-determinism

Four packages make up the decision path: `internal/engine`, `internal/coverage`, `internal/assign`, `internal/doctrine`. They are pure functions over in-memory state: no I/O, no clock, no concurrency. Everything else (`planner`, `planir`, `eventlog`, `world`, `transport`, `daemon`, the adapters) sits outside and may do I/O. Putting the four under `internal/` means nothing outside the module can call into them in a way they were not designed for.

Go has five ordinary sources of non-determinism and one architectural one. Each is closed, and each closure has a place where it is enforced.

| Source | Closure | Enforced by |
|---|---|---|
| Wall clock | Mission time is an `int64` of milliseconds advanced by exactly one tick (100 ms) per `Step`. Nothing in the decision path reads the time of day | `forbidigo` in `.golangci.yml`: `time.Now`, `time.Since`, `time.Until`, `time.Tick`, `time.After`, `time.NewTimer`, `time.NewTicker`, scoped to the four packages; invariant I5 in the DST harness |
| Map iteration | Keys are collected, sorted, then iterated, through the helpers of `internal/engine/order.go` (`SortedKeys`, `SortedValues`, `EachSorted`) so a reviewer has one pattern to look for | Review, since `for k := range m` cannot be linted away; the DST harness, where order dependence surfaces as a seed-dependent failure. The one map the canonical encoder walks is sorted by key before encoding |
| Goroutine scheduling | `Step` starts no goroutine and reads no channel. The daemon is concurrent; a decision never is | Review; `forbidigo` bans `runtime.NumGoroutine` in the four packages |
| Randomness | One seeded source, `engine.Rand` (`internal/engine/rand.go`): a value type wrapping `math/rand/v2`'s PCG, its two words derived from one 64-bit seed by SplitMix64, carried in `State`. Copying a `State` copies the stream's position, so a snapshot is an independent continuation rather than a second reader of one shared stream | `forbidigo`: every package-level function of `math/rand` and `math/rand/v2`, and `crypto/rand` |
| Float accumulation | Addition is not associative: summing costs in a different order shifts the total by an amount too small to notice and large enough to flip a comparison. Every reduction sorts first (`SumFloat`), costs compare with an explicit epsilon, ties break on `VectorID` | Review; the per-package determinism tests and the DST harness |
| Fused multiply-add | `a*b + c` may compile to one FMA instruction that rounds once instead of twice. arm64 fuses, amd64 does not, and the last bit differs. Decision-path code writes `float64(a*b) + c`, which the Go specification guarantees is never fused (spec section 12, requirement 7) | Review. Not tested until an arm64 run exists (section 9, step 8) |

Ambient input is closed the same way: `os.Getenv`, `os.LookupEnv` and `os.Environ` are banned in the four packages, and configuration reaches the engine through `State`, whose `Config` is recorded in the log's header.

**The pacer decides when a tick runs, never what it does.** `internal/pacer` holds the only wall clock on the loop. `pacer.Fast` runs ticks back to back; `pacer.RealTime` sleeps until each tick is due, and runs late ticks back to back rather than skipping them, which a `time.Ticker` would not do. Both drive the same `Step` with the same batches, so a headless run and a real-time run of one scenario record the same chain (`cmd/keelsim/loop_test.go`).

---

## 4. Canonical encoding

Everything hashed or compared goes through one encoder, `eventlog.Canonical` in `internal/eventlog/codec.go`. A second, slightly different encoder anywhere would make two runs that agree on every decision disagree on their head hash.

- JSON, UTF-8, no insignificant whitespace.
- Object keys sorted by Unicode code point.
- Integers without exponent; floats through `strconv.FormatFloat(f, 'g', 17, 64)`, which round trips exactly: a cost is recorded as `2647.5319048288243`, not `2647.53`.
- Absent optional fields omitted, never `null`.
- No `NaN`, no infinity. Encoding one is an error, and a bug: tests panic on it.
- Slices keep their order. **A slice that represents a set is sorted when it is built, not when it is encoded.** `Capabilities.Tags` and `Lane.Cells` are sets; waypoint paths and candidate lists are sequences whose order is their meaning. Sorting in the encoder would hide an unsorted set rather than fix the code that built it, and would silently reorder the sequences.

---

## 5. The hash chain

Every record of a log links to the one before it:

```
Hash = SHA256(canonical({kind, payload, prev_hash, seq, tick_ms}))
```

`seq` is global and starts at 1, the first record's `prev_hash` is 32 zero bytes, and in the preimage the two byte strings (`payload`, the payload's canonical encoding, and `prev_hash`) are standard base64, as the canonical encoder renders a `[]byte`. On disk a record is one line of ndjson, the payload spliced in as the JSON it is and the digests in lowercase hex:

```json
{"hash":"4d0575458fee...","kind":"header","payload":{...},"prev_hash":"0000000000000000...","seq":1,"tick_ms":0}
```

This gives three properties:

- **Tamper evidence.** Changing one byte of one record changes its digest, which no longer matches the `prev_hash` of the next one.
- **Equality in one value.** Two runs are compared by their head hashes, not by diffing two logs.
- **A visible artifact.** The head is rendered live in the frontend's time strip and recomputed during a replay.

**A mission log** (`internal/missionlog`, spec section 10.1) is a `header` record at 0 ms (name, seed, tick interval, engine configuration, plan hash, mission id), then for every tick, stamped with its mission time: one `event` record per event `Step` was given, **as given and in the order given**, one `decision` record per decision, one `command` record per command, and a `tick` record with the coverage and the mission state. The events are recorded before the engine sorts them, so a replay feeds `Step` exactly the batch the live loop did. keelsim and keeld write through the same `AppendHeader` and `AppendTick`: one layout, one reader.

Segments (`*.keellog`, rolled at 8 MiB) are an artifact of storage. The chain and `seq` run across them unbroken, and their concatenation is the log.

---

## 6. Replay

**Replay is `Step` fed recorded input, not a second implementation.** `missionlog.Replay` (`internal/missionlog/replay.go`) builds the initial state from the header (`engine.NewState(seed, config, packs)`), feeds `Step` each recorded batch, and rebuilds the chain through the same `AppendHeader` and `AppendTick` the live loops use. A replay path of its own would be one that can drift from the live one.

- **The recorded chain is checked as it is read** (I6): every record must carry the next `seq`, link to the previous digest and carry the digest its content implies. A break is an error, since nothing after it can be trusted.
- **A verifying replay compares record by record**, by digest, and stops at the first record that differs, reporting its `seq`, its mission time and both records (I7). A forged decision is found at its own record, not at a head that differs a thousand records later.
- **Doctrine packs are pinned by hash.** The log records which pack a plan ran under and its expected hash, and a replay against a pack edited under the same version refuses the approval and diverges there, naming both hashes (spec section 4.5). A log therefore replays only beside the packs it ran under (`design.md` section 10, item 10).
- **It streams.** One tick of the recording and one of the rebuilt chain are held at a time, whatever the mission's length.

The frontend's replay route uses the same code. keeld replays a finished log through `missionlog.Replay` and projects windows of it with the same `transport.TickMessages` the live stream uses (`GET /api/v1/replays/{id}/frames`), which `@keel/sdk` folds with the same `applyFrame` as live frames. Independently, a worker in the browser recomputes the whole chain from the log's bytes (`sdk-ts/src/chain.ts`, WebCrypto), hashing each payload as the exact bytes on the line rather than parsing and re-encoding it. At the end of the mission the page shows three heads side by side: the recorded one as keeld read it, the one keeld rebuilt, and its own. They are equal. The Go encoder writes a small golden chain (`internal/eventlog/sdk_golden_test.go`, `TestSDKChainGolden`) that the TypeScript test (`sdk-ts/src/chain.test.ts`) must hash to the same head, so the two implementations of the preimage cannot drift apart.

---

## 7. The tests that hold the claim

| Test | Asserts |
|---|---|
| `TestDeterminism` in `internal/engine` | A seeded run of 400 ticks produces the same head hash, clock and coverage across 100 repetitions |
| `TestDeterminism` in `internal/doctrine` and `internal/world`; `TestAllocateDeterminism` in `internal/assign`; `TestDecomposeDeterminism` and `TestRedecomposeDeterminism` in `internal/coverage`; `TestParseDeterministic` in `internal/doctrine` | Each decision-path package, and the simulated world, returns identical output across repeated seeded runs |
| `TestDecomposeGolden` in `internal/coverage` | Decomposition matches a committed fixture byte for byte |
| `TestReferenceReplay` in `cmd/keelsim` | The reference mission, recorded by the current code, replays verifying every record, and its chain matches the heads pinned every 100 s of mission time and at the end (`cmd/keelsim/testdata/reference.heads`). The DST harness replays logs the code it tests has just written, so a change that moves both sides together is invisible to it; the pinned heads were written by an earlier engine and are not. The 62 MB recording itself is not versioned: the seed rebuilds it byte for byte |
| `TestDST` in `internal/engine` | The reference mission under seeded faults, invariants I1 to I9 at every tick boundary, each run ending with a verifying replay of its own log (section 8) |
| `cmd/keelsim/loop_test.go` | Headless-fast and real-time runs record the same head; the reference run completes |
| `TestSDKChainGolden` and `sdk-ts/src/chain.test.ts` | The Go chain and the browser's chain check agree on one golden log |

---

## 8. Deterministic simulation testing

Unit tests check what their authors thought of. The DST harness (`internal/engine/dst_test.go`), after the approach of FoundationDB and TigerBeetle, checks what faults produce.

Each seed runs the reference mission through keelsim's closed loop (the engine, the mission log, `internal/world`) and seeds the engine and the world. From a stream of its own, salted so it never shares numbers with them, it draws a schedule:

- one to four world faults up to tick 8000: kills (at most two), link loss and stale telemetry for 3 to 30 s or for good, battery drains of 20 to 70 percent, GPS drift of 1 to 10 m/s for 5 to 30 s;
- a hot swap, pinned by hash, on half the seeds;
- on the wire between the world and the engine, telemetry frames dropped, duplicated or delayed by up to 3 s at 0.5 percent each, and one batch in 50 reordered.

At every tick boundary it asserts invariants I1 to I9 (spec section 11) on the engine's state: no cell held by two lanes, coverage monotonic, pending work allocated within its deadline whenever the allocator can place it, no vector at work outside its constraint envelope without an `rtb`, the clock advancing by exactly one tick, the mission ending by coverage or the stall rather than the tick ceiling, a hot swap never changing the mission state or the coverage. The run ends with a verifying replay of its log (I6 and I7) on a mission shaped by faults, where an unsorted map or reduction would show.

The engine is pure and the world is seeded, so **a failing seed is a complete bug report**. The harness prints the violation, the seed's schedule and the command that reproduces it, and the failure reproduces exactly on every run:

```
go test ./internal/engine -run 'TestDST$' -count=1 -dst.seed=<seed>
```

`-dst.seeds` (8 by default, 2 under `-short`) and `-dst.from` (1) choose the sweep.

What it asserts is what the engine believes: the positions the fleet reported, the batteries it announced. A vehicle the world has flown outside the AO while its drifting GPS reported it inside satisfies I4 (`design.md` section 10, item 21).

---

## 9. Check it yourself

Every step runs from a clean clone with no credentials: the reference scenario compiles no intent, its Plan IR is committed (`examples/plans/reference.json`).

**1. Two runs, one head.** The reference scenario (seed 42, mission `MSN-042`), headless:

```sh
go run ./cmd/keelsim --quiet
go run ./cmd/keelsim --quiet
```

Both print `complete after 2265.4 s of mission time (22654 ticks)`, 5711 of 5711 cells, and `head 9e9213a9788821be88a749ebcab9de6f2c48f8f7a8b824af8dde2d88c7442441` (linux/amd64, Go 1.26.0).

**2. Real time records the same chain.** The same scenario paced on the wall clock, at 100 times real time (about 23 s):

```sh
go run ./cmd/keelsim --quiet --realtime --speed 100
```

**3. Record it, then replay the recording.** `make reference-log` writes that run to `examples/replays/run-042.log`, the eight segments concatenated, in about a second. It is not versioned: the seed rebuilds the same 62 MB byte for byte, and its chain is what the repository pins (step 5).

```sh
make reference-log
go run ./cmd/keelctl replay examples/replays/run-042.log --verify-hash
```

It prints the twelve decisions of the mission (the link loss on DRONE-02 at 60 percent coverage and the redecomposition that follows it among them), both heads, and exits 0.

**4. Break it.** Change the last digit of one candidate's cost in record 10, an allocation decision:

```sh
sed '10s/2647.5319048288243/2647.5319048288244/' examples/replays/run-042.log \
  | go run ./cmd/keelctl replay - --verify-hash
echo "exit $?"
```

The chain breaks at record 10, and the exit status is 2.

**5. The current code against the pinned chain.**

```sh
go test ./cmd/keelsim -run TestReferenceReplay -count=1
```

After a deliberate change of behaviour it fails at the first pinned head that moved, which bounds the change to 100 s of mission time, and `make record-reference` pins the heads again.

**6. A DST sweep.**

```sh
go test ./internal/engine -run 'TestDST$' -count=1 -dst.seeds=64
```

**7. In the browser.** Hand the recording to a local keeld as a finished mission, start keeld and the frontend (`README.md`), and open `/replays/MSN-042`:

```sh
make demo-replay
```

The page replays the mission on the globe from keeld's windows while a worker recomputes the chain from the log's bytes, and ends on three equal heads: recorded, rebuilt by keeld, computed by the page. A mission keeld recorded itself replays the same way, from the command line too:

```sh
curl -s http://127.0.0.1:8080/api/v1/replays/MSN-043 | go run ./cmd/keelctl replay - --verify-hash
```

**8. Across architectures (not run yet).** On an amd64 Linux host with `qemu-user` registered through `binfmt_misc`, Go executes arm64 test binaries under emulation:

```sh
GOARCH=arm64 go test ./internal/engine -run TestDeterminism -count=1
GOARCH=arm64 go test ./cmd/keelsim -run TestReferenceReplay -count=1
```

The reference heads were pinned on amd64, so a pass extends the claim to arm64 and a divergence names the first 100 s window where an FMA or a function of package `math` rounded differently; `make reference-log` on both platforms and `cmp` then name the record. Until it has been run, the claim stays scoped to one platform (section 1).

---

## 10. What the claim does not cover

- **The planner.** Two compilations of one intent may differ; that is why the model sits above the fence and the plan is recorded, not recomputed (section 2).
- **Other architectures and Go versions**, until tested (section 9, step 8). Functions of package `math` are outside the explicit-rounding rule.
- **A log without its packs.** The pin detects an edited pack; it cannot recover the original (`design.md` section 10, item 10).
- **The physical truth.** The engine decides on what the fleet reports; determinism makes its decisions reproducible, not its inputs correct (`design.md` section 10, item 21).
- **Live timing.** A live mission cannot be re-run from its seed, since packet timing decides which frame lands in which tick. Its log replays exactly, which is the guarantee that matters after the fact (section 1).
