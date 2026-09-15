package missionlog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
)

// Side is one replay of a diff: the pack the plan starts under, and how the
// replay ended.
type Side struct {
	Pack      *doctrine.Pack
	Decisions int
	Commands  int
	State     engine.State
}

// TickDiff is one tick on which the two replays decided or commanded
// differently. OnlyA and OnlyB are the decisions one replay made and the
// other did not, each in its own order.
type TickDiff struct {
	TickMs         int64
	OnlyA          []domain.Decision
	OnlyB          []domain.Decision
	CommandsA      int
	CommandsB      int
	CommandsDiffer bool
}

// DiffResult is how a diff ended.
type DiffResult struct {
	Header Header
	A, B   Side
	Ticks  int64
	// Diverging counts the ticks on which the replays differ, FirstMs is
	// the mission time of the first of them.
	Diverging int64
	FirstMs   int64
}

// Same reports whether the two packs made the same decisions and issued the
// same commands on every tick.
func (r DiffResult) Same() bool { return r.Diverging == 0 }

// Diff replays a mission log twice in lockstep, the plan approved under pack
// a in one replay and under pack b in the other, and calls visit for every
// tick on which they differ (design section 6.3). Hot swaps are replayed as
// recorded: a swap to a third pack happens in both at its tick, each carrying
// its own windows across.
//
// The substituted plan keeps its recorded hash: the diff compares doctrine,
// not plans, and both replays then name the plan the operator approved.
// Decisions are compared modulo the starting pack's identity, where the
// approval and a swap leaving it name it (identity), so an approval naming
// its pack is not a difference.
//
// The replay is open loop. The recorded telemetry answers the commands the
// recording issued, so after the first divergence the replays show what each
// pack decides on the same inputs, not what the mission would have done.
func Diff(src io.Reader, packs *doctrine.Registry, a, b *doctrine.Pack, visit func(TickDiff)) (DiffResult, error) {
	if a == nil || b == nil {
		return DiffResult{}, errors.New("missionlog: a diff needs two packs")
	}
	rd, err := NewReader(src)
	if err != nil {
		return DiffResult{}, err
	}
	h := rd.Header()
	res := DiffResult{Header: h, A: Side{Pack: a}, B: Side{Pack: b}}
	sa := engine.NewState(h.Seed, h.Config, packs)
	sb := engine.NewState(h.Seed, h.Config, packs)
	same := identity(a, b)
	substituted := false

	for {
		t, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, err
		}
		ba, oka := substitute(t.Batch, a)
		bb, _ := substitute(t.Batch, b)
		substituted = substituted || oka

		var ca, cb []domain.Command
		var da, db []domain.Decision
		sa, ca, da = engine.Step(sa, ba)
		sb, cb, db = engine.Step(sb, bb)
		res.Ticks++
		res.A.Decisions += len(da)
		res.A.Commands += len(ca)
		res.B.Decisions += len(db)
		res.B.Commands += len(cb)
		res.A.State, res.B.State = sa, sb

		d, err := compareTick(sa.Clock.TickMs, da, db, ca, cb, same)
		if err != nil {
			return res, err
		}
		if d == nil {
			continue
		}
		if res.Diverging == 0 {
			res.FirstMs = d.TickMs
		}
		res.Diverging++
		if visit != nil {
			visit(*d)
		}
	}
	if !substituted {
		return res, fmt.Errorf("%w: the log approves no plan, so no pack is substituted", ErrLayout)
	}
	return res, nil
}

// substitute returns the batch with every approved plan starting under p,
// pinned to its hash, and whether it held one. The plans are copies: the
// recorded batch is shared by both replays.
func substitute(batch []domain.Event, p *doctrine.Pack) ([]domain.Event, bool) {
	out := make([]domain.Event, len(batch))
	copy(out, batch)
	found := false
	for i, ev := range out {
		if ev.Kind != domain.EventPlanApproved || ev.Plan == nil {
			continue
		}
		plan := ev.Plan.Clone()
		plan.Doctrine, plan.DoctrineHash = p.Ref, p.Hash
		out[i].Plan = &plan
		found = true
	}
	return out, found
}

// identity reads a decision of the replay under b as if b were a, in the two
// places a decision names the pack a plan starts under: the approval, and a
// hot swap leaving it. Only there: a recorded swap's target is the same pack
// in both replays, even when it is b itself, and stays as it is.
func identity(a, b *doctrine.Pack) func(domain.Decision) domain.Decision {
	la, lb := engine.PackLabel(a.Ref, a.Hash), engine.PackLabel(b.Ref, b.Hash)
	return func(d domain.Decision) domain.Decision {
		switch {
		case d.Kind == domain.DecisionMissionState:
			d.Rationale = strings.ReplaceAll(d.Rationale, lb, la)
		case d.Kind == domain.DecisionDoctrineSwap && d.Swap != nil && d.Swap.From == b.Ref && d.Swap.FromHash == b.Hash:
			sw := *d.Swap
			sw.From, sw.FromHash = a.Ref, a.Hash
			d.Swap = &sw
			// The pack swapped from is named first.
			d.Rationale = strings.Replace(d.Rationale, lb, la, 1)
		}
		return d
	}
}

// compareTick pairs the decisions of one tick by canonical encoding, b's read
// through same, and compares the commands as issued. It returns nil when the
// tick is the same under both packs.
func compareTick(tickMs int64, da, db []domain.Decision, ca, cb []domain.Command, same func(domain.Decision) domain.Decision) (*TickDiff, error) {
	ea, err := encodeAll(da, func(d domain.Decision) domain.Decision { return d })
	if err != nil {
		return nil, err
	}
	eb, err := encodeAll(db, same)
	if err != nil {
		return nil, err
	}
	matched := make([]bool, len(eb))
	var onlyA []domain.Decision
	for i, x := range ea {
		j := -1
		for k, y := range eb {
			if !matched[k] && bytes.Equal(x, y) {
				j = k
				break
			}
		}
		if j < 0 {
			onlyA = append(onlyA, da[i])
			continue
		}
		matched[j] = true
	}
	var onlyB []domain.Decision
	for k, m := range matched {
		if !m {
			onlyB = append(onlyB, db[k])
		}
	}

	xa, err := eventlog.Canonical(ca)
	if err != nil {
		return nil, err
	}
	xb, err := eventlog.Canonical(cb)
	if err != nil {
		return nil, err
	}
	differ := !bytes.Equal(xa, xb)
	if len(onlyA) == 0 && len(onlyB) == 0 && !differ {
		return nil, nil
	}
	return &TickDiff{TickMs: tickMs, OnlyA: onlyA, OnlyB: onlyB, CommandsA: len(ca), CommandsB: len(cb), CommandsDiffer: differ}, nil
}

func encodeAll(ds []domain.Decision, f func(domain.Decision) domain.Decision) ([][]byte, error) {
	out := make([][]byte, len(ds))
	for i, d := range ds {
		b, err := eventlog.Canonical(f(d))
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}
