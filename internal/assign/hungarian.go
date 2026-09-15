package assign

import "math"

// hungarian solves the rectangular assignment problem: it gives each row a
// distinct column so that the total cost is minimal, and returns the column
// of each row. It requires len(cost) <= len(cost[i]) for every row.
//
// This is the Kuhn-Munkres algorithm with potentials, in O(n²m). Costs are
// integers on purpose: every comparison inside the algorithm is exact, so no
// epsilon can decide an assignment and the result is a pure function of the
// matrix. The traversal order is fixed (rows and columns ascending), so among
// several optimal assignments the same one is returned on every run.
func hungarian(cost [][]int64) []int {
	n := len(cost)
	if n == 0 {
		return nil
	}
	m := len(cost[0])
	const inf = math.MaxInt64 / 4

	// 1-indexed, column 0 is the virtual start of each augmenting path.
	u := make([]int64, n+1)
	v := make([]int64, m+1)
	p := make([]int, m+1)   // p[j]: row matched to column j, 0 when free
	way := make([]int, m+1) // way[j]: previous column on the augmenting path
	minv := make([]int64, m+1)
	used := make([]bool, m+1)

	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		for j := range minv {
			minv[j] = inf
			used[j] = false
		}
		for {
			used[j0] = true
			i0 := p[j0]
			delta := int64(inf)
			j1 := 0
			for j := 1; j <= m; j++ {
				if used[j] {
					continue
				}
				cur := cost[i0-1][j-1] - u[i0] - v[j]
				if cur < minv[j] {
					minv[j] = cur
					way[j] = j0
				}
				if minv[j] < delta {
					delta = minv[j]
					j1 = j
				}
			}
			for j := 0; j <= m; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}
		for j0 != 0 {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
		}
	}

	out := make([]int, n)
	for j := 1; j <= m; j++ {
		if p[j] != 0 {
			out[p[j]-1] = j - 1
		}
	}
	return out
}
