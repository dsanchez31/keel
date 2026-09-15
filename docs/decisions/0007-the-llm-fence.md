---
status: accepted
date: 2026-09-12
---

# The LLM is a compiler frontend, fenced above the validator and the human gate

## Context and Problem Statement

An operator states intent in natural language ("grid-search the unexplored area east of the river"), which only a language model turns into a plan. A language model is non-deterministic by construction and can be wrong with confidence, while KEEL moves physical vehicles and must reproduce and explain every decision. Where does the model belong in the system, and what keeps its output from reaching a vehicle unchecked?

## Decision Drivers

* Ambiguous human intent must become an executable plan.
* Nothing the model produces may move a vehicle without validation and human approval.
* Everything below approval must be deterministic and replayable ([ADR 0001](0001-event-log-as-source-of-truth.md)).
* Safety must be a property of the system, not of the model's quality: it must hold with a weak local model.
* The repository must run without credentials, so the default backend is local.

## Considered Options

* The LLM as a compiler frontend: intent to a closed Plan IR, deterministic expansion, a four-gate validator, a bounded repair loop, a human gate, then the engine with no model in it
* An LLM inside the control loop, reacting to telemetry during the mission
* No LLM: plans built through forms
* An LLM planning and executing directly, constrained by its prompt

## Decision Outcome

Chosen option: "the LLM as a compiler frontend", because it puts the model where being wrong is cheap. Above the fence a wrong plan is refused by the validator, retried by the repair loop or discarded by the operator; below it, the engine is pure, seeded and hash chained, and the model is never consulted again.

Three properties make the fence structural:

* **The Plan IR cannot express a coordinate.** The schema has no field able to hold one; `mission.area` is a name resolved server side, and an unknown name is a validation failure (spec section 5.2).
* **The model chooses the tactic, code computes the geometry** ([ADR 0008](0008-tactic-from-model-geometry-from-code.md)).
* **A human approves** a content-addressed plan, rendered for inspection, before anything moves.

### Consequences

* Good, because the approved plan is recorded whole in the log's `plan_approved` event, so a replay never calls a model.
* Good, because the backend is pluggable (Ollama locally by default, Claude opt-in) and swapping them shows that safety lives in the validator.
* Bad, because the repair loop stops after three attempts and then produces no plan, the most likely thing to be visibly imperfect in a demo with a small local model. That is the correct behaviour: nothing is relaxed to force success (`design.md` section 10, item 4).
* Bad, because structured-output modes reject some schema keywords, so the model sees a looser schema than gate 1 enforces and can spend an attempt on a bound it could not see (`design.md` section 10, item 8).
* Bad, because the human gate is modelled, not enforced: there is no authentication behind the approval (`design.md` section 10, item 5).

### Confirmation

* The canonical Plan IR schema (`internal/planir/schema.go`) is closed and has no coordinate field; gates 1 to 4 run in order in `internal/planir/validate.go`, with `TestValidateIsDeterministic`.
* `internal/planner/repair.go` bounds the loop at three attempts.
* Replaying a log (`keelctl replay`, `TestReferenceReplay`) needs no planner, no network and no model.

## Pros and Cons of the Options

### The LLM as a compiler frontend

* Good, because model output is data checked before use, never an action.
* Good, because every run after approval is reproducible.
* Bad, because a compilation can fail, and the operator must read and approve.

### An LLM inside the control loop

* Good, because it could react to situations no doctrine rule anticipates.
* Bad, because every reaction would be non-reproducible and unreviewed, at the moment when latency and correctness matter most. Reaction belongs to doctrine, evaluated deterministically.

### No LLM

* Good, because fully deterministic end to end.
* Bad, because the operator must translate intent into parameters by hand, the problem the system exists to solve.

### An LLM constrained by its prompt

* Good, because the least machinery.
* Bad, because "the model is told not to" is not a guarantee: a hallucinated coordinate looks exactly like a correct one, and nothing would catch it before the vehicle does.

## More Information

Recorded retroactively on 2026-09-12: the decision was taken at the start of design and holds in the code as built. Normative: `spec.md` section 5 (Plan IR, hard constraints, validation, repair loop, expansion, content addressing). Reasoning: `design.md` section 2. The determinism below the fence: `docs/determinism.md`.
