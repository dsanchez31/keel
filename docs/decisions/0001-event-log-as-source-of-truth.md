---
status: accepted
date: 2026-09-12
---

# The append-only, hash-chained event log is the mission's source of truth

## Context and Problem Statement

KEEL moves physical vehicles on decisions an operator must be able to explain after the fact and a reviewer must be able to reproduce. Something has to be the system of record for a mission: what the engine was told, what it decided, what it commanded. What should that record be, so that "why did it do that" and "would it do it again" both have exact answers?

## Decision Drivers

* Replay must reproduce every decision, not describe them: the same `engine.Step`, fed the same inputs, from the same initial state.
* Tamper evidence: an edited record must be detectable, and two runs comparable by one value.
* Explanation: every decision recorded with its full rationale, candidates and rules (spec section 10.2).
* One format for the simulator (`keelsim`) and the daemon (`keeld`), so one replay verifies both.
* Streamable: a mission of any length is read without holding it in memory.

## Considered Options

* An append-only log of the inputs as given, the decisions and the commands, SHA-256 hash-chained, replayed through `Step`
* Mutable mission state in a database, with an audit table of decisions
* A log of the decisions only
* An event store on a message broker

## Decision Outcome

Chosen option: "an append-only, hash-chained log of inputs, decisions and commands", because it is the only option in which replay is the live code path fed recorded input, and in which the record proves its own integrity.

A mission log is a header (seed, tick interval, engine configuration, plan hash, mission id), then for every tick the events `Step` was given, in the order given, its decisions, its commands and a tick summary. Each record carries `Hash = SHA256(canonical(seq, tick_ms, kind, payload, prev_hash))`. `internal/missionlog` is the one writer for keelsim and keeld and the one reader, and `missionlog.Replay` rebuilds the chain through the same writer.

### Consequences

* Good, because a verifying replay compares record by record and names the first that differs, not only a head that differs a thousand records later.
* Good, because the head hash is a single value that two runs, a replay and a browser can compare, and that the frontend displays.
* Good, because derived views (mission state, replay windows, doctrine diffs) are rebuilt from the log by the same `Step`, never maintained beside it.
* Bad, because the log is large: the 38 minute reference mission is 142,416 records, 62 MB of ndjson, dominated by telemetry.
* Bad, because the log records which doctrine pack ran and its hash, not the pack's content, so a log replays only beside the packs it ran under (`design.md` section 10, item 10).
* Bad, because keeld keeps finished mission views in memory only, and they do not survive a restart although their logs still replay (`design.md` section 10, item 18).

### Confirmation

* `TestReferenceReplay` (`cmd/keelsim/reference_test.go`) records the reference mission, replays it verifying every record, and holds its chain to the heads pinned in `cmd/keelsim/testdata/reference.heads`.
* The DST harness (`internal/engine/dst_test.go`) ends every seed with a verifying replay of its own log (invariants I6 and I7).
* `keelctl replay <log> --verify-hash` exits 2 on a broken chain or a divergence.
* The browser recomputes the chain from the log's bytes (`sdk-ts/src/chain.ts`), pinned to the Go encoder by `TestSDKChainGolden`.

## Pros and Cons of the Options

### Append-only log of inputs, decisions and commands, hash-chained

* Good, because recording the inputs is what makes replay possible: `Step` is pure, so the inputs and the initial state determine everything.
* Good, because the chain gives tamper evidence and single-value equality.
* Neutral, because segments (8 MiB) are an artifact of storage; the chain and `seq` run across them.
* Bad, because every query is a replay or a scan, and the file grows with telemetry.

### Mutable state in a database, with an audit table of decisions

* Good, because queries over current state are cheap.
* Bad, because the state is the output of the decisions, not their input: nothing re-derives a decision from it, so reproduction is impossible and explanation is whatever the audit table happened to record.
* Bad, because an update in place destroys the history a replay needs.

### A log of the decisions only

* Good, because it is small and explains every decision.
* Bad, because it cannot be replayed: without the telemetry and operator events that led to a decision, nothing checks that the engine would decide the same again.

### An event store on a message broker

* Good, because it scales to many producers and consumers.
* Bad, because it adds a service to operate for a single orchestrator writing one stream, and a broker's ordering and retention are one more thing a replay guarantee would depend on.

## More Information

Recorded retroactively on 2026-09-12: the decision was taken during design and holds in the code as built. Normative: `spec.md` section 10 (records, mission log layout, replay, doctrine diff) and section 9 (canonical encoding). Reasoning: `design.md` sections 3.1 and 4.2. How to check it: `docs/determinism.md`. Related: [ADR 0003](0003-no-database.md), [ADR 0004](0004-logical-tick-over-wall-clock.md).
