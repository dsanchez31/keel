package transport

// The TypeScript wire types of @keel/sdk are generated from this package,
// internal/domain and internal/planir by tools/sdkgen, a module of its own so
// the generator's dependencies stay out of this one's build (spec section 8).
// make generate runs it, make check-sdk fails when the committed output
// differs from a fresh one.
//
// The paths after `run .` are relative to tools/sdkgen, where `go -C` runs it.

//go:generate go -C ../../tools/sdkgen run . -root ../.. -out ../../sdk-ts/src/generated/wire.ts
