package transport

import (
	"fmt"
	"io"

	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/missionlog"
)

// Replay windows (GET /api/v1/replays/{id}/frames, spec section 8.1).
//
// A mission log holds events, decisions, commands and tick counts: not the
// fog, not the lanes a redecomposition cut. Drawing a replay at a mission
// time needs Step run over the log, so the daemon runs it, through
// missionlog.Replay, the live code path fed recorded input, and projects
// each tick with TickMessages, the projection the live stream uses. A client
// folds a window as it folds the stream, into the same view.

const (
	// ReplaySampleTicks thins a window's telemetry and tick frames to one
	// tick in ten, 1 Hz of mission time. Every other frame is kept: an event,
	// a decision or a coverage delta dropped would be a state that never was,
	// while a vector drawn from one position a second is still flying,
	// interpolated between its samples. The window's last tick is kept whole,
	// so a window ends on a tick boundary.
	ReplaySampleTicks = 10
	// MaxReplayWindowMs bounds one window to 30 minutes of mission.
	MaxReplayWindowMs = 30 * 60_000
)

// ReplayWindow is the answer to GET /api/v1/replays/{id}/frames: a window of
// the replayed mission as stream frames, and what verifying it found.
type ReplayWindow struct {
	FromMs int64 `json:"from_ms"`
	ToMs   int64 `json:"to_ms"`
	// Frames starts with the mission view after the last tick before FromMs,
	// seq 1, then every tick from FromMs to ToMs as the stream would send
	// it, numbered on without a gap, telemetry and tick frames thinned.
	Frames []EncodedFrame `json:"frames"`
	// VerifiedMs is the mission time of the last tick the replay rebuilt to
	// the recorded digests, every record before it included (I7 so far).
	VerifiedMs int64 `json:"verified_ms"`
	// Divergence is the first rebuilt record that differs from the recorded
	// one; the window stops at its tick.
	Divergence *ReplayDivergence `json:"divergence,omitempty"`
	// End is set when the replay read the recording to its end.
	End *ReplayEnd `json:"end,omitempty"`
}

// ReplayDivergence locates the first record a replay did not rebuild. A
// digest is absent when the replay rebuilt more records for the tick than
// were recorded, or fewer.
type ReplayDivergence struct {
	Seq      uint64 `json:"seq"`
	TickMs   int64  `json:"tick_ms"`
	Recorded string `json:"recorded,omitempty"`
	Replayed string `json:"replayed,omitempty"`
}

// ReplayEnd is how the recording ends: its last tick and both heads, equal
// when the replay reproduced it.
type ReplayEnd struct {
	TickMs   int64  `json:"tick_ms"`
	Recorded string `json:"recorded"`
	Replayed string `json:"replayed"`
}

// CheckReplayWindow reports what is wrong with a window's bounds, if
// anything.
func CheckReplayWindow(fromMs, toMs int64) error {
	switch {
	case fromMs < 0:
		return fmt.Errorf("from %d is negative", fromMs)
	case toMs < fromMs:
		return fmt.Errorf("to %d is before from %d", toMs, fromMs)
	case toMs-fromMs > MaxReplayWindowMs:
		return fmt.Errorf("the window spans %d ms, at most %d", toMs-fromMs, MaxReplayWindowMs)
	}
	return nil
}

// BuildReplayWindow replays a recording, verifying it, up to the first tick
// at or past toMs, and projects the ticks from fromMs on.
//
// A broken chain or a recording that is not a mission log is an error. A
// replay that diverges is not: the window stops at the divergent tick and
// says where. A window past the recording's end holds its final view.
func BuildReplayWindow(src io.Reader, packs *doctrine.Registry, fromMs, toMs int64) (ReplayWindow, error) {
	w := ReplayWindow{FromMs: fromMs, ToMs: toMs}
	var (
		seq      uint64
		snapped  bool
		lastMs   int64
		beforeMs int64
		encErr   error
		// tail holds the thinned frames of the last tick seen, sent after the
		// replay when that tick was not a sampled one.
		tail   []Message
		tailMs int64
	)
	emit := func(t int64, m Message) {
		if encErr != nil {
			return
		}
		b, err := eventlog.Canonical(m.Data)
		if err != nil {
			encErr = fmt.Errorf("transport: encoding a %s frame at %d ms: %w", m.Type, t, err)
			return
		}
		seq++
		w.Frames = append(w.Frames, EncodedFrame{Data: b, Seq: seq, T: t, Type: m.Type})
	}

	res, err := missionlog.Replay(src, missionlog.Options{
		Packs:   packs,
		Verify:  true,
		UntilMs: max(toMs, 1),
		OnTick: func(rt missionlog.ReplayedTick) {
			beforeMs, lastMs = lastMs, rt.TickMs
			if rt.TickMs < fromMs {
				return
			}
			if !snapped {
				emit(rt.Prev.Clock.TickMs, Message{Type: FrameMission, Data: NewMissionView(rt.Prev, rt.PrevHead)})
				snapped = true
			}
			sampled := Sampled(rt.State.Clock.Tick, ReplaySampleTicks) || rt.TickMs >= toMs
			var kept []Message
			kept, tail = ThinTick(TickMessages(rt.Prev, rt.State, rt.Batch, rt.Decisions, rt.Head), sampled)
			for _, m := range kept {
				emit(rt.TickMs, m)
			}
			tailMs = rt.TickMs
		},
	})
	if err != nil {
		return ReplayWindow{}, err
	}
	for _, m := range tail {
		emit(tailMs, m)
	}
	if !snapped {
		emit(res.State.Clock.TickMs, Message{Type: FrameMission, Data: NewMissionView(res.State, res.Replayed)})
	}
	if encErr != nil {
		return ReplayWindow{}, encErr
	}

	w.VerifiedMs = lastMs
	if d := res.Divergence; d != nil {
		w.VerifiedMs = beforeMs
		w.Divergence = &ReplayDivergence{Seq: d.Seq, TickMs: d.TickMs}
		if d.Recorded != nil {
			w.Divergence.Recorded = d.Recorded.Hash.String()
		}
		if d.Replayed != nil {
			w.Divergence.Replayed = d.Replayed.Hash.String()
		}
	}
	if res.Complete {
		w.End = &ReplayEnd{TickMs: lastMs, Recorded: res.Recorded.String(), Replayed: res.Replayed.String()}
	}
	return w, nil
}
