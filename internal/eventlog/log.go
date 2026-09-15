package eventlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// The log is the source of truth. There is no database: a relational store
// would be a projection of this file, and a projection is rebuildable while
// the log is not (design.md section 9.3).
//
// It is append only and segmented. Segments exist so that a long mission does
// not become one unbounded file, and because rolling at a boundary lets a
// reader stream a mission without holding it in memory. They are an artifact
// of storage: the chain runs across segment boundaries unbroken, and Seq is
// global rather than per segment.

// SegmentExt is the extension of a log segment file.
const SegmentExt = ".keellog"

// DefaultMaxSegmentBytes rolls a segment at 8 MiB.
const DefaultMaxSegmentBytes int64 = 8 << 20

// Appender is the write side of the log, as the daemon sees it.
//
// It is an interface so the DST harness and the unit tests can substitute
// MemLog and keep the disk out of a deterministic run.
type Appender interface {
	Append(kind RecordKind, tickMs int64, payload any) (Record, error)
	Head() Digest
	Seq() uint64
}

// Options configures a Log.
type Options struct {
	// MaxSegmentBytes rolls a segment once it exceeds this size. Zero means
	// DefaultMaxSegmentBytes.
	MaxSegmentBytes int64
	// SyncEveryRecord fsyncs after every append. Correct and slow. Off by
	// default: a crash costs the tail of one mission recording, and the
	// alternative costs a disk flush every 100 ms.
	SyncEveryRecord bool
}

// Log is an append-only segmented log with a SHA-256 hash chain.
//
// It is safe for concurrent use. The lock is here rather than at the call
// site because the daemon appends from the tick loop while the transport
// layer reads the head for the live UI badge.
type Log struct {
	mu       sync.Mutex
	dir      string
	opts     Options
	seq      uint64
	head     Digest
	segIndex int
	segBytes int64
	file     *os.File
	w        *bufio.Writer
}

// Open opens or creates a log in dir.
//
// An existing log is scanned end to end and its chain verified before a single
// byte is appended. Appending to a log whose chain is already broken would
// produce a recording that looks valid from the join point onward, which is
// worse than refusing to open it.
func Open(dir string, opts Options) (*Log, error) {
	if opts.MaxSegmentBytes <= 0 {
		opts.MaxSegmentBytes = DefaultMaxSegmentBytes
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("eventlog: create %s: %w", dir, err)
	}

	l := &Log{dir: dir, opts: opts}

	segs, err := segmentFiles(dir)
	if err != nil {
		return nil, err
	}
	for _, s := range segs {
		if err := l.scanSegment(s); err != nil {
			return nil, err
		}
	}
	if n := len(segs); n > 0 {
		l.segIndex = segmentIndexOf(segs[n-1])
		info, err := os.Stat(segs[n-1])
		if err != nil {
			return nil, err
		}
		l.segBytes = info.Size()
	}

	if err := l.openSegment(); err != nil {
		return nil, err
	}
	return l, nil
}

// Dir is the directory holding the segments.
func (l *Log) Dir() string { return l.dir }

// Head is the digest of the last appended record, or the zero digest for an
// empty log. This is the value rendered live in the UI and compared after a
// replay.
func (l *Log) Head() Digest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.head
}

// Seq is the sequence number of the last appended record.
func (l *Log) Seq() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// Append canonically encodes payload, chains it onto the head and writes it.
func (l *Log) Append(kind RecordKind, tickMs int64, payload any) (Record, error) {
	body, err := Canonical(payload)
	if err != nil {
		return Record{}, fmt.Errorf("eventlog: encode %s payload: %w", kind, err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	rec := Record{Seq: l.seq + 1, TickMs: tickMs, Kind: kind, Payload: body}
	rec, err = Chain(rec, l.head)
	if err != nil {
		return Record{}, err
	}

	line, err := encodeLine(rec)
	if err != nil {
		return Record{}, err
	}
	if l.segBytes > 0 && l.segBytes+int64(len(line)) > l.opts.MaxSegmentBytes {
		if err := l.rollLocked(); err != nil {
			return Record{}, err
		}
	}
	n, err := l.w.Write(line)
	if err != nil {
		return Record{}, fmt.Errorf("eventlog: write segment: %w", err)
	}
	l.segBytes += int64(n)
	l.seq = rec.Seq
	l.head = rec.Hash

	if l.opts.SyncEveryRecord {
		if err := l.syncLocked(); err != nil {
			return Record{}, err
		}
	}
	return rec, nil
}

// Sync flushes the buffer and fsyncs the current segment.
func (l *Log) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.syncLocked()
}

func (l *Log) syncLocked() error {
	if l.w != nil {
		if err := l.w.Flush(); err != nil {
			return err
		}
	}
	if l.file != nil {
		return l.file.Sync()
	}
	return nil
}

// Close flushes, syncs and releases the current segment.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.syncLocked()
	if cerr := l.file.Close(); err == nil {
		err = cerr
	}
	l.file = nil
	l.w = nil
	return err
}

func (l *Log) openSegment() error {
	path := segmentPath(l.dir, l.segIndex)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("eventlog: open segment %s: %w", path, err)
	}
	l.file = f
	l.w = bufio.NewWriterSize(f, 64<<10)
	return nil
}

func (l *Log) rollLocked() error {
	if err := l.syncLocked(); err != nil {
		return err
	}
	if err := l.file.Close(); err != nil {
		return err
	}
	l.segIndex++
	l.segBytes = 0
	return l.openSegment()
}

func (l *Log) scanSegment(path string) error {
	return scanFile(path, func(rec Record) error {
		if rec.Seq != l.seq+1 {
			return fmt.Errorf("eventlog: %s: sequence jumps from %d to %d", path, l.seq, rec.Seq)
		}
		if err := VerifyRecord(rec, l.head); err != nil {
			return fmt.Errorf("eventlog: %s: %w", path, err)
		}
		l.seq = rec.Seq
		l.head = rec.Hash
		return nil
	})
}

// scanFile streams every record of one segment file into fn, stopping at the
// first error fn returns.
//
// It is the only place a segment is opened for reading, so the file is closed
// on every path and a failing Close is reported rather than dropped. Nothing
// is lost when a read-only Close fails, but a failure there still means the
// descriptor or the device is in a state the caller should hear about.
func scanFile(path string, fn func(Record) error) (err error) {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("eventlog: open %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("eventlog: close %s: %w", path, cerr))
		}
	}()

	sc := NewScanner(f)
	for sc.Scan() {
		if err := fn(sc.Record()); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("eventlog: read %s: %w", path, err)
	}
	return nil
}

func segmentPath(dir string, index int) string {
	return filepath.Join(dir, fmt.Sprintf("%08d%s", index, SegmentExt))
}

func segmentIndexOf(path string) int {
	base := strings.TrimSuffix(filepath.Base(path), SegmentExt)
	var n int
	if _, err := fmt.Sscanf(base, "%d", &n); err != nil {
		return 0
	}
	return n
}

// segmentFiles lists the segments of a directory in index order. The names are
// zero padded so lexicographic order is index order, and the sort makes that
// explicit rather than relying on the reader to notice.
func segmentFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("eventlog: read %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != SegmentExt {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// line is the on-disk shape of a record: one canonical JSON object per line.
//
// It is not the hash preimage. The preimage is the five fields of hash.go,
// which is why this can be readable (hex digests, the payload spliced in as
// JSON rather than base64) without weakening anything.
type line struct {
	Hash     string  `json:"hash"`
	Kind     string  `json:"kind"`
	Payload  RawJSON `json:"payload"`
	PrevHash string  `json:"prev_hash"`
	Seq      uint64  `json:"seq"`
	TickMs   int64   `json:"tick_ms"`
}

func encodeLine(r Record) ([]byte, error) {
	b, err := Canonical(line{
		Hash:     r.Hash.String(),
		Kind:     string(r.Kind),
		Payload:  RawJSON(r.Payload),
		PrevHash: r.PrevHash.String(),
		Seq:      r.Seq,
		TickMs:   r.TickMs,
	})
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func decodeLine(b []byte) (Record, error) {
	var raw struct {
		Hash     string          `json:"hash"`
		Kind     string          `json:"kind"`
		Payload  json.RawMessage `json:"payload"`
		PrevHash string          `json:"prev_hash"`
		Seq      uint64          `json:"seq"`
		TickMs   int64           `json:"tick_ms"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return Record{}, fmt.Errorf("eventlog: malformed record: %w", err)
	}
	h, err := ParseDigest(raw.Hash)
	if err != nil {
		return Record{}, err
	}
	p, err := ParseDigest(raw.PrevHash)
	if err != nil {
		return Record{}, err
	}
	return Record{
		Seq:      raw.Seq,
		TickMs:   raw.TickMs,
		Kind:     RecordKind(raw.Kind),
		Payload:  []byte(raw.Payload),
		PrevHash: p,
		Hash:     h,
	}, nil
}

// Scanner streams records from a segment.
//
// Streaming rather than slurping, because a replay of a long mission should
// not need the whole recording resident, and because keelctl replay wants to
// report progress as it verifies.
type Scanner struct {
	sc  *bufio.Scanner
	rec Record
	err error
}

// MaxLineBytes bounds one record. A record larger than this is a bug, and
// bufio's default 64 KiB is too small for a plan payload.
const MaxLineBytes = 8 << 20

// NewScanner reads newline-delimited records from r.
func NewScanner(r io.Reader) *Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLineBytes)
	return &Scanner{sc: sc}
}

// Scan advances to the next record. Blank lines are skipped, so a truncated
// tail from a crash is tolerated up to the last complete line.
func (s *Scanner) Scan() bool {
	for s.sc.Scan() {
		b := s.sc.Bytes()
		if len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		rec, err := decodeLine(b)
		if err != nil {
			s.err = err
			return false
		}
		s.rec = rec
		return true
	}
	if err := s.sc.Err(); err != nil {
		s.err = err
	}
	return false
}

// Record is the record most recently returned by Scan.
func (s *Scanner) Record() Record { return s.rec }

// Err reports the first error encountered, if any.
func (s *Scanner) Err() error { return s.err }

// ReadFile reads and verifies every record of a single log file.
func ReadFile(path string) ([]Record, error) {
	var out []Record
	if err := scanFile(path, collect(&out)); err != nil {
		return nil, err
	}
	if _, err := Verify(out); err != nil {
		return nil, err
	}
	return out, nil
}

// ReadDir reads and verifies every segment of a log directory, in order, as
// one continuous chain.
func ReadDir(dir string) ([]Record, error) {
	segs, err := segmentFiles(dir)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, s := range segs {
		if err := scanFile(s, collect(&out)); err != nil {
			return nil, err
		}
	}
	if _, err := Verify(out); err != nil {
		return nil, err
	}
	return out, nil
}

// OpenDir streams the segments of a log directory in order, as the bytes on
// disk: one record per line, the chain unbroken across segment boundaries.
// It verifies nothing, so a reader can check the chain from exactly what it
// received. A directory holding no segment is os.ErrNotExist. The caller
// closes the reader, which closes every segment.
func OpenDir(dir string) (io.ReadCloser, error) {
	segs, err := segmentFiles(dir)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("eventlog: no segment in %s: %w", dir, os.ErrNotExist)
	}
	files := make([]*os.File, 0, len(segs))
	readers := make([]io.Reader, 0, len(segs))
	for _, s := range segs {
		f, err := os.Open(s)
		if err != nil {
			for _, o := range files {
				_ = o.Close()
			}
			return nil, fmt.Errorf("eventlog: %w", err)
		}
		files = append(files, f)
		readers = append(readers, f)
	}
	return &segmentReader{Reader: io.MultiReader(readers...), files: files}, nil
}

// segmentReader is the concatenation of a directory's segments.
type segmentReader struct {
	io.Reader
	files []*os.File
}

func (r *segmentReader) Close() error {
	var errs []error
	for _, f := range r.files {
		errs = append(errs, f.Close())
	}
	return errors.Join(errs...)
}

// collect returns a scanFile callback appending every record to *out.
func collect(out *[]Record) func(Record) error {
	return func(r Record) error {
		*out = append(*out, r)
		return nil
	}
}

// Verify walks a chain and returns its head digest.
//
// This is invariant I6 as an executable check: record[n].PrevHash equals
// record[n-1].Hash, and every digest is what the record's contents imply.
func Verify(records []Record) (Digest, error) {
	head := ZeroDigest
	for i, r := range records {
		if r.Seq != uint64(i+1) {
			return ZeroDigest, fmt.Errorf("eventlog: record %d carries seq %d", i, r.Seq)
		}
		if err := VerifyRecord(r, head); err != nil {
			return ZeroDigest, err
		}
		head = r.Hash
	}
	return head, nil
}

// MemLog is an in-memory Appender for tests and for the DST harness, where a
// disk write would add an ordering the engine is not allowed to observe.
type MemLog struct {
	mu      sync.Mutex
	records []Record
	head    Digest
	seq     uint64
}

// NewMemLog returns an empty in-memory log.
func NewMemLog() *MemLog { return &MemLog{} }

// Append chains a record onto the head and keeps it in memory.
func (m *MemLog) Append(kind RecordKind, tickMs int64, payload any) (Record, error) {
	body, err := Canonical(payload)
	if err != nil {
		return Record{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := Chain(Record{Seq: m.seq + 1, TickMs: tickMs, Kind: kind, Payload: body}, m.head)
	if err != nil {
		return Record{}, err
	}
	m.records = append(m.records, rec)
	m.seq = rec.Seq
	m.head = rec.Hash
	return rec, nil
}

// Head is the digest of the last appended record.
func (m *MemLog) Head() Digest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.head
}

// Seq is the sequence number of the last appended record.
func (m *MemLog) Seq() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq
}

// Records copies out the accumulated chain.
func (m *MemLog) Records() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Record(nil), m.records...)
}

// WriteTo serialises the chain in the on-disk format, so a DST run or a test
// can be promoted to a committed fixture.
func (m *MemLog) WriteTo(w io.Writer) (int64, error) {
	var n int64
	for _, r := range m.Records() {
		b, err := encodeLine(r)
		if err != nil {
			return n, err
		}
		k, err := w.Write(b)
		n += int64(k)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// ErrNoRecords reports an operation that needs at least one record.
var ErrNoRecords = errors.New("eventlog: no records")

// HeadOf returns the head digest of a record slice without re-verifying it.
func HeadOf(records []Record) (Digest, error) {
	if len(records) == 0 {
		return ZeroDigest, ErrNoRecords
	}
	return records[len(records)-1].Hash, nil
}
