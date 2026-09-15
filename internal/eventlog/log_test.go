package eventlog

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Every test writes the same records to a Log on disk and to a MemLog. MemLog
// never touches a file, so it is the reference: whatever the disk path reads
// back must equal it record for record, head included.

// testSegmentBytes rolls a segment every few records, so a few dozen appends
// span many segments and every read crosses segment boundaries.
const testSegmentBytes = 1024

type testPayload struct {
	Index int    `json:"index"`
	Note  string `json:"note"`
}

var testKinds = []RecordKind{RecordEvent, RecordDecision, RecordCommand}

func openTestLog(t *testing.T, dir string) *Log {
	t.Helper()
	l, err := Open(dir, Options{MaxSegmentBytes: testSegmentBytes})
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	return l
}

func closeTestLog(t *testing.T, l *Log) {
	t.Helper()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// appendRecords appends records from (inclusive) to to (exclusive) to every
// appender, in the same order.
func appendRecords(t *testing.T, from, to int, logs ...Appender) {
	t.Helper()
	for i := from; i < to; i++ {
		kind := testKinds[i%len(testKinds)]
		p := testPayload{Index: i, Note: "record"}
		for _, a := range logs {
			if _, err := a.Append(kind, int64(i)*100, p); err != nil {
				t.Fatalf("append %d: %v", i, err)
			}
		}
	}
}

// writeTestLog writes n records to a fresh directory and returns it with the
// reference chain.
func writeTestLog(t *testing.T, n int) (string, *MemLog) {
	t.Helper()
	dir := t.TempDir()
	l := openTestLog(t, dir)
	mem := NewMemLog()
	appendRecords(t, 0, n, l, mem)
	closeTestLog(t, l)
	return dir, mem
}

func assertSameChain(t *testing.T, got []Record, want *MemLog) {
	t.Helper()
	w := want.Records()
	if len(got) != len(w) {
		t.Fatalf("read %d records, want %d", len(got), len(w))
	}
	for i := range w {
		if !reflect.DeepEqual(got[i], w[i]) {
			t.Fatalf("record %d differs\n  got  %+v\n  want %+v", i, got[i], w[i])
		}
	}
	head, err := HeadOf(got)
	if err != nil {
		t.Fatal(err)
	}
	if head != want.Head() {
		t.Fatalf("head %s, want %s", head, want.Head())
	}
}

func segmentsOf(t *testing.T, dir string) []string {
	t.Helper()
	segs, err := segmentFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	return segs
}

func TestLogRoundTripAcrossSegments(t *testing.T) {
	dir, mem := writeTestLog(t, 50)

	segs := segmentsOf(t, dir)
	if len(segs) < 3 {
		t.Fatalf("%d segments, want at least 3 for the test to cross boundaries", len(segs))
	}

	got, err := ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	assertSameChain(t, got, mem)

	first, err := ReadFile(segs[0])
	if err != nil {
		t.Fatalf("read first segment: %v", err)
	}
	if len(first) == 0 || len(first) >= len(got) {
		t.Fatalf("first segment holds %d of %d records", len(first), len(got))
	}
	if !reflect.DeepEqual(first, got[:len(first)]) {
		t.Fatal("first segment read alone differs from the same records read through the directory")
	}
}

// OpenDir streams the segments as one log: the bytes a MemLog would write,
// and a chain that verifies across the boundaries.
func TestOpenDirStreamsTheSegmentsInOrder(t *testing.T) {
	dir, mem := writeTestLog(t, 50)
	if len(segmentsOf(t, dir)) < 3 {
		t.Fatal("the test log does not cross segment boundaries")
	}
	rc, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if _, err := got.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var want bytes.Buffer
	if _, err := mem.WriteTo(&want); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatal("the streamed segments differ from the log's on-disk form")
	}
	var recs []Record
	for sc := NewScanner(&got); sc.Scan(); {
		recs = append(recs, sc.Record())
	}
	assertSameChain(t, recs, mem)

	if _, err := OpenDir(t.TempDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a directory without a segment: err %v, want os.ErrNotExist", err)
	}
}

func TestLogReopenContinuesChain(t *testing.T) {
	dir, mem := writeTestLog(t, 20)

	l := openTestLog(t, dir)
	if l.Seq() != mem.Seq() {
		t.Fatalf("reopened seq %d, want %d", l.Seq(), mem.Seq())
	}
	if l.Head() != mem.Head() {
		t.Fatalf("reopened head %s, want %s", l.Head(), mem.Head())
	}
	appendRecords(t, 20, 40, l, mem)
	closeTestLog(t, l)

	got, err := ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	assertSameChain(t, got, mem)
}

func TestLogRejectsCorruptChain(t *testing.T) {
	const target = `"index":7,`

	// corrupt rewrites the one line holding the target record.
	corrupt := func(t *testing.T, dir string, edit func(line []byte) []byte) {
		t.Helper()
		for _, s := range segmentsOf(t, dir) {
			b, err := os.ReadFile(s)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(b, []byte(target)) {
				continue
			}
			lines := bytes.SplitAfter(b, []byte("\n"))
			for i, ln := range lines {
				if bytes.Contains(ln, []byte(target)) {
					lines[i] = edit(ln)
				}
			}
			if err := os.WriteFile(s, bytes.Join(lines, nil), 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
		t.Fatalf("no segment holds %s", target)
	}

	t.Run("edited payload", func(t *testing.T) {
		dir, _ := writeTestLog(t, 20)
		corrupt(t, dir, func(ln []byte) []byte {
			return bytes.Replace(ln, []byte(target), []byte(`"index":70,`), 1)
		})

		var ce *ChainError
		_, err := Open(dir, Options{MaxSegmentBytes: testSegmentBytes})
		if !errors.As(err, &ce) || ce.Seq != 8 || ce.Field != "hash" {
			t.Fatalf("open: %v, want a hash mismatch at seq 8", err)
		}
		_, err = ReadDir(dir)
		if !errors.As(err, &ce) || ce.Seq != 8 || ce.Field != "hash" {
			t.Fatalf("read dir: %v, want a hash mismatch at seq 8", err)
		}
	})

	t.Run("dropped record", func(t *testing.T) {
		dir, _ := writeTestLog(t, 20)
		corrupt(t, dir, func([]byte) []byte { return nil })

		_, err := Open(dir, Options{MaxSegmentBytes: testSegmentBytes})
		if err == nil || !strings.Contains(err.Error(), "sequence jumps from 7 to 9") {
			t.Fatalf("open: %v, want a sequence jump from 7 to 9", err)
		}
		if _, err := ReadDir(dir); err == nil {
			t.Fatal("read dir accepted a chain with a record missing")
		}
	})
}
