---
status: accepted
date: 2026-09-12
---

# The model chooses the tactic, code computes the geometry

## Context and Problem Statement

A coverage mission ends up as lanes of waypoints over an area of operations. The model compiles the operator's intent ([ADR 0007](0007-the-llm-fence.md)), but how much of the plan should it write: the waypoints themselves, the lanes, or only the choices that need judgement?

## Decision Drivers

* A wrong plan must be detectable before approval.
* Geometry must be identical on every run and every machine, so a plan can be regenerated and replayed without the model.
* The model's output should be small enough to get right and to review.
* Geometry should improve without prompt engineering or model re-evaluation.

## Considered Options

* The model emits four tactical scalars and a rationale; `coverage.Decompose` computes lanes and waypoints
* The model emits the waypoints
* The model emits lane polygons, code routes within them
* No model tactic: one fixed decomposition for every intent

## Decision Outcome

Chosen option: "tactic from the model, geometry from code", because it gives the model the part that needs judgement and code the part that has an exact answer. The Plan IR's tactic is `pattern`, `orientation`, `lanes` (1 to 16) and `overlap_pct`, with a required `rationale`; expansion is deterministic and LLM free, ties broken by lane index then cell id (spec section 5.5). Four lanes north-south at fifteen percent overlap is the model's decision; where those lanes lie over the AO is `coverage.Decompose`'s.

### Consequences

* Good, because a wrong tactic is visible (four lanes where six were asked) while a wrong waypoint 200 m outside the AO would look exactly like a right one.
* Good, because the expanded plan, generated and hashed, is what the operator inspects and approves, and what the log records.
* Good, because geometry work (overlap handling, route clearance, turn cost) is a change to tested Go, evolving independently of the model.
* Bad, because the model can only choose tactics the code implements: `spiral` and `perimeter` are valid Plan IR values with no expansion yet, refused at gate 3 as unsupported.
* Bad, because complete coverage costs waypoints: every cell within half a swath of a pass means a pass turns aside for corner cells, 25 to 35 waypoints per reference lane (`design.md` section 10, item 13).

### Confirmation

* `TestDecomposeGolden` (`internal/coverage/decompose_test.go`) compares decomposition byte for byte against a committed fixture, and `TestDecomposeDeterminism` across repeated runs.
* The canonical schema has no field able to carry a coordinate, and gate 1 refuses unknown fields (`internal/planir/schema.go`).

## Pros and Cons of the Options

### Tactic from the model, geometry from code

* Good, because the model emits a few scalars, not dozens of coordinate pairs it must get exactly right.
* Good, because regenerating a plan needs the Plan IR and the AO, never the model.
* Bad, because the tactic vocabulary is bounded by what expansion implements.

### The model emits the waypoints

* Good, because any pattern the model can describe is expressible.
* Bad, because hallucinated coordinates are undetectable, geometry is the part computers do exactly and a model only approximates, replay would need the model, and the plan becomes dozens of coordinate pairs to emit without error.

### The model emits lane polygons

* Good, because the model shapes the coverage while code routes the passes.
* Bad, because polygons are still coordinates: the same undetectability and the same dependency on the model for replay.

### One fixed decomposition

* Good, because no model in planning at all.
* Bad, because the intent's choices (orientation along a road, lane count for the fleet at hand, overlap for the sensor) are lost.

## More Information

Recorded retroactively on 2026-09-12: the decision was taken at the start of design and holds in the code as built. Normative: `spec.md` sections 5.1 (schema), 5.2 (hard constraints) and 5.5 (expansion). Reasoning: `design.md` section 5.
