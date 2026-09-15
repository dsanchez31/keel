---
status: accepted
date: 2026-09-12
---

# No database: the event log is the system of record

## Context and Problem Statement

Given that the mission log is the source of truth ([ADR 0001](0001-event-log-as-source-of-truth.md)), does KEEL also need a database, for mission state, history or queries?

## Decision Drivers

* One source of truth: two stores that can disagree need a rule for which one wins.
* One-command startup from a clean clone, with no credentials.
* Operational surface proportional to a single orchestrator.
* Any query model should be rebuildable, so it cannot corrupt the record.

## Considered Options

* No database: the log, with any query model derived from it when a need justifies one
* A PostgreSQL projection of mission state, maintained beside the log
* A SQLite projection now
* PostgreSQL, Redis and a message bus

## Decision Outcome

Chosen option: "no database", because a database would be a projection of the log, a projection is rebuildable while the log is not, and nothing KEEL does today needs the projection. Event-sourced state with deterministic replay makes the log the system of record and any query model derived; a SQLite projection is the planned step if query needs justify it, and it would change nothing about correctness.

### Consequences

* Good, because there is no second store to keep consistent, migrate or back up.
* Good, because `docker compose up` starts the SITL vehicles, keelsim, keeld and the frontend, and nothing else.
* Bad, because keeld holds the final view of the missions it ran in memory only: after a restart `GET /api/v1/missions/{id}` answers 404 while their logs still replay (`design.md` section 10, item 18). The durable fix is to rebuild a view by replaying its log through `missionlog.Replay`, the code path `keelctl replay` runs.
* Bad, because a question across missions (all missions where DRONE-02 lost its link) means scanning or replaying logs.

### Confirmation

* `go.mod` carries no database driver, and `docker-compose.yml` no database service.
* Mission ids are numbered from the directories under `data_dir/missions`, not from a table.

## Pros and Cons of the Options

### No database

* Good, because the log is the only state that must survive, and it is append-only.
* Bad, because queries are replays until a projection exists.

### A PostgreSQL projection maintained beside the log

* Good, because cross-mission queries become SQL.
* Bad, because a service to run, credentials to hold and a write path that can diverge from the log on a crash between the two writes.

### A SQLite projection now

* Good, because embedded, no service, rebuildable from logs.
* Neutral, because it is the right shape for the first real query need.
* Bad, because today it would be code and schema with no consumer.

### PostgreSQL, Redis and a message bus

* Good, because it is the conventional platform shape.
* Bad, because it buys operational surface, not capability, for one orchestrator writing one stream.

## More Information

Recorded retroactively on 2026-09-12: the decision was taken during design and holds in the code as built. Reasoning: `design.md` section 9.3. Scope: `spec.md` section 1 (single orchestrator, no multi tenancy).
