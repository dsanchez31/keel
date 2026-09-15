package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/transport"
)

func TestServiceRefusals(t *testing.T) {
	data := t.TempDir()
	// A mission an earlier process recorded: known, not running.
	if err := os.MkdirAll(filepath.Join(data, "missions", "MSN-007"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, data)
	ctx := context.Background()
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"unknown backend", second(h.d.Compile(ctx, transport.CompileRequest{Intent: referenceIntent, Backend: "gpt"})), transport.ErrInvalid},
		{"approve an unknown plan", second(h.d.Approve(ctx, "ab12")), transport.ErrNotFound},
		{"discard an unknown plan", h.d.Discard(ctx, "ab12"), transport.ErrNotFound},
		{"an unknown mission", second(h.d.Mission(ctx, "MSN-001")), transport.ErrNotFound},
		{"swap to an unknown pack", h.d.SwapDoctrine(ctx, "MSN-001", domain.DoctrineRef{Name: "recon-standard", Version: "9.0.0"}), transport.ErrInvalid},
		{"swap for an unknown mission", h.d.SwapDoctrine(ctx, "MSN-001", domain.DoctrineRef{Name: "recon-standard", Version: "2.1.0"}), transport.ErrNotFound},
		{"swap for a path", h.d.SwapDoctrine(ctx, "../../etc", domain.DoctrineRef{Name: "recon-standard", Version: "2.1.0"}), transport.ErrNotFound},
		{"swap for a recorded mission", h.d.SwapDoctrine(ctx, "MSN-007", domain.DoctrineRef{Name: "recon-standard", Version: "2.1.0"}), transport.ErrConflict},
		{"fault on an unbound vector", h.d.InjectFault(ctx, domain.Fault{Kind: domain.FaultKill, Vector: "DRONE-09"}), transport.ErrInvalid},
		{"replay a path", second(h.d.Replay(ctx, "../../etc")), transport.ErrNotFound},
		{"replay an unknown mission", second(h.d.Replay(ctx, "MSN-999")), transport.ErrNotFound},
	}
	for _, tc := range cases {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: err %v, want %v", tc.name, tc.err, tc.want)
		}
	}

	h.sim.answer(http.StatusUnprocessableEntity, "vector DRONE-02 is not in the fleet")
	if err := h.d.InjectFault(ctx, domain.Fault{Kind: domain.FaultKill, Vector: "DRONE-02"}); !errors.Is(err, transport.ErrInvalid) || !strings.Contains(err.Error(), "not in the fleet") {
		t.Errorf("a fault the simulator refuses: err %v, want ErrInvalid with its detail", err)
	}
	h.sim.answer(http.StatusInternalServerError, "boom")
	if err := h.d.InjectFault(ctx, domain.Fault{Kind: domain.FaultKill, Vector: "DRONE-02"}); !errors.Is(err, transport.ErrUpstream) {
		t.Errorf("a failing simulator: err %v, want ErrUpstream", err)
	}
	h.d.mu.Lock()
	queued := len(h.d.inbox)
	h.d.mu.Unlock()
	if queued != 0 {
		t.Fatalf("%d refused faults queued for the engine", queued)
	}
}

func second[T any](_ T, err error) error { return err }

// The pending plans are bounded, the oldest dropped, and a plan compiled
// twice is held once.
func TestPendingPlansAreBounded(t *testing.T) {
	h := newHarness(t, t.TempDir())
	for i := range MaxPendingPlans {
		h.d.pending = append(h.d.pending, domain.ApprovedPlan{Hash: domain.PlanHash(fmt.Sprintf("%064d", i))})
	}
	oldest := h.d.pending[0].Hash

	h.tickUntilFleet(t)
	hash := h.compileReference(t)
	hash2 := h.compileReference(t)
	if hash != hash2 {
		t.Fatalf("one reply compiled to two hashes %s and %s", hash, hash2)
	}
	if n := len(h.d.pending); n != MaxPendingPlans {
		t.Fatalf("%d pending plans, want the bound %d", n, MaxPendingPlans)
	}
	if slices.ContainsFunc(h.d.pending, func(p domain.ApprovedPlan) bool { return p.Hash == oldest }) {
		t.Fatal("the oldest pending plan was kept past the bound")
	}
	if got := slices.IndexFunc(h.d.pending, func(p domain.ApprovedPlan) bool { return p.Hash == hash }); got != MaxPendingPlans-1 {
		t.Fatalf("the plan compiled twice sits at %d, want once, newest", got)
	}
	if err := h.d.Discard(context.Background(), hash); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Approve(context.Background(), hash); !errors.Is(err, transport.ErrNotFound) {
		t.Fatalf("approving a discarded plan: err %v, want ErrNotFound", err)
	}
}

func TestWorldListsTheWorldFile(t *testing.T) {
	h := newHarness(t, t.TempDir())
	w, err := h.d.World(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Areas) != 1 || w.Areas[0].Name != "fog_of_war_east" || len(w.Areas[0].Polygon.Ring) == 0 {
		t.Fatalf("areas %+v, want fog_of_war_east with its outline", w.Areas)
	}
	if len(w.Stations) != 1 || w.Stations[0].Name != "gcs-west" {
		t.Fatalf("stations %+v, want gcs-west", w.Stations)
	}
}

// Fleet lists every bound vector from the start, joined or not, then each
// with its adapter's verdict once its frames flow.
func TestFleetListsEveryBoundVector(t *testing.T) {
	h := newHarness(t, t.TempDir())
	ids := func(fleet []transport.FleetVectorView) []domain.VectorID {
		out := make([]domain.VectorID, len(fleet))
		for i, v := range fleet {
			out[i] = v.ID
		}
		return out
	}
	want := make([]domain.VectorID, 0, len(h.cfg.Vectors))
	for _, b := range h.cfg.Vectors {
		want = append(want, b.ID)
	}
	slices.Sort(want)

	before, err := h.d.Fleet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(before), want) {
		t.Fatalf("fleet %v before any tick, want every binding %v in id order", ids(before), want)
	}

	h.tickUntilFleet(t)
	after, err := h.d.Fleet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(after), want) {
		t.Fatalf("fleet %v once joined, want %v", ids(after), want)
	}
	for _, v := range after {
		if v.Health != domain.HealthOK {
			t.Errorf("%s: health %s %q once its frames flow, want ok", v.ID, v.Health, v.Detail)
		}
	}
}

func TestDoctrinesListsTheRegistry(t *testing.T) {
	h := newHarness(t, t.TempDir())
	infos, err := h.d.Doctrines(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, i := range infos {
		if i.Hash == "" {
			t.Errorf("%s carries no hash", i.Ref)
		}
		refs = append(refs, i.Ref)
	}
	for _, want := range []string{"recon-standard@2.0.0", "recon-standard@2.1.0", "recon-standard@2.2.0"} {
		if !slices.Contains(refs, want) {
			t.Errorf("packs %v lack %s", refs, want)
		}
	}
}
