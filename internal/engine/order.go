package engine

import (
	"cmp"
	"math"
	"slices"

	"github.com/dsanchez31/keel/internal/domain"
)

// Ordered iteration and ordered reduction.
//
// Two of the five sources of non-determinism in Go are closed here.
//
// Map iteration order is randomised by the runtime on purpose. A linter
// cannot catch it: `for k := range m` is legal, common, and correct almost
// everywhere except in the decision path, where it makes the output a
// function of the runtime's hash seed. The helpers below are the uniform
// pattern (collect keys, sort, iterate) so that a reviewer looking for a bare
// range over a map has one thing to look for.
//
// Float accumulation is not associative. Summing the same numbers in a
// different order gives a different total, by an amount too small to notice
// and large enough to flip a comparison that decides which vector gets a
// lane. SumFloat sorts before reducing, so the total is a function of the
// multiset rather than of the traversal.

// SortedKeys returns a map's keys in ascending order.
//
// Every iteration over a map in the decision path goes through this, or
// through one of the Sorted* helpers built on it.
func SortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// SortedValues returns a map's values ordered by their key.
func SortedValues[K cmp.Ordered, V any](m map[K]V) []V {
	keys := SortedKeys(m)
	out := make([]V, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

// EachSorted calls fn for every entry of m in ascending key order.
func EachSorted[K cmp.Ordered, V any](m map[K]V, fn func(k K, v V)) {
	for _, k := range SortedKeys(m) {
		fn(k, m[k])
	}
}

// SumFloat adds a slice of float64 in a fixed order.
//
// Sorting first makes the sum depend only on the multiset of values. Sorting
// ascending specifically also improves accuracy: adding the small magnitudes
// first loses fewer low bits than adding them into an already large running
// total.
func SumFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := slices.Clone(xs)
	slices.SortFunc(sorted, func(a, b float64) int {
		// Order by magnitude, then by sign, so -1 and +1 have a fixed
		// relative position instead of comparing equal under a magnitude-only
		// comparator and being left in input order.
		if c := cmp.Compare(math.Abs(a), math.Abs(b)); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	var total float64
	for _, x := range sorted {
		total += x
	}
	return total
}

// SumBy maps a slice to floats and reduces with SumFloat.
func SumBy[T any](xs []T, f func(T) float64) float64 {
	vals := make([]float64, len(xs))
	for i, x := range xs {
		vals[i] = f(x)
	}
	return SumFloat(vals)
}

// MeanFloat is SumFloat divided by the count. Zero elements gives zero rather
// than NaN: a NaN would propagate into a hashed payload and panic the encoder
// somewhere far from the empty slice that caused it.
func MeanFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	return SumFloat(xs) / float64(len(xs))
}

// MinByCost returns the index of the lowest-cost element, comparing with
// CostEpsilon and breaking ties on the key returned by keyOf. The one
// implementation is domain.MinByCost, shared with internal/assign.
func MinByCost[T any](xs []T, costOf func(T) float64, keyOf func(T) string) int {
	return domain.MinByCost(xs, costOf, keyOf)
}

// CompareCost orders two costs, treating any difference below CostEpsilon as
// equality so the caller's identifier tiebreak decides.
func CompareCost(a, b float64) int { return domain.CompareCost(a, b) }

// SortedStrings returns a sorted, deduplicated copy of a string set.
func SortedStrings(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// SortedCells returns a sorted, deduplicated copy of a cell set.
func SortedCells(in []CellID) []CellID {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// SortedVectorIDs returns a sorted, deduplicated copy of a vector id set.
func SortedVectorIDs(in []VectorID) []VectorID {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
