package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/files"
	"github.com/dsanchez31/keel/internal/missionlog"
	"github.com/dsanchez31/keel/internal/planir"
)

const shippedPacks = "../../doctrine-packs"

// recordMission records the reference plan approved over the reference
// fleet, held where the world file puts it for 30 ticks, into app.
func recordMission(t *testing.T, app eventlog.Appender) {
	t.Helper()
	w, err := files.ReadWorld(referenceWorld)
	if err != nil {
		t.Fatal(err)
	}
	packs, err := files.ReadPacks(shippedPacks)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../examples/plans/reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var head struct {
		Intent string `json:"intent"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		t.Fatal(err)
	}
	res := planir.Validate(planir.Input{Intent: head.Intent, Raw: raw, World: w, Doctrines: packs, Arrival: engine.DefaultConfig().Arrival()})
	if !res.OK() {
		t.Fatalf("reference plan refused: %+v", res.Diagnostics)
	}
	plan := res.Plan.Clone()
	plan.Mission = "MSN-001"

	cfg := engine.DefaultConfig()
	if err := missionlog.AppendHeader(app, missionlog.Header{Name: "test", Seed: 7, Config: cfg, Plan: plan.Hash, Mission: plan.Mission}); err != nil {
		t.Fatal(err)
	}
	s := engine.NewState(7, cfg, packs)
	var batch []domain.Event
	for _, v := range w.Fleet {
		caps, st := v.Caps, v.State
		batch = append(batch, domain.Event{Kind: domain.EventVectorJoined, Vector: caps.ID, Caps: &caps, Telemetry: &st})
	}
	batch = append(batch, domain.Event{Kind: domain.EventPlanApproved, Plan: &plan})
	for range 30 {
		next, cmds, decs := engine.Step(s, batch)
		s = next
		if err := missionlog.AppendTick(app, s, batch, decs, cmds); err != nil {
			t.Fatal(err)
		}
		batch = nil
		for _, v := range w.Fleet {
			st := v.State
			st.LastSeenMs = 0
			batch = append(batch, domain.Event{Kind: domain.EventTelemetry, Vector: st.ID, Telemetry: &st})
		}
	}
}

func encodedLog(t *testing.T, log *eventlog.MemLog) []byte {
	t.Helper()
	var b bytes.Buffer
	if _, err := log.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A mission log verifies from each of the three places it can be read from:
// a file, keeld's segment directory, standard input.
func TestReplayVerifiesTheRecording(t *testing.T) {
	mem := eventlog.NewMemLog()
	recordMission(t, mem)
	raw := encodedLog(t, mem)

	file := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "MSN-001")
	disk, err := eventlog.Open(dir, eventlog.Options{MaxSegmentBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	recordMission(t, disk)
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		path  string
		stdin []byte
	}{
		{"file", file, nil},
		{"directory", dir, nil},
		{"stdin", "-", raw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := keelctlIn(t, bytes.NewReader(tc.stdin), "replay", "--doctrine-dir", shippedPacks, "--verify-hash", tc.path)
			if code != exitOK {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			for _, want := range []string{"assignment", "mission MSN-001", "recorded  " + mem.Head().String(), "replayed  " + mem.Head().String(), "reproduces the recording"} {
				if !strings.Contains(out, want) {
					t.Fatalf("output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

// Without --verify-hash the replay prints its timeline and the head it
// rebuilt, and passes no verdict.
func TestReplayWithoutVerifyPrintsTheHead(t *testing.T) {
	mem := eventlog.NewMemLog()
	recordMission(t, mem)
	code, out, errOut := keelctlIn(t, bytes.NewReader(encodedLog(t, mem)), "replay", "--doctrine-dir", shippedPacks, "-")
	if code != exitOK || !strings.Contains(out, "head      "+mem.Head().String()) || strings.Contains(out, "verdict") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

// A decision forged under a rebuilt chain links, and the verifying replay
// names its record and exits 2.
func TestReplayReportsADivergence(t *testing.T) {
	mem := eventlog.NewMemLog()
	recordMission(t, mem)
	forged := eventlog.NewMemLog()
	var at uint64
	for _, r := range mem.Records() {
		p := r.Payload
		if at == 0 && r.Kind == eventlog.RecordDecision {
			at = r.Seq
			p = bytes.Replace(p, []byte(`"rationale":"`), []byte(`"rationale":"forged: `), 1)
		}
		if _, err := forged.Append(r.Kind, r.TickMs, eventlog.RawJSON(p)); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errOut := keelctlIn(t, bytes.NewReader(encodedLog(t, forged)), "replay", "--doctrine-dir", shippedPacks, "--verify-hash", "-")
	if code != exitNotVerified || !strings.Contains(errOut, "not verified") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"diverged  at record " + strconv.FormatUint(at, 10), "forged: ", "does not reproduce"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

// A broken chain fails verification with 2, and a plain replay with 1: the
// recording cannot be trusted either way.
func TestReplayOfABrokenChain(t *testing.T) {
	mem := eventlog.NewMemLog()
	recordMission(t, mem)
	raw := string(encodedLog(t, mem))
	i := strings.LastIndex(raw, `"explored":`)
	broken := raw[:i] + `"explored":9` + raw[i+len(`"explored":`):]

	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"verifying", []string{"--verify-hash"}, exitNotVerified},
		{"plain", nil, exitError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]string{"replay", "--doctrine-dir", shippedPacks}, tc.args...), "-")
			code, _, errOut := keelctlIn(t, strings.NewReader(broken), args...)
			if code != tc.want || !strings.Contains(errOut, "chain is broken") {
				t.Fatalf("exit %d, want %d, stderr %q", code, tc.want, errOut)
			}
		})
	}
}
