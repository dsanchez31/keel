package eventlog

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Digest is a SHA-256 digest. Rendered as lowercase hex everywhere it is
// shown, stored as bytes everywhere it is chained.
type Digest [32]byte

// ZeroDigest is the PrevHash of the first record in a chain.
var ZeroDigest Digest

// String renders the digest as lowercase hex.
func (d Digest) String() string { return hex.EncodeToString(d[:]) }

// Short renders the first 12 hex characters, enough to recognise a chain head
// in a log line or a UI badge without filling the line.
func (d Digest) Short() string { return d.String()[:12] }

// IsZero reports whether the digest is the zero value.
func (d Digest) IsZero() bool { return d == ZeroDigest }

// ParseDigest reads a lowercase hex digest.
func ParseDigest(s string) (Digest, error) {
	var d Digest
	b, err := hex.DecodeString(s)
	if err != nil {
		return d, fmt.Errorf("eventlog: bad digest %q: %w", s, err)
	}
	if len(b) != len(d) {
		return d, fmt.Errorf("eventlog: digest %q is %d bytes, want %d", s, len(b), len(d))
	}
	copy(d[:], b)
	return d, nil
}

// RecordKind classifies a log record.
type RecordKind string

const (
	// RecordEvent is one event as the engine received it. Recording the
	// inputs rather than only the outputs is what makes replay possible:
	// Step is re-run against these, unchanged.
	RecordEvent RecordKind = "event"
	// RecordDecision is one orchestration choice with its full rationale.
	RecordDecision RecordKind = "decision"
	// RecordCommand is one instruction dispatched to a vector.
	RecordCommand RecordKind = "command"
	// RecordTick marks a tick boundary and carries the coverage summary, so
	// a reader can locate a point in mission time without replaying.
	RecordTick RecordKind = "tick"
	// RecordHeader is the first record of a mission log: what a replay needs
	// to rebuild the engine's initial state (missionlog.Header).
	RecordHeader RecordKind = "header"
)

// Record is one link of the hash chain.
//
// Hash = SHA256(canonical(Seq, TickMs, Kind, Payload, PrevHash)). The first
// record uses a zero PrevHash. Altering any record invalidates every hash
// after it, which is what makes the chain tamper evident and what lets two
// runs be compared by one value instead of by a diff.
type Record struct {
	Seq      uint64     `json:"seq"`
	TickMs   int64      `json:"tick_ms"`
	Kind     RecordKind `json:"kind"`
	Payload  []byte     `json:"payload"`
	PrevHash Digest     `json:"prev_hash"`
	Hash     Digest     `json:"hash"`
}

// preimage is exactly the five fields that are hashed, and nothing else.
//
// Hash is deliberately absent: a record cannot commit to its own digest. The
// struct exists so the preimage is a named, reviewable thing rather than a
// concatenation order buried in a function.
type preimage struct {
	Seq      uint64     `json:"seq"`
	TickMs   int64      `json:"tick_ms"`
	Kind     RecordKind `json:"kind"`
	Payload  []byte     `json:"payload"`
	PrevHash Digest     `json:"prev_hash"`
}

// ComputeHash returns the digest a record must carry given its contents.
func ComputeHash(seq uint64, tickMs int64, kind RecordKind, payload []byte, prev Digest) (Digest, error) {
	b, err := Canonical(preimage{
		Seq:      seq,
		TickMs:   tickMs,
		Kind:     kind,
		Payload:  payload,
		PrevHash: prev,
	})
	if err != nil {
		return ZeroDigest, err
	}
	return sha256.Sum256(b), nil
}

// Chain fills PrevHash and Hash on a record, given the previous head.
func Chain(r Record, prev Digest) (Record, error) {
	r.PrevHash = prev
	h, err := ComputeHash(r.Seq, r.TickMs, r.Kind, r.Payload, prev)
	if err != nil {
		return r, err
	}
	r.Hash = h
	return r, nil
}

// VerifyRecord recomputes a record's digest and checks it against the one it
// carries and against the expected previous head.
func VerifyRecord(r Record, prev Digest) error {
	if r.PrevHash != prev {
		return &ChainError{Seq: r.Seq, Want: prev, Got: r.PrevHash, Field: "prev_hash"}
	}
	h, err := ComputeHash(r.Seq, r.TickMs, r.Kind, r.Payload, r.PrevHash)
	if err != nil {
		return err
	}
	if h != r.Hash {
		return &ChainError{Seq: r.Seq, Want: h, Got: r.Hash, Field: "hash"}
	}
	return nil
}

// ChainError reports a break in the hash chain, which is invariant I6.
type ChainError struct {
	Seq   uint64
	Field string
	Want  Digest
	Got   Digest
}

func (e *ChainError) Error() string {
	return fmt.Sprintf("eventlog: chain broken at seq %d: %s is %s, want %s",
		e.Seq, e.Field, e.Got.Short(), e.Want.Short())
}

// HashOf is the convenience form used wherever a value needs a stable
// content address: plan hashes, doctrine pack hashes, golden fixtures.
func HashOf(v any) (Digest, error) {
	b, err := Canonical(v)
	if err != nil {
		return ZeroDigest, err
	}
	return sha256.Sum256(b), nil
}

// MustHashOf is HashOf for call sites where failure is a programming error.
func MustHashOf(v any) Digest {
	d, err := HashOf(v)
	if err != nil {
		panic("eventlog: hashing failed: " + err.Error())
	}
	return d
}

// HexOf is HashOf rendered as lowercase hex, the form PlanHash and the
// doctrine pack hash take.
func HexOf(v any) (string, error) {
	d, err := HashOf(v)
	if err != nil {
		return "", err
	}
	return d.String(), nil
}
