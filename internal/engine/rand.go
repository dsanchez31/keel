package engine

import (
	"math/rand/v2"
)

// Randomness in the decision path.
//
// Spec section 12.3: all randomness comes from an explicitly seeded source
// carried in state, and the global source is forbidden. The ban is enforced by
// forbidigo; this file is what makes the ban livable.
//
// Rand is a value type wrapping rand.PCG, and that is the whole design. A
// *rand.Rand carried in State would be shared by every copy of that State, so
// two branches of a DST run, or a state snapshot taken for an assertion, would
// draw from one stream and consume each other's numbers. Copying a Rand copies
// its position, so a State copy is a genuinely independent continuation.

// Rand is a seeded, copyable PCG source.
//
// The zero value is not usable: call NewRand. A zero PCG produces a valid but
// meaningless stream, and silently accepting it would let a state built by
// struct literal look deterministic while being seeded with nothing.
type Rand struct {
	pcg  rand.PCG
	seed uint64
	init bool
}

// NewRand returns a source seeded from a single 64-bit seed.
//
// The two PCG words are derived from the one seed by SplitMix64, so a caller
// only has to carry one number and a seed of 0 is still a usable stream. The
// DST harness prints that number in its reproduction command, so a failing
// seed is a complete bug report.
func NewRand(seed uint64) Rand {
	s := splitMix64(seed)
	lo := s.next()
	hi := s.next()
	return Rand{pcg: *rand.NewPCG(lo, hi), seed: seed, init: true}
}

// Seed is the seed the source was created from.
func (r Rand) Seed() uint64 { return r.seed }

// Initialised reports whether the source was created by NewRand.
func (r *Rand) Initialised() bool { return r.init }

// Uint64 draws the next value and advances the source.
func (r *Rand) Uint64() uint64 {
	r.mustInit()
	return r.pcg.Uint64()
}

// Uint64N draws a value in [0, n), uniformly. Panics for n == 0.
func (r *Rand) Uint64N(n uint64) uint64 {
	r.mustInit()
	// Lemire's multiply-shift rejection method: unbiased, and the rejection
	// loop terminates with probability 1 in a bounded expected number of
	// draws, so it is safe inside the decision path.
	if n == 0 {
		panic("engine: Uint64N with n == 0")
	}
	threshold := (-n) % n
	for {
		v := r.pcg.Uint64()
		if v >= threshold {
			return v % n
		}
	}
}

// IntN draws a value in [0, n).
func (r *Rand) IntN(n int) int {
	if n <= 0 {
		panic("engine: IntN with n <= 0")
	}
	return int(r.Uint64N(uint64(n)))
}

// Float64 draws a value in [0, 1) using the top 53 bits, which is the only
// construction that gives a uniform spacing over the representable doubles.
func (r *Rand) Float64() float64 {
	return float64(r.Uint64()>>11) / (1 << 53)
}

// Clone returns an independent source at the same position. Explicit at call
// sites that want to fork a stream, so forking is visible in review.
func (r Rand) Clone() Rand { return r }

func (r *Rand) mustInit() {
	if !r.init {
		panic("engine: Rand used before NewRand, state was built without a seed")
	}
}

// splitMix64 derives the PCG words from one seed. Stateless, fixed constants,
// identical on every platform.
type splitMix64 uint64

func (s *splitMix64) next() uint64 {
	*s += 0x9E3779B97F4A7C15
	z := uint64(*s)
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}
