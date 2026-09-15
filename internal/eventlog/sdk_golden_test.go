package eventlog

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateSDKGolden = flag.Bool("update-sdk-golden", false, "rewrite sdk-ts/src/testdata/chain.ndjson from the Go encoder")

// sdkGoldenPath is the chain the browser's own check (sdk-ts/src/chain.ts)
// is tested against.
var sdkGoldenPath = filepath.Join("..", "..", "sdk-ts", "src", "testdata", "chain.ndjson")

// sdkGoldenRecords are payloads chosen for what the TypeScript side must
// hash byte for byte without re-encoding anything: non-ASCII, every escape,
// a control character, floats the encoder renders with 17 digits, nesting,
// an empty array, several ticks.
var sdkGoldenRecords = []struct {
	kind    RecordKind
	tickMs  int64
	payload any
}{
	{RecordHeader, 0, map[string]any{"name": "golden", "note": "é ✓ 🛰 \"quoted\" back\\slash\nline\ttab\x01"}},
	{RecordEvent, 100, map[string]any{"kind": "telemetry", "values": []any{0.1, 1.5, -2, 1e21, 3}}},
	{RecordDecision, 100, map[string]any{"rationale": "</script> & <b>", "nested": map[string]any{"b": []any{}, "a": true}}},
	{RecordTick, 100, map[string]any{"state": "running", "tick": 1}},
	{RecordTick, 200, map[string]any{"tick": 2}},
}

// TestSDKChainGolden pins the contract between the Go chain and the one the
// browser recomputes: the file sdk-ts/src/chain.test.ts verifies is what this
// encoder writes. A change to the record layout, the preimage or the
// encoder fails here first; regenerate with -update-sdk-golden, then the
// TypeScript test says whether the browser still agrees.
func TestSDKChainGolden(t *testing.T) {
	log := NewMemLog()
	for _, r := range sdkGoldenRecords {
		if _, err := log.Append(r.kind, r.tickMs, r.payload); err != nil {
			t.Fatal(err)
		}
	}
	var got bytes.Buffer
	if _, err := log.WriteTo(&got); err != nil {
		t.Fatal(err)
	}
	if *updateSDKGolden {
		if err := os.WriteFile(sdkGoldenPath, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(sdkGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("%s is not what the encoder writes; regenerate with go test ./internal/eventlog -run TestSDKChainGolden -update-sdk-golden\ngot:\n%s", sdkGoldenPath, got.Bytes())
	}
}
