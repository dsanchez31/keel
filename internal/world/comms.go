package world

import (
	"math/rand/v2"
	"slices"

	"github.com/dsanchez31/keel/internal/domain"
)

// Comms model.
//
// A vehicle has a radio link when it is within range of a station and
// outside every blackout zone, and has not had its link cut by a fault. The
// link carries both ways: telemetry down, commands up, each message
// independently lost with the scenario's probability and delayed by a whole
// number of ticks of jitter. A message delayed past a later one arrives out
// of order; the engine drops a stale frame, and a vehicle ignores a command
// whose Seq it has already passed.
//
// A message is judged when it is sent: a frame sent from inside a blackout
// zone is never received, one sent just before a vehicle entered it still
// arrives.

type comms struct {
	cfg       Comms
	stations  []domain.Station
	blackouts []Blackout
}

// reach reports whether a vehicle at p has a link, before faults.
func (c comms) reach(p domain.Position) bool {
	for _, b := range c.blackouts {
		if b.Contains(p) {
			return false
		}
	}
	if c.cfg.RangeM <= 0 || len(c.stations) == 0 {
		return true
	}
	for _, s := range c.stations {
		if domain.HaversineM(p, s.Position) <= c.cfg.RangeM {
			return true
		}
	}
	return false
}

// transit draws the fate of one message: lost, or delayed by some ticks. It
// always draws twice, so the stream advances by the same amount whatever
// the outcome and a lost message cannot shift the draws of the next one.
func (c comms) transit(rng *rand.Rand) (lost bool, delayTicks int64) {
	lossDraw := rng.Float64()
	jitterDraw := rng.Int64N(c.cfg.JitterMs/domain.TickIntervalMs + 1)
	return lossDraw*100 < c.cfg.LossPct, jitterDraw
}

// inflight is one message on the way, due at a tick.
type inflight[T any] struct {
	due   int64
	order uint64 // send order, the tiebreak among messages due together
	item  T
}

// channel is the messages in transit one way.
type channel[T any] struct {
	q    []inflight[T]
	sent uint64
}

func (c *channel[T]) push(due int64, item T) {
	c.sent++
	c.q = append(c.q, inflight[T]{due: due, order: c.sent, item: item})
}

// pop removes and returns every message due at or before tick, in due order
// then send order.
func (c *channel[T]) pop(tick int64) []T {
	var due, later []inflight[T]
	for _, m := range c.q {
		if m.due <= tick {
			due = append(due, m)
		} else {
			later = append(later, m)
		}
	}
	c.q = later
	slices.SortStableFunc(due, func(a, b inflight[T]) int {
		if a.due != b.due {
			if a.due < b.due {
				return -1
			}
			return 1
		}
		if a.order < b.order {
			return -1
		}
		return 1
	})
	out := make([]T, len(due))
	for i, m := range due {
		out[i] = m.item
	}
	return out
}
