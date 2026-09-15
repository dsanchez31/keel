package doctrine

import (
	"errors"
	"slices"

	"github.com/dsanchez31/keel/internal/domain"
)

// ErrInvalidSwap reports a hot swap with a missing pack.
var ErrInvalidSwap = errors.New("doctrine: invalid hot swap")

// Swap records one hot swap, carrying both pack hashes so a replay reproduces
// the swap exactly. It is the payload of the doctrine_swap decision, so the
// definition lives in internal/domain.
type Swap = domain.DoctrineSwap

// HotSwap replaces pack from with pack to, carrying the duration windows
// across (spec.md section 6.6). The caller applies it at a tick boundary,
// between two evaluations, never inside one.
//
// A window is keyed by (rule id, agent) and is retained, with its start time
// and its fired flag, when the new pack declares a rule with the same id. A
// condition that has held for 3 s therefore does not restart its clock, and a
// swap that changes only a rule's actions fires at exactly the tick it would
// have fired at without the swap. A window whose rule the new pack does not
// declare is discarded.
//
// Retention looks at the rule id only, not at the condition: a rule whose
// condition changed keeps the start time observed under the old condition.
// Tightening "battery_pct < 30 for 10s" into "battery_pct < 20 for 10s" under
// the same id can therefore fire as soon as the new condition holds, without
// it having held for 10 s. This follows spec.md section 6.6 as written, by
// explicit decision. The durable alternative is to retain a window only when
// the condition's source is unchanged and discard it otherwise; it would be
// triggered by a pack revision that narrows a windowed condition in place.
// Until then, a pack author narrowing a condition gives the rule a new id.
//
// Only window state crosses a swap. The mission, its plan, its lane
// assignments and its coverage are not doctrine state and this function never
// sees them, which is how a swap keeps invariant I9.
func HotSwap(from, to *Pack, windows []Window) ([]Window, Swap, error) {
	if from == nil || to == nil {
		return nil, Swap{}, ErrInvalidSwap
	}
	sw := Swap{From: from.Ref, FromHash: from.Hash, To: to.Ref, ToHash: to.Hash}
	sorted := slices.Clone(windows)
	slices.SortFunc(sorted, compareWindows)
	for _, w := range sorted {
		if _, ok := to.Rule(w.RuleID); ok {
			sw.Retained = append(sw.Retained, w)
		} else {
			sw.Discarded = append(sw.Discarded, w)
		}
	}
	return slices.Clone(sw.Retained), sw, nil
}
