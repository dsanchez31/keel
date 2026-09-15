package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/missionlog"
	"github.com/dsanchez31/keel/internal/planir"
	"github.com/dsanchez31/keel/internal/planner"
	"github.com/dsanchez31/keel/internal/transport"
)

var _ transport.Service = (*Daemon)(nil)

// Compile compiles an intent against the live fleet: every vector the engine
// holds, neither written off nor with its link lost, with what it declared and
// its latest state, in place of the world file's fleet snapshot. A plan is
// validated against the fleet that will fly it. The areas and stations are
// the world file's.
func (d *Daemon) Compile(ctx context.Context, req transport.CompileRequest) (planner.Outcome, error) {
	backend := req.Backend
	if backend == "" {
		backend = d.cfg.Planner.Backend
	}
	p, err := d.planners(backend)
	if err != nil {
		if errors.Is(err, planner.ErrUnknownBackend) {
			return planner.Outcome{}, fmt.Errorf("%w: %v", transport.ErrInvalid, err)
		}
		return planner.Outcome{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, d.cfg.Planner.Timeout)
	defer cancel()
	out, err := planner.Compile(ctx, p, planner.Input{Intent: req.Intent, World: d.liveWorld(), Doctrines: d.packs, Arrival: d.engine.Arrival()})
	if out.Plan != nil {
		d.mu.Lock()
		d.pending = slices.DeleteFunc(d.pending, func(p domain.ApprovedPlan) bool { return p.Hash == out.Plan.Hash })
		d.pending = append(d.pending, out.Plan.Clone())
		if len(d.pending) > MaxPendingPlans {
			d.pending = d.pending[len(d.pending)-MaxPendingPlans:]
		}
		d.mu.Unlock()
	}
	return out, err
}

// liveWorld is the world file with the engine's current fleet.
func (d *Daemon) liveWorld() *planir.World {
	d.mu.Lock()
	view := d.view
	d.mu.Unlock()
	w := *d.world
	w.Fleet = nil
	for _, v := range view.Vectors {
		if v.State.Mode == domain.ModeDown || v.State.Link == domain.LinkLost {
			continue
		}
		w.Fleet = append(w.Fleet, planir.FleetVector{Caps: v.Caps, State: v.State})
	}
	return &w
}

// Approve opens the human gate: the pending plan is stamped with the next
// mission id, its log is created and its header recorded now, so a disk that
// refuses is the operator's answer rather than a mission running unrecorded,
// and the engine starts it at the next tick.
func (d *Daemon) Approve(_ context.Context, hash domain.PlanHash) (domain.MissionID, error) {
	d.mu.Lock()
	switch {
	case d.stopping:
		d.mu.Unlock()
		return "", fmt.Errorf("%w: the daemon is shutting down", transport.ErrConflict)
	case d.running != "":
		running := d.running
		d.mu.Unlock()
		return "", fmt.Errorf("%w: mission %s is running", transport.ErrConflict, running)
	}
	i := slices.IndexFunc(d.pending, func(p domain.ApprovedPlan) bool { return p.Hash == hash })
	if i < 0 {
		d.mu.Unlock()
		return "", fmt.Errorf("%w: no pending plan %s", transport.ErrNotFound, hash)
	}
	plan := d.pending[i].Clone()
	d.pending = slices.Delete(d.pending, i, i+1)
	plan.Mission = missionID(d.next)
	d.next++
	d.running = plan.Mission
	d.mu.Unlock()

	seed := d.newSeed()
	l, err := d.openMission(plan, seed)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		d.running = ""
		return "", err
	}
	if d.stopping {
		// Shutdown began while the log was being opened: the mission never
		// starts, and its header-only log goes.
		d.running = ""
		_ = l.Close()
		_ = os.RemoveAll(l.Dir())
		return "", fmt.Errorf("%w: the daemon is shutting down", transport.ErrConflict)
	}
	d.start = &approval{plan: plan, seed: seed, log: l}
	d.log.Info("plan approved", "mission", plan.Mission, "plan", plan.Hash, "seed", seed)
	return plan.Mission, nil
}

// Discard drops a pending plan.
func (d *Daemon) Discard(_ context.Context, hash domain.PlanHash) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := slices.IndexFunc(d.pending, func(p domain.ApprovedPlan) bool { return p.Hash == hash })
	if i < 0 {
		return fmt.Errorf("%w: no pending plan %s", transport.ErrNotFound, hash)
	}
	d.pending = slices.Delete(d.pending, i, i+1)
	return nil
}

// Mission is the running mission's view, or the final view of a mission this
// process ran. A mission approved and not started yet has none: it starts at
// the next tick.
func (d *Daemon) Mission(_ context.Context, id domain.MissionID) (transport.MissionView, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if v, ok := d.finished[id]; ok {
		return v, nil
	}
	if id == d.running {
		if d.view.Mission.ID == id {
			return d.view, nil
		}
		return transport.MissionView{}, fmt.Errorf("%w: mission %s starts at the next tick", transport.ErrNotFound, id)
	}
	return transport.MissionView{}, fmt.Errorf("%w: no mission %s in this process, its log may still replay", transport.ErrNotFound, id)
}

// SwapDoctrine queues a hot swap of the running mission's pack for the next
// tick boundary, pinned to the hash of the pack the registry holds. The
// registry is loaded once, so the pin never refuses a live swap; it is what a
// replay against an edited pack is refused on.
//
// A mission that exists and is not running (approved and not started yet,
// finished, or recorded by an earlier process) is a conflict; one that never
// existed is not found, as it is for every other route.
func (d *Daemon) SwapDoctrine(_ context.Context, id domain.MissionID, ref domain.DoctrineRef) error {
	pack, err := d.packs.Resolve(ref)
	if err != nil {
		return fmt.Errorf("%w: %v", transport.ErrInvalid, err)
	}
	d.mu.Lock()
	if id == d.running && d.view.Mission.ID == id {
		r := ref
		d.inbox = append(d.inbox, domain.Event{Kind: domain.EventDoctrineSwap, Doctrine: &r, DoctrineHash: pack.Hash})
		d.mu.Unlock()
		return nil
	}
	_, finished := d.finished[id]
	known := finished || id == d.running
	d.mu.Unlock()
	if !known && !d.recorded(id) {
		return fmt.Errorf("%w: no mission %s", transport.ErrNotFound, id)
	}
	return fmt.Errorf("%w: mission %s is not running", transport.ErrConflict, id)
}

// recorded reports whether a mission has a log directory, whichever process
// recorded it. Only an id shaped as keeld assigns them reaches the path.
func (d *Daemon) recorded(id domain.MissionID) bool {
	if !missionPattern.MatchString(string(id)) {
		return false
	}
	info, err := os.Stat(filepath.Join(d.missionsDir(), string(id)))
	return err == nil && info.IsDir()
}

// Doctrines lists the registered packs, sorted by name then version.
func (d *Daemon) Doctrines(context.Context) ([]transport.DoctrineInfo, error) {
	refs := d.packs.Refs()
	out := make([]transport.DoctrineInfo, 0, len(refs))
	for _, ref := range refs {
		p, err := d.packs.Resolve(ref)
		if err != nil {
			return nil, err
		}
		out = append(out, transport.NewDoctrineInfo(p))
	}
	return out, nil
}

// World is the world file keeld loaded at startup: its areas and stations,
// the names an intent may use. It never changes while keeld runs.
func (d *Daemon) World(context.Context) (transport.WorldView, error) {
	return transport.NewWorldView(d.world), nil
}

// Fleet is every bound vector's health now, in id order: its adapter's
// verdict and why, a vector that has not joined the engine included. An
// autopilot whose frames wait for its position estimate says so here, and
// nowhere else before it joins.
func (d *Daemon) Fleet(context.Context) ([]transport.FleetVectorView, error) {
	health := d.fleet.Health()
	out := make([]transport.FleetVectorView, 0, len(health))
	for _, id := range engine.SortedKeys(health) {
		out = append(out, transport.FleetVectorView{ID: id, Health: health[id].Kind, Detail: health[id].Detail})
	}
	return out, nil
}

// InjectFault forwards a fault to the simulator serving its vector and, once
// the simulator accepted it, queues the fault_injected event for the next
// tick, so the log records the fault the vehicle undergoes and only that.
func (d *Daemon) InjectFault(ctx context.Context, f domain.Fault) error {
	if err := d.fleet.InjectFault(ctx, f); err != nil {
		switch {
		case errors.Is(err, ErrUnbound), errors.Is(err, ErrNotSimulated), errors.Is(err, ErrFaultRefused):
			return fmt.Errorf("%w: %v", transport.ErrInvalid, err)
		case errors.Is(err, ErrSimulator):
			return fmt.Errorf("%w: %v", transport.ErrUpstream, err)
		}
		return err
	}
	fault := f
	d.mu.Lock()
	d.inbox = append(d.inbox, domain.Event{Kind: domain.EventFaultInjected, Vector: f.Vector, Fault: &fault})
	d.mu.Unlock()
	return nil
}

// Replay opens a finished mission's log as it is on disk. The running
// mission's log is still being written: its tail may be a record cut in two,
// and the stream is what follows it live.
func (d *Daemon) Replay(_ context.Context, id domain.MissionID) (io.ReadCloser, error) {
	// The id names a directory: nothing but a mission id may reach the path.
	if !missionPattern.MatchString(string(id)) {
		return nil, fmt.Errorf("%w: no mission %s", transport.ErrNotFound, id)
	}
	d.mu.Lock()
	running := id == d.running
	d.mu.Unlock()
	if running {
		return nil, fmt.Errorf("%w: mission %s is still running, follow the stream", transport.ErrConflict, id)
	}
	rc, err := eventlog.OpenDir(filepath.Join(d.missionsDir(), string(id)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: no log for mission %s", transport.ErrNotFound, id)
	}
	return rc, err
}

// ReplayWindow replays a finished mission's log through the engine, against
// the daemon's registry, over a window of mission time. A log whose chain is
// broken, or that is not a mission log, is a conflict: its state is what
// refuses the replay, and the operator is told so rather than handed a 500.
func (d *Daemon) ReplayWindow(ctx context.Context, id domain.MissionID, fromMs, toMs int64) (transport.ReplayWindow, error) {
	rc, err := d.Replay(ctx, id)
	if err != nil {
		return transport.ReplayWindow{}, err
	}
	defer func() { _ = rc.Close() }()
	w, err := transport.BuildReplayWindow(rc, d.packs, fromMs, toMs)
	if errors.Is(err, missionlog.ErrBroken) || errors.Is(err, missionlog.ErrLayout) {
		return transport.ReplayWindow{}, fmt.Errorf("%w: mission %s: %w", transport.ErrConflict, id, err)
	}
	return w, err
}
