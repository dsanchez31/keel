package daemon

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/missionlog"
	"github.com/dsanchez31/keel/internal/pacer"
	"github.com/dsanchez31/keel/internal/planir"
	"github.com/dsanchez31/keel/internal/planner"
	"github.com/dsanchez31/keel/internal/transport"
)

// MaxPendingPlans bounds the compiled plans held for approval. Compiling one
// more drops the oldest, which the operator has most likely moved on from.
const MaxPendingPlans = 16

// ackWait bounds how long shutdown waits for the vectors it stopped to
// acknowledge the stop.
const ackWait = 3 * time.Second

// shutdownReason is the operator_abort a shutdown records.
const shutdownReason = "daemon shutting down"

// missionPattern is a mission id keeld assigns, and the name of its log
// directory under data_dir/missions.
var missionPattern = regexp.MustCompile(`^MSN-([0-9]{3,9})$`)

// Options is what a Daemon runs on besides its configuration.
type Options struct {
	Config *Config
	World  *planir.World
	Packs  *doctrine.Registry
	Fleet  *Fleet
	Hub    *transport.Hub
	// Pacer paces the ticks. Nil means real time: the fleet moves in it. A
	// pacer.Scalable one takes the pace SetClock sets; keeld hands its scaled
	// clock to the fleet's agents too (FleetOptions.Agent.Clock).
	Pacer pacer.Pacer
	// Planner returns the planner of a backend name. Nil means
	// planner.NewBackend, with the configured model and Ollama URL for the
	// configured backend and the configured thinking for either.
	Planner func(backend string) (planner.Planner, error)
	// Engine configures every engine. The zero value means
	// engine.DefaultConfig.
	Engine engine.Config
	// Logger receives mission starts and ends and the failures no endpoint
	// answers. Nil discards them.
	Logger *slog.Logger
}

// Daemon is keeld: the tick loop and the operator's Service over one fleet.
//
// It holds two kinds of engine, never at once. Between missions an idle
// engine, unrecorded, follows the fleet so the operator sees it before any
// plan exists; its view carries the zero head of an empty chain. An approved
// plan starts a fresh engine recording into the mission's own log, whose
// first batch joins every vector with its latest frame and approves the plan:
// the layout keelsim writes, so both replay alike. When the mission ends its
// log is closed and the idle engine resumes.
type Daemon struct {
	cfg      *Config
	world    *planir.World
	packs    *doctrine.Registry
	fleet    *Fleet
	hub      *transport.Hub
	pacer    pacer.Pacer
	planners func(string) (planner.Planner, error)
	engine   engine.Config
	log      *slog.Logger

	// clockMu serialises the changes of pace.
	clockMu sync.Mutex

	mu sync.Mutex
	// inbox holds the operator's events for the next tick: faults, swaps.
	inbox []domain.Event
	// start is the mission approved and not started yet.
	start *approval
	// running is the mission approved, until it ends.
	running  domain.MissionID
	view     transport.MissionView
	finished map[domain.MissionID]transport.MissionView
	pending  []domain.ApprovedPlan // oldest first
	next     int                   // the number of the next mission
	stopping bool

	// Owned by the loop.
	cur    *run
	prev   engine.State // the state last published, whichever engine
	latest map[domain.VectorID]sighting
}

// approval is a mission ready to start: its plan, its seed, its open log.
type approval struct {
	plan domain.ApprovedPlan
	seed uint64
	log  *eventlog.Log
}

// run is one engine's lifetime.
type run struct {
	state   engine.State
	joined  map[domain.VectorID]bool
	log     *eventlog.Log // nil for the idle engine
	mission domain.MissionID
}

// sighting is the fleet's last word on a vector: what it declares, its last
// frame, the adapter's verdict on its link.
type sighting struct {
	caps  domain.Capabilities
	frame *domain.VectorState
	link  domain.LinkState
}

// New prepares a daemon. Missions are numbered after the highest already in
// data_dir/missions, so a restart never reuses a log.
func New(o Options) (*Daemon, error) {
	if o.Config == nil || o.World == nil || o.Packs == nil || o.Fleet == nil || o.Hub == nil {
		return nil, errors.New("daemon: configuration, world, packs, fleet and hub are required")
	}
	d := &Daemon{
		cfg:      o.Config,
		world:    o.World,
		packs:    o.Packs,
		fleet:    o.Fleet,
		hub:      o.Hub,
		pacer:    o.Pacer,
		planners: o.Planner,
		engine:   o.Engine,
		log:      o.Logger,
		finished: map[domain.MissionID]transport.MissionView{},
		latest:   map[domain.VectorID]sighting{},
	}
	if d.pacer == nil {
		rt, err := pacer.NewRealTime(1)
		if err != nil {
			return nil, err
		}
		d.pacer = rt
	}
	if d.planners == nil {
		pc := o.Config.Planner
		d.planners = func(backend string) (planner.Planner, error) {
			o := planner.BackendOptions{Model: pc.Model, OllamaURL: pc.OllamaURL, Think: pc.Think}
			if backend != pc.Backend {
				// The configured model belongs to the configured backend.
				o.Model = ""
			}
			return planner.NewBackend(backend, o)
		}
	}
	if d.engine == (engine.Config{}) {
		d.engine = engine.DefaultConfig()
	}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	missions := d.missionsDir()
	if err := os.MkdirAll(missions, 0o755); err != nil {
		return nil, fmt.Errorf("daemon: data directory: %w", err)
	}
	entries, err := os.ReadDir(missions)
	if err != nil {
		return nil, fmt.Errorf("daemon: data directory: %w", err)
	}
	for _, e := range entries {
		if m := missionPattern.FindStringSubmatch(e.Name()); m != nil && e.IsDir() {
			n, _ := strconv.Atoi(m[1])
			d.next = max(d.next, n)
		}
	}
	d.next++
	d.cur = d.idle()
	d.view = transport.NewMissionView(d.cur.state, eventlog.ZeroDigest)
	return d, nil
}

func (d *Daemon) missionsDir() string { return filepath.Join(d.cfg.DataDir, "missions") }

func (d *Daemon) idle() *run {
	return &run{state: engine.NewState(0, d.engine, d.packs), joined: map[domain.VectorID]bool{}}
}

// Run ticks until the context ends. A mission still running then gets one
// last tick recording an operator_abort, which stops every vector it set in
// motion (spec section 4.5), and its log is closed. Run returns nil after a
// clean shutdown.
func (d *Daemon) Run(ctx context.Context) error {
	var fatal error
	for tick := int64(1); ; tick++ {
		if err := d.pacer.Wait(ctx, tick); err != nil {
			break
		}
		if _, err := d.tick(); err != nil {
			fatal = err
			break
		}
	}
	return errors.Join(fatal, d.shutdown())
}

// tick runs one engine step: the fleet drained into events, the operator's
// events, Step, the log, the commands dispatched, the tick published.
func (d *Daemon) tick(extra ...domain.Event) ([]domain.Command, error) {
	reports := d.fleet.Drain()
	for _, r := range reports {
		s := d.latest[r.Vector]
		s.caps, s.link = r.Caps, r.Link
		if r.Frame != nil {
			s.frame = r.Frame
		}
		d.latest[r.Vector] = s
	}

	d.mu.Lock()
	start, ops := d.start, d.inbox
	d.start, d.inbox = nil, nil
	d.mu.Unlock()

	var batch []domain.Event
	if start != nil {
		d.cur = &run{
			state:   engine.NewState(start.seed, d.engine, d.packs),
			joined:  map[domain.VectorID]bool{},
			log:     start.log,
			mission: start.plan.Mission,
		}
		batch = d.events(nil, true)
		plan := start.plan
		batch = append(batch, domain.Event{Kind: domain.EventPlanApproved, Plan: &plan})
	} else {
		batch = d.events(reports, false)
	}
	batch = append(append(batch, ops...), extra...)

	next, cmds, decs := engine.Step(d.cur.state, batch)
	d.cur.state = next
	head := eventlog.ZeroDigest
	var recordErr error
	if d.cur.log != nil {
		if recordErr = missionlog.AppendTick(d.cur.log, next, batch, decs, cmds); recordErr == nil {
			head = d.cur.log.Head()
		}
	}
	// Dispatched even when recording failed: the commands may be the stop
	// the vectors need.
	d.fleet.Dispatch(cmds)
	if recordErr != nil {
		return cmds, fmt.Errorf("daemon: recording mission %s: %w", d.cur.mission, recordErr)
	}

	view := transport.NewMissionView(next, head)
	// Faster than real time, the stream carries telemetry and tick frames
	// for one tick in speed, about ten a second of wall clock, and every tick
	// the mission changes state (spec section 8.2).
	speed := d.speed()
	sampled := transport.Sampled(next.Clock.Tick, int64(speed)) || d.prev.Mission.ID != next.Mission.ID || d.prev.Mission.State != next.Mission.State
	msgs, _ := transport.ThinTick(transport.TickMessages(d.prev, next, batch, decs, head), sampled)
	transport.StampSpeed(msgs, speed)
	live := view
	live.Speed = speed
	if err := d.hub.Publish(next.Clock.TickMs, live, msgs...); err != nil && !errors.Is(err, transport.ErrHubClosed) {
		d.log.Error("publishing a tick", "tick", next.Clock.Tick, "error", err)
	}
	d.prev = next
	d.mu.Lock()
	d.view = view
	d.mu.Unlock()

	if d.cur.log != nil && next.Mission.State != domain.MissionRunning {
		d.retire(view)
	}
	return cmds, nil
}

// events turns a drain into the current engine's events. A vector the engine
// has not seen joins with its latest frame, the link set to the adapter's
// verdict; a joined one sends its new frame and any change of verdict. With
// all, as when an engine starts, every vector with a frame joins.
func (d *Daemon) events(reports []Report, all bool) []domain.Event {
	ids := make([]domain.VectorID, 0, len(reports))
	changed := map[domain.VectorID]Report{}
	if all {
		ids = engine.SortedKeys(d.latest)
	}
	for _, r := range reports {
		ids = append(ids, r.Vector)
		changed[r.Vector] = r
	}
	var out []domain.Event
	for _, id := range ids {
		s := d.latest[id]
		if s.frame == nil {
			continue
		}
		if !d.cur.joined[id] {
			caps, st := s.caps.Clone(), *s.frame
			st.Link = s.link
			out = append(out, domain.Event{Kind: domain.EventVectorJoined, Vector: id, Caps: &caps, Telemetry: &st})
			d.cur.joined[id] = true
			continue
		}
		r := changed[id]
		if r.Frame != nil {
			st := *r.Frame
			out = append(out, domain.Event{Kind: domain.EventTelemetry, Vector: id, Telemetry: &st})
		}
		if r.LinkChanged {
			out = append(out, domain.Event{Kind: domain.EventLinkChanged, Vector: id, Link: r.Link})
		}
	}
	return out
}

// retire closes the ended mission's log, keeps its final view, and hands the
// fleet back to an idle engine.
func (d *Daemon) retire(view transport.MissionView) {
	id := d.cur.mission
	if err := d.cur.log.Close(); err != nil {
		d.log.Error("closing a mission log", "mission", id, "error", err)
	}
	started := view.Mission.ID == id
	d.mu.Lock()
	if started {
		d.finished[id] = view
	}
	d.running = ""
	d.mu.Unlock()
	if started {
		d.log.Info("mission ended", "mission", id, "state", view.Mission.State, "explored", view.Explored, "total", view.Total, "head", view.Head)
	} else {
		d.log.Warn("approval refused by the engine", "mission", id)
	}
	d.cur = d.idle()
}

// shutdown stops a running mission through the engine, waits a bounded time
// for the stopped vectors to acknowledge, and closes its log. An approved
// mission that never started has its log removed: it holds a header only.
func (d *Daemon) shutdown() error {
	d.mu.Lock()
	d.stopping = true
	start := d.start
	d.start = nil
	d.mu.Unlock()
	var errs []error
	if start != nil {
		errs = append(errs, start.log.Close(), os.RemoveAll(start.log.Dir()))
		d.mu.Lock()
		d.running = ""
		d.mu.Unlock()
	}
	if d.cur.log == nil {
		return errors.Join(errs...)
	}
	id := d.cur.mission
	cmds, err := d.tick(domain.Event{Kind: domain.EventOperatorAbort, Reason: shutdownReason})
	errs = append(errs, err)
	d.log.Info("mission aborted on shutdown", "mission", id, "stopped", len(cmds))
	d.awaitAcks(cmds)
	if d.cur.log != nil {
		// Recording failed: the mission never settled.
		errs = append(errs, d.cur.log.Close())
	}
	return errors.Join(errs...)
}

// awaitAcks waits, at most ackWait, until every vector sent an abort reports
// it applied.
func (d *Daemon) awaitAcks(cmds []domain.Command) {
	want := map[domain.VectorID]uint64{}
	for _, c := range cmds {
		if c.Type == domain.CommandAbort {
			want[c.Vector] = c.Seq
		}
	}
	deadline := time.Now().Add(ackWait)
	for len(want) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Duration(domain.TickIntervalMs) * time.Millisecond)
		for _, r := range d.fleet.Drain() {
			if seq, ok := want[r.Vector]; ok && r.Frame != nil && r.Frame.AckSeq >= seq {
				delete(want, r.Vector)
			}
		}
	}
	for _, id := range engine.SortedKeys(want) {
		d.log.Warn("vector did not acknowledge its stop before shutdown", "vector", id, "seq", want[id])
	}
}

// newSeed is the configured seed, or one drawn for the mission.
func (d *Daemon) newSeed() uint64 {
	if d.cfg.Seed != nil {
		return *d.cfg.Seed
	}
	var b [8]byte
	_, _ = crand.Read(b[:])
	return binary.LittleEndian.Uint64(b[:])
}

// openMission creates the mission's log directory, which must not exist, and
// records its header.
func (d *Daemon) openMission(plan domain.ApprovedPlan, seed uint64) (*eventlog.Log, error) {
	dir := filepath.Join(d.missionsDir(), string(plan.Mission))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, fmt.Errorf("daemon: mission log: %w", err)
	}
	l, err := eventlog.Open(dir, eventlog.Options{})
	if err != nil {
		return nil, err
	}
	err = missionlog.AppendHeader(l, missionlog.Header{
		Name:    d.cfg.Name,
		Seed:    seed,
		Config:  d.engine,
		Plan:    plan.Hash,
		Mission: plan.Mission,
	})
	if err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// missionID formats the n-th mission's id.
func missionID(n int) domain.MissionID { return domain.MissionID(fmt.Sprintf("MSN-%03d", n)) }
