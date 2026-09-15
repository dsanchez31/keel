package missionlog

import (
	"errors"
	"fmt"
	"io"

	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/strictjson"
)

var (
	// ErrBroken reports a recording whose hash chain does not hold (I6): a
	// record that does not link to the previous one, or whose digest is not
	// what its content implies. Nothing read from it can be trusted.
	ErrBroken = errors.New("missionlog: the recorded chain is broken")
	// ErrLayout reports a recording that is not a mission log: no header, a
	// record kind the layout does not write, a payload that does not decode,
	// a tick left unfinished.
	ErrLayout = errors.New("missionlog: not a mission log")
)

// Tick is one recorded tick: the batch engine.Step was given, and every
// record of the tick as recorded, from its events to its summary.
type Tick struct {
	TickMs  int64
	Batch   []domain.Event
	Records []eventlog.Record
}

// Reader reads a mission log one tick at a time, checking the chain as it
// goes. It streams, so a long mission is never resident.
type Reader struct {
	sc     *eventlog.Scanner
	head   eventlog.Digest
	seq    uint64
	header Header
	first  eventlog.Record
	// peeked is the record AtEnd read ahead, not linked onto the chain yet.
	peeked *eventlog.Record
}

// NewReader reads the header record, the first of every mission log. A
// header recorded under another tick interval is refused: mission time would
// not mean what the engine takes it to mean.
func NewReader(src io.Reader) (*Reader, error) {
	r := &Reader{sc: eventlog.NewScanner(src)}
	rec, ok, err := r.scan()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: no record", ErrLayout)
	}
	if rec.Kind != eventlog.RecordHeader || rec.TickMs != 0 {
		return nil, fmt.Errorf("%w: the first record is a %s at %d ms, want the header at 0 ms", ErrLayout, rec.Kind, rec.TickMs)
	}
	h, err := strictjson.Decode[Header](rec.Payload, "header")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLayout, err)
	}
	if h.TickIntervalMs != domain.TickIntervalMs {
		return nil, fmt.Errorf("%w: recorded with a tick of %d ms, this engine ticks every %d ms", ErrLayout, h.TickIntervalMs, domain.TickIntervalMs)
	}
	r.header, r.first = h, rec
	return r, nil
}

// Header is the mission's header record, decoded.
func (r *Reader) Header() Header { return r.header }

// Head is the digest of the last record read.
func (r *Reader) Head() eventlog.Digest { return r.head }

// Seq is the sequence number of the last record read.
func (r *Reader) Seq() uint64 { return r.seq }

// AtEnd reports whether the log holds no record after the last one read. It
// reads the next record ahead when there is one, and links it onto the chain
// only when Next takes it: Head and Seq stay those of the records read.
func (r *Reader) AtEnd() (bool, error) {
	if r.peeked != nil {
		return false, nil
	}
	if !r.sc.Scan() {
		if err := r.sc.Err(); err != nil {
			return false, fmt.Errorf("%w: record %d: %w", ErrLayout, r.seq+1, err)
		}
		return true, nil
	}
	rec := r.sc.Record()
	r.peeked = &rec
	return false, nil
}

// Next reads the records of one tick, up to and including its summary, and
// decodes its events. It returns io.EOF after the last tick. A log ending
// inside a tick, as a crash mid-write leaves it, is ErrLayout: the part
// recorded cannot be told apart from a tick that was recorded short.
func (r *Reader) Next() (Tick, error) {
	var t Tick
	for {
		rec, ok, err := r.scan()
		if err != nil {
			return Tick{}, err
		}
		if !ok {
			if len(t.Records) > 0 {
				return Tick{}, fmt.Errorf("%w: the log ends inside the tick at %d ms, %d record(s) after the last tick record", ErrLayout, t.TickMs, len(t.Records))
			}
			return Tick{}, io.EOF
		}
		t.TickMs = rec.TickMs
		t.Records = append(t.Records, rec)
		switch rec.Kind {
		case eventlog.RecordEvent:
			ev, err := strictjson.Decode[domain.Event](rec.Payload, "event")
			if err != nil {
				return Tick{}, fmt.Errorf("%w: record %d: %w", ErrLayout, rec.Seq, err)
			}
			t.Batch = append(t.Batch, ev)
		case eventlog.RecordDecision, eventlog.RecordCommand:
			// Outputs: a replay recomputes them and compares them by digest.
		case eventlog.RecordTick:
			return t, nil
		default:
			return Tick{}, fmt.Errorf("%w: record %d is a %s, which a mission log does not hold", ErrLayout, rec.Seq, rec.Kind)
		}
	}
}

// scan reads one record and links it onto the chain read so far.
func (r *Reader) scan() (eventlog.Record, bool, error) {
	var rec eventlog.Record
	switch {
	case r.peeked != nil:
		rec, r.peeked = *r.peeked, nil
	case r.sc.Scan():
		rec = r.sc.Record()
	default:
		if err := r.sc.Err(); err != nil {
			return eventlog.Record{}, false, fmt.Errorf("%w: record %d: %w", ErrLayout, r.seq+1, err)
		}
		return eventlog.Record{}, false, nil
	}
	if rec.Seq != r.seq+1 {
		return eventlog.Record{}, false, fmt.Errorf("%w: record %d carries seq %d", ErrBroken, r.seq+1, rec.Seq)
	}
	if err := eventlog.VerifyRecord(rec, r.head); err != nil {
		return eventlog.Record{}, false, fmt.Errorf("%w: %w", ErrBroken, err)
	}
	r.head, r.seq = rec.Hash, rec.Seq
	return rec, true, nil
}

// Options configures a replay.
type Options struct {
	// Packs is the registry the recorded plan and hot swaps resolve against.
	// Both are pinned by hash (spec section 4.5), so a pack edited since the
	// recording makes the replay diverge at a refusal naming it.
	Packs *doctrine.Registry
	// Verify compares every record the replay rebuilds with the recorded one
	// and stops at the first that differs.
	Verify bool
	// UntilMs, when positive, stops the replay after the first tick at or
	// past that mission time: a window of the mission rather than all of it.
	// A window whose stop is the recording's last tick reads it to its end.
	UntilMs int64
	// OnTick, if set, is called after every replayed tick, before the tick
	// is compared with the recording.
	OnTick func(ReplayedTick)
}

// ReplayedTick is one tick a replay ran: what Step was given and returned,
// the state before it, and the head of the rebuilt chain before and after
// the tick's records. Enough to project the tick as the live loop does
// (transport.TickMessages).
type ReplayedTick struct {
	engine.TickResult
	TickMs   int64
	Prev     engine.State
	Batch    []domain.Event
	PrevHead eventlog.Digest
	Head     eventlog.Digest
}

// Result is how a replay ended.
type Result struct {
	Header    Header
	Ticks     int64
	Decisions int
	Commands  int
	// State is the engine's state after the last tick replayed.
	State engine.State
	// Records is how many records were read from the recording, every one
	// linked onto the chain.
	Records uint64
	// Recorded is the head of the recording as far as it was read, Replayed
	// the head of the chain the replay rebuilt from it.
	Recorded eventlog.Digest
	Replayed eventlog.Digest
	// Divergence is the first rebuilt record that differs from the recorded
	// one. Only a verifying replay looks for it, and stops there.
	Divergence *Divergence
	// Complete reports that the recording was read to its end: no
	// divergence stopped the replay, and UntilMs did not come before the
	// last tick.
	Complete bool
}

// Match reports whether the replay rebuilt the recording: no divergence and
// one head, which is invariant I7.
func (r Result) Match() bool { return r.Divergence == nil && r.Recorded == r.Replayed }

// Divergence is the first record at which the rebuilt chain leaves the
// recorded one. Recorded is nil when the replay rebuilt more records for the
// tick than were recorded, Replayed when it rebuilt fewer.
type Divergence struct {
	Seq      uint64
	TickMs   int64
	Recorded *eventlog.Record
	Replayed *eventlog.Record
}

// Replay feeds engine.Step, from the state the header describes, the batches
// the recording holds, and rebuilds the chain the live loop would have
// written through the same AppendHeader and AppendTick. It is the live code
// path fed recorded input, not a second implementation of it (design section
// 3.1).
//
// A broken chain or a recording that is not a mission log is an error. A
// replay that does not reproduce the recording is not: Result says where it
// left it.
func Replay(src io.Reader, opts Options) (Result, error) {
	rd, err := NewReader(src)
	if err != nil {
		return Result{}, err
	}
	res := Result{Header: rd.Header()}
	tp := &tape{}
	finish := func() Result {
		res.Records, res.Recorded, res.Replayed = rd.Seq(), rd.Head(), tp.head
		return res
	}

	if err := AppendHeader(tp, rd.Header()); err != nil {
		return finish(), err
	}
	if opts.Verify {
		if res.Divergence = tp.compare([]eventlog.Record{rd.first}); res.Divergence != nil {
			return finish(), nil
		}
	}
	tp.recs = nil

	s := engine.NewState(res.Header.Seed, res.Header.Config, opts.Packs)
	for {
		t, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			res.State = s
			return finish(), err
		}
		prev, prevHead := s, tp.head
		next, cmds, decs := engine.Step(s, t.Batch)
		s = next
		res.State = s
		if err := AppendTick(tp, s, t.Batch, decs, cmds); err != nil {
			return finish(), err
		}
		res.Ticks++
		res.Decisions += len(decs)
		res.Commands += len(cmds)
		if opts.OnTick != nil {
			opts.OnTick(ReplayedTick{
				TickResult: engine.TickResult{State: s, Commands: cmds, Decisions: decs},
				TickMs:     t.TickMs,
				Prev:       prev,
				Batch:      t.Batch,
				PrevHead:   prevHead,
				Head:       tp.head,
			})
		}
		if opts.Verify {
			if res.Divergence = tp.compare(t.Records); res.Divergence != nil {
				return finish(), nil
			}
		}
		tp.recs = tp.recs[:0]
		if opts.UntilMs > 0 && t.TickMs >= opts.UntilMs {
			// Stopped on the last tick, the replay has read the recording
			// to its end, and says so: a client asking for the window that
			// closes on the mission's last tick is told how it ends.
			end, err := rd.AtEnd()
			if err != nil {
				return finish(), err
			}
			if !end {
				return finish(), nil
			}
		}
	}
	res.Complete = true
	return finish(), nil
}

// tape is the Appender a replay rebuilds the chain into. It chains records
// as a log does and holds only those of the tick being compared.
type tape struct {
	head eventlog.Digest
	seq  uint64
	recs []eventlog.Record
}

func (t *tape) Append(kind eventlog.RecordKind, tickMs int64, payload any) (eventlog.Record, error) {
	body, err := eventlog.Canonical(payload)
	if err != nil {
		return eventlog.Record{}, err
	}
	rec, err := eventlog.Chain(eventlog.Record{Seq: t.seq + 1, TickMs: tickMs, Kind: kind, Payload: body}, t.head)
	if err != nil {
		return eventlog.Record{}, err
	}
	t.recs = append(t.recs, rec)
	t.head, t.seq = rec.Hash, rec.Seq
	return rec, nil
}

func (t *tape) Head() eventlog.Digest { return t.head }

func (t *tape) Seq() uint64 { return t.seq }

// compare pairs the rebuilt records of one tick with the recorded ones. Both
// chains start from the same head at the tick, so one digest compares
// sequence number, mission time, kind and payload at once.
func (t *tape) compare(recorded []eventlog.Record) *Divergence {
	for i := range max(len(recorded), len(t.recs)) {
		var got, want *eventlog.Record
		if i < len(t.recs) {
			got = &t.recs[i]
		}
		if i < len(recorded) {
			want = &recorded[i]
		}
		if got != nil && want != nil && got.Hash == want.Hash {
			continue
		}
		d := &Divergence{Recorded: want, Replayed: got}
		if want != nil {
			d.Seq, d.TickMs = want.Seq, want.TickMs
		} else {
			d.Seq, d.TickMs = got.Seq, got.TickMs
		}
		return d
	}
	return nil
}
