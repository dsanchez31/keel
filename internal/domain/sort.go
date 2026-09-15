package domain

import (
	"cmp"
	"slices"
)

// sortSlice is a stable sort with an explicit comparator.
//
// Stability matters: an unstable sort is free to permute equal elements, and
// "equal under this comparator" is not "identical". A run that reorders equal
// elements differently produces a different canonical encoding and therefore a
// different hash, which is a determinism break that only appears at scale.
func sortSlice[T any](s []T, cmpFn func(a, b T) int) {
	slices.SortStableFunc(s, cmpFn)
}

// compareString is the total order used for every id comparison in the
// decision path: Unicode code point order, matching the canonical encoder's
// key ordering.
func compareString(a, b string) int { return cmp.Compare(a, b) }
