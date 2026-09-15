# Architecture decision records

The decisions that shape KEEL, one per file, in [MADR 4.0](https://adr.github.io/madr/) format: the problem, the drivers, the options considered, the one chosen and why, its consequences, and how the decision is confirmed in the code (a test, a lint rule, a command).

| ADR | Decision | Status | Date |
|---|---|---|---|
| [0001](0001-event-log-as-source-of-truth.md) | The append-only, hash-chained event log is the mission's source of truth | accepted | 2026-09-12 |
| [0002](0002-go-for-the-core.md) | Go for the orchestrator core | accepted | 2026-09-12 |
| [0003](0003-no-database.md) | No database: the event log is the system of record | accepted | 2026-09-12 |
| [0004](0004-logical-tick-over-wall-clock.md) | A logical tick of 100 ms, not the wall clock, drives the engine | accepted | 2026-09-12 |
| [0005](0005-mavlink-as-first-adapter.md) | MAVLink v2 to ArduPilot as the first real-vehicle adapter | accepted | 2026-09-12 |
| [0006](0006-offline-cartography-without-cesium-ion.md) | Offline cartography, without Cesium ion or any tile server | accepted | 2026-09-12 |
| [0007](0007-the-llm-fence.md) | The LLM is a compiler frontend, fenced above the validator and the human gate | accepted | 2026-09-12 |
| [0008](0008-tactic-from-model-geometry-from-code.md) | The model chooses the tactic, code computes the geometry | accepted | 2026-09-12 |
| [0009](0009-accelerated-live-simulation.md) | A simulated fleet runs faster than real time, the whole closed loop together | accepted | 2026-09-12 |

0001 to 0008 were recorded retroactively on 2026-09-12, after the system was built: each states a decision taken during design or development that holds in the code. 0009 was recorded as its decision was taken. The reasoning they summarise lives at length in `design.md`, the normative rules in `spec.md`; each ADR names its sections.

## Adding a decision

* Take the next number, four digits, and a file name stating the decision: `0010-<decision-in-kebab-case>.md`.
* One decision per file, following the MADR 4.0 sections of the existing records, with a `Confirmation` section naming what holds the decision in the code.
* An accepted ADR is not rewritten in substance. A decision that changes gets a new ADR; the old one's `status` becomes `superseded by ADR-NNNN`, and the new one links back to it.
* A behaviour change the ADR introduces goes into `spec.md` as well: the ADR records why, the spec what.
