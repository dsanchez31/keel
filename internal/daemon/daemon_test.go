package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/files"
	"github.com/dsanchez31/keel/internal/missionlog"
	"github.com/dsanchez31/keel/internal/pacer"
	"github.com/dsanchez31/keel/internal/planner"
	"github.com/dsanchez31/keel/internal/transport"
)

var referencePlan = filepath.Join("..", "..", "examples", "plans", "reference.json")

// referenceIntent is the intent the reference plan echoes.
const referenceIntent = "Grid-search the unexplored area to lift the fog of war"

// fakePlanner finds a mission in every intent and answers every plan attempt
// with the same reply.
type fakePlanner struct{ reply string }

func (fakePlanner) Name() string { return "fake" }

func (f fakePlanner) Propose(_ context.Context, c planner.Conversation) (string, error) {
	if planner.IsTriage(c) {
		return `{"reason":"A grid search of the unexplored area.","mission":true}`, nil
	}
	return f.reply, nil
}

// received records what the vehicles were commanded.
type received struct {
	mu   sync.Mutex
	cmds []domain.Command
}

func (r *received) add(c domain.Command) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, c)
}

func (r *received) of(t domain.CommandType) []domain.Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Command
	for _, c := range r.cmds {
		if c.Type == t {
			out = append(out, c)
		}
	}
	return out
}

// harness is a daemon over the reference fleet, every vector a native vehicle
// that stands on its pad, sends its world-file state every 20 ms and
// acknowledges every command, faults going to a fake simulator.
type harness struct {
	d    *Daemon
	cfg  *Config
	sim  *simulator
	got  *received
	hub  *transport.Hub
	runs chan error
	stop context.CancelFunc
}

func newHarness(t *testing.T, dataDir string) *harness {
	t.Helper()
	w := readReferenceWorld(t)
	packs, err := files.ReadPacks(shippedPacks)
	if err != nil {
		t.Fatal(err)
	}
	vs := newVehicles(t, w)
	sim := &simulator{status: http.StatusAccepted}
	ss := httptest.NewServer(sim)
	t.Cleanup(ss.Close)
	h := &harness{sim: sim, got: &received{}}

	cfg := &Config{Name: "test", DataDir: dataDir, Planner: PlannerConfig{Backend: planner.BackendOllama, Timeout: 10 * time.Second}}
	for _, fv := range w.Fleet {
		id := fv.Caps.ID
		cfg.Vectors = append(cfg.Vectors, Binding{ID: id, Native: &NativeBinding{URL: vs.url(id), Faults: ss.URL + "/v1/faults"}})
		v := vs.add(t, id)
		go func() {
			var ack uint64
			tk := time.NewTicker(20 * time.Millisecond)
			defer tk.Stop()
			for {
				select {
				case c, ok := <-v.Commands():
					if !ok {
						return
					}
					ack = c.Seq
					h.got.add(c)
				case <-tk.C:
					s := frame(id, fv)
					s.AckSeq = ack
					if v.Send(s) != nil {
						return
					}
				}
			}
		}()
	}
	h.cfg = cfg
	fleet := newTestFleet(t, cfg, w)
	if h.hub, err = transport.NewHub(transport.HubOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.hub.Close() })
	reply, err := os.ReadFile(referencePlan)
	if err != nil {
		t.Fatal(err)
	}
	p, err := pacer.NewRealTime(10)
	if err != nil {
		t.Fatal(err)
	}
	h.d, err = New(Options{
		Config: cfg, World: w, Packs: packs, Fleet: fleet, Hub: h.hub, Pacer: p,
		Planner: func(backend string) (planner.Planner, error) {
			if backend == planner.BackendOllama {
				return fakePlanner{reply: string(reply)}, nil
			}
			return planner.NewBackend(backend, planner.BackendOptions{})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// run starts the loop until the test ends or stopAndWait.
func (h *harness) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.stop, h.runs = cancel, make(chan error, 1)
	go func() { h.runs <- h.d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-h.runs
	})
}

func (h *harness) stopAndWait(t *testing.T) {
	t.Helper()
	h.stop()
	select {
	case err := <-h.runs:
		if err != nil {
			t.Fatalf("Run returned %v, want nil after a clean shutdown", err)
		}
		h.runs <- nil
	case <-time.After(2 * wait):
		t.Fatal("Run did not return")
	}
}

// tickUntilFleet ticks the daemon by hand, the loop not running, until the
// idle engine holds the whole fleet.
func (h *harness) tickUntilFleet(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * wait)
	for len(h.d.current().Vectors) < 6 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the fleet")
		}
		if _, err := h.d.tick(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (d *Daemon) current() transport.MissionView {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.view
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// compileReference compiles the reference intent once the whole fleet is in
// the idle engine, and returns the plan's hash.
func (h *harness) compileReference(t *testing.T) domain.PlanHash {
	t.Helper()
	eventually(t, "the fleet in the idle engine", func() bool { return len(h.d.current().Vectors) == 6 })
	out, err := h.d.Compile(context.Background(), transport.CompileRequest{Intent: referenceIntent})
	if err != nil {
		t.Fatal(err)
	}
	if !out.OK() {
		t.Fatalf("the reference plan was refused against the live fleet: %+v", out.Attempts)
	}
	return out.Plan.Hash
}

// The whole lifecycle: the fleet seen before any plan, a plan compiled
// against it and approved, a mission recording into its own log, a fault
// forwarded and recorded, a hot swap, and a shutdown that stops the fleet
// through the engine and leaves a log that verifies.
func TestMissionLifecycle(t *testing.T) {
	data := t.TempDir()
	h := newHarness(t, data)
	h.run(t)
	ctx := context.Background()

	idle := func() transport.MissionView { return h.d.current() }
	eventually(t, "the fleet in the idle engine", func() bool { return len(idle().Vectors) == 6 })
	if v := idle(); v.Mission.ID != "" || v.Head != eventlog.ZeroDigest.String() {
		t.Fatalf("idle view mission %q head %s, want no mission and the empty chain", v.Mission.ID, v.Head)
	}

	hash := h.compileReference(t)
	id, err := h.d.Approve(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if id != "MSN-001" {
		t.Fatalf("mission id %s, want MSN-001", id)
	}
	if _, err := h.d.Approve(ctx, hash); !errors.Is(err, transport.ErrConflict) {
		t.Fatalf("a second approval while a mission runs: err %v, want ErrConflict", err)
	}
	eventually(t, "the mission running", func() bool {
		v, err := h.d.Mission(ctx, id)
		return err == nil && v.Mission.State == domain.MissionRunning
	})
	eventually(t, "the launch gotos reaching the fleet", func() bool { return len(h.got.of(domain.CommandGoto)) >= 4 })

	if err := h.d.InjectFault(ctx, domain.Fault{Kind: domain.FaultLinkLoss, Vector: "DRONE-02", DurationMs: 5000}); err != nil {
		t.Fatal(err)
	}
	if got := h.sim.received(); len(got) != 1 || !strings.Contains(got[0], `"vector":"DRONE-02"`) {
		t.Fatalf("the simulator received %q", got)
	}
	swapTo := domain.DoctrineRef{Name: "recon-standard", Version: "2.1.0"}
	pack, err := h.d.packs.Resolve(swapTo)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.d.SwapDoctrine(ctx, id, swapTo); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the hot swap applied", func() bool { return h.d.current().DoctrineHash == pack.Hash })
	if _, err := h.d.Replay(ctx, id); !errors.Is(err, transport.ErrConflict) {
		t.Fatalf("replaying the running mission: err %v, want ErrConflict", err)
	}

	h.stopAndWait(t)

	aborts := h.got.of(domain.CommandAbort)
	if len(aborts) == 0 {
		t.Fatal("shutdown stopped no vehicle")
	}
	v, err := h.d.Mission(ctx, id)
	if err != nil || v.Mission.State != domain.MissionFailed {
		t.Fatalf("final view %+v, err %v, want the mission failed", v.Mission, err)
	}

	recs, err := eventlog.ReadDir(filepath.Join(data, "missions", string(id)))
	if err != nil {
		t.Fatalf("the mission log does not verify: %v", err)
	}
	if head, _ := eventlog.HeadOf(recs); head.String() != v.Head {
		t.Fatalf("log head %s, final view head %s", head, v.Head)
	}
	var header missionlog.Header
	if err := json.Unmarshal(recs[0].Payload, &header); err != nil || recs[0].Kind != eventlog.RecordHeader {
		t.Fatalf("first record %s: %v", recs[0].Kind, err)
	}
	if header.Name != "test" || header.Mission != id || header.Plan != hash || header.Config != h.d.engine {
		t.Fatalf("header %+v", header)
	}
	var kinds []domain.EventKind
	var rationales []string
	var recorded []domain.Command
	for _, r := range recs[1:] {
		switch r.Kind {
		case eventlog.RecordEvent:
			var ev domain.Event
			if err := json.Unmarshal(r.Payload, &ev); err != nil {
				t.Fatal(err)
			}
			kinds = append(kinds, ev.Kind)
		case eventlog.RecordDecision:
			var dec domain.Decision
			if err := json.Unmarshal(r.Payload, &dec); err != nil {
				t.Fatal(err)
			}
			rationales = append(rationales, dec.Rationale)
		case eventlog.RecordCommand:
			var c domain.Command
			if err := json.Unmarshal(r.Payload, &c); err != nil {
				t.Fatal(err)
			}
			recorded = append(recorded, c)
		}
	}
	joins := 0
	for _, k := range kinds[:7] {
		if k == domain.EventVectorJoined {
			joins++
		}
	}
	if joins != 6 || kinds[6] != domain.EventPlanApproved {
		t.Fatalf("first batch %v, want the six joins and the approval", kinds[:7])
	}
	for _, k := range []domain.EventKind{domain.EventFaultInjected, domain.EventDoctrineSwap, domain.EventOperatorAbort} {
		if !slices.Contains(kinds, k) {
			t.Errorf("no %s event recorded", k)
		}
	}
	for _, want := range []string{"fault link_loss injected on DRONE-02", "mission failed: " + shutdownReason} {
		if !slices.ContainsFunc(rationales, func(r string) bool { return strings.Contains(r, want) }) {
			t.Errorf("no decision %q recorded", want)
		}
	}
	for _, a := range aborts {
		if !slices.Contains(recorded, a) {
			t.Errorf("abort %+v reached a vehicle without being recorded", a)
		}
	}

	rc, err := h.d.Replay(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	var streamed bytes.Buffer
	if _, err := streamed.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	var n int
	for sc := eventlog.NewScanner(&streamed); sc.Scan(); {
		n++
	}
	if n != len(recs) {
		t.Fatalf("the replay streams %d records, the log holds %d", n, len(recs))
	}
}

// A restart numbers its missions after the ones already recorded.
func TestMissionsAreNumberedAcrossRestarts(t *testing.T) {
	data := t.TempDir()
	for _, dir := range []string{"MSN-001", "MSN-007", "notes"} {
		if err := os.MkdirAll(filepath.Join(data, "missions", dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t, data)
	h.run(t)
	id, err := h.d.Approve(context.Background(), h.compileReference(t))
	if err != nil {
		t.Fatal(err)
	}
	if id != "MSN-008" {
		t.Fatalf("mission id %s, want MSN-008 after MSN-007", id)
	}
}

// An approval the loop has not started yet is dropped at shutdown, its
// header-only log with it, and the gate stays shut after.
func TestShutdownDropsAMissionNotStarted(t *testing.T) {
	data := t.TempDir()
	h := newHarness(t, data)
	// Ticked by hand: no tick runs between the approval and the shutdown.
	h.tickUntilFleet(t)
	hash := h.compileReference(t)
	if _, err := h.d.Approve(context.Background(), hash); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(data, "missions")); len(entries) != 1 {
		t.Fatalf("missions directory holds %v after the approval, want its log", entries)
	}
	if err := h.d.shutdown(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(data, "missions")); len(entries) != 0 {
		t.Fatalf("missions directory holds %v after shutdown, want the unstarted log gone", entries)
	}
	if len(h.got.of(domain.CommandAbort)) != 0 {
		t.Fatal("shutdown stopped vehicles no mission had moved")
	}
	if _, err := h.d.Approve(context.Background(), hash); !errors.Is(err, transport.ErrConflict) {
		t.Fatalf("an approval after shutdown: err %v, want ErrConflict", err)
	}
}
