---
status: accepted
date: 2026-09-12
---

# Go for the orchestrator core

## Context and Problem Statement

KEEL's core is concurrent, event-driven work (vehicle links, a WebSocket hub, HTTP, a planner call over the network) wrapped around a decision path that must stay free of concurrency, clocks and I/O. It ships as a daemon, a simulator and a CLI that should run in a disconnected environment. Which language should the core be written in?

## Decision Drivers

* Concurrency natural at the edges and absent from the decision path.
* A single binary with no runtime dependency, deployable without a package manager or network.
* Determinism attainable and enforceable: seeded randomness without global state, a way to ban ambient inputs mechanically.
* Libraries for the transports the project needs: MAVLink v2, WebSocket, YAML.
* Iteration speed for one developer over a short project.

## Considered Options

* Go
* Rust
* Python
* TypeScript on Node.js

## Decision Outcome

Chosen option: "Go", because goroutines and channels at the edges and plain functions in the middle make the purity boundary natural, it compiles to one static binary, `math/rand/v2` gives explicitly seeded sources, and `golangci-lint`'s `forbidigo` can ban the ambient inputs in the four decision-path packages.

### Consequences

* Good, because `keeld`, `keelctl` and `keelsim` are single binaries that run on a distroless base.
* Good, because the decision path's purity is enforced by `internal/` visibility and by lint rather than by review alone.
* Good, because the transports exist as maintained pure-Go libraries: `github.com/bluenviron/gomavlib/v4` for MAVLink, `github.com/coder/websocket`.
* Bad, because Go randomises map iteration on purpose, and iterating a map in the decision path makes output depend on the runtime's hash seed. No linter catches it: keys are sorted through `internal/engine/order.go`, and review and the DST harness catch the rest (spec section 12, requirement 2).
* Bad, because the Go specification allows `a*b + c` to compile to a fused multiply-add on arm64 and not on amd64, so the last bit of a float can differ across architectures. Decision-path code rounds products explicitly with `float64(a*b)`, and the claim stays scoped to the tested platform until a cross-architecture run exists (spec section 12, requirement 7; `design.md` section 10, item 2).
* Neutral, because Go alone does not signal low-level work; the MAVLink codec and the determinism discipline carry that signal.

### Confirmation

* `.golangci.yml` bans wall clock reads, the global random source, `crypto/rand` and environment reads in `internal/engine`, `internal/coverage`, `internal/assign` and `internal/doctrine`.
* Each decision-path package carries a determinism test (`TestDeterminism` in `internal/engine`, `TestAllocateDeterminism`, `TestDecomposeDeterminism` and others), and the DST harness surfaces order dependence as a seed-dependent failure.

## Pros and Cons of the Options

### Go

* Good, because of the concurrency model, single static binaries and fast builds.
* Good, because seeded `math/rand/v2` sources are values that a `State` can carry and copy.
* Bad, because map order and FMA fusion must be closed by discipline, not by the compiler.

### Rust

* Good, because ownership rules out data races at compile time, and an ordered map (`BTreeMap`) is the idiomatic alternative to the randomly seeded `HashMap`.
* Good, because single static binaries as well.
* Bad, because iteration is slower for one developer on a short project, and the async ecosystem adds a runtime choice to every transport.

### Python

* Good, because prototyping is fastest and the LLM ecosystem is richest.
* Bad, because deployment needs an interpreter and dependencies on the target, packaging a disconnected install is its own project, and a 10 Hz loop with many vehicles runs against the interpreter's overhead.

### TypeScript on Node.js

* Good, because types could be shared with the frontend without generation.
* Bad, because one event loop mixes the decision path with I/O scheduling, a `number` is a float64 whose integers are exact only to 2^53 (the `int64` and `uint64` of the log's sequence numbers would need `BigInt`), and deployment carries a runtime.

## More Information

Recorded retroactively on 2026-09-12: the decision was taken during design and holds in the code as built. Reasoning: `design.md` section 9.1; the determinism mechanism: `design.md` section 4.1 and `docs/determinism.md`. The wire types shared with the frontend are generated from the Go types (`tools/sdkgen`), which answers the TypeScript option's one advantage.
