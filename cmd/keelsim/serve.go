package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsanchez31/keel/adapters/native"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/httpjson"
	"github.com/dsanchez31/keel/internal/pacer"
	"github.com/dsanchez31/keel/internal/world"
	"github.com/dsanchez31/keel/vector"
)

// DefaultListen is where --serve listens unless told otherwise: loopback,
// since the native protocol carries no authentication (spec section 1) and a
// fleet reachable from the network would take commands from anyone.
const DefaultListen = "127.0.0.1:8090"

// FaultPath is where --serve takes faults, beside the vehicles: the path by
// which an orchestrator's fault injection reaches the simulated world.
const FaultPath = "/v1/faults"

// ClockPath is where --serve takes its pace, beside the faults: the path by
// which an orchestrator running faster than real time brings the world along,
// so that no vehicle flies faster than its declared cruise speed in either's
// mission time.
const ClockPath = "/v1/clock"

// MaxPendingFaults bounds the faults accepted and not yet injected. A tick
// drains them all, so only a client posting faster than ten per 100 ms tick
// reaches it.
const MaxPendingFaults = 64

// shutdownTimeout bounds the HTTP server's graceful stop.
const shutdownTimeout = 5 * time.Second

// Serve is a scenario's world with no engine: each vehicle served over the
// native protocol to whichever orchestrator dials it (spec section 15.4).
//
// One tick of the loop, the closed loop's order without the engine:
//
//  1. wait for the pacer
//  2. put the commands the vehicles received since the last tick on the air,
//     through the simulated radio
//  3. inject every scheduled fault now due, then every fault posted to
//     FaultPath since the last tick, in arrival order
//  4. world.Step, and send each vehicle the frames of it that reached the
//     ground
//
// The radio stays in the loop: a command or a frame the world loses is lost,
// whatever the WebSocket underneath delivered.
type Serve struct {
	World *world.World
	// Vehicles are the served vehicles, sorted by id.
	Vehicles []*native.Vehicle
	// Faults are time-triggered only: coverage is the engine's to observe,
	// and the engine runs elsewhere.
	Faults []world.ScheduledFault
	// Pacer paces the ticks. A pacer.Scalable one takes its speed on
	// ClockPath.
	Pacer pacer.Pacer
	// OnFault, if set, is called with each fault as it is injected.
	OnFault func(nowMs int64, f domain.Fault)

	// mu guards posted: the faults accepted on FaultPath, not yet injected.
	mu     sync.Mutex
	posted []domain.Fault
}

// Run serves until the context ends, returning its error.
func (s *Serve) Run(ctx context.Context) error {
	pending := slices.Clone(s.Faults)
	for tick := int64(1); ; tick++ {
		if err := s.Pacer.Wait(ctx, tick); err != nil {
			return err
		}
		s.World.Send(domain.SortCommands(s.received()))
		var due []domain.Fault
		kept := pending[:0]
		for _, f := range pending {
			if f.Due(s.World.NowMs()+engine.TickIntervalMs, 0) {
				due = append(due, f.Fault)
			} else {
				kept = append(kept, f)
			}
		}
		pending = kept
		for _, f := range append(due, s.takePosted()...) {
			// A posted fault was held against Fault.Check and the fleet when
			// accepted, a scheduled one when the scenario loaded: a refusal
			// here is a bug.
			if err := s.World.Inject(f); err != nil {
				return fmt.Errorf("injecting %s on %s: %w", f.Kind, f.Vector, err)
			}
			if s.OnFault != nil {
				s.OnFault(s.World.NowMs(), f)
			}
		}
		for _, ev := range s.World.Step() {
			if ev.Kind != domain.EventTelemetry || ev.Telemetry == nil {
				continue
			}
			frame := *ev.Telemetry
			// keelsim's mission clock started when it did, keeld's when keeld
			// did: the engine stamps the tick it accepts the frame on (spec
			// section 4.2), as it does for an autopilot that knows no mission
			// time.
			frame.LastSeenMs = 0
			v := s.vehicle(frame.ID)
			if v == nil {
				continue
			}
			if err := v.Send(frame); err != nil && !errors.Is(err, vector.ErrClosed) {
				return fmt.Errorf("serving a frame of %s: %w", frame.ID, err)
			}
		}
	}
}

// received drains every vehicle's command channel without waiting, in id
// order.
func (s *Serve) received() []domain.Command {
	var out []domain.Command
	for _, v := range s.Vehicles {
		for more := true; more; {
			select {
			case c, ok := <-v.Commands():
				if !ok {
					more = false
					break
				}
				out = append(out, c)
			default:
				more = false
			}
		}
	}
	return out
}

func (s *Serve) vehicle(id domain.VectorID) *native.Vehicle {
	i, ok := slices.BinarySearchFunc(s.Vehicles, id, func(v *native.Vehicle, id domain.VectorID) int {
		return strings.Compare(string(v.ID()), string(id))
	})
	if !ok {
		return nil
	}
	return s.Vehicles[i]
}

// Handler serves POST FaultPath, GET and PUT ClockPath, and hands every
// other request to vehicles, the native server.
//
// Like the vehicle socket beside it, the endpoint carries no authentication
// (spec section 1). A browser page of another origin is refused the POST by
// net/http's CrossOriginProtection, and the body must be application/json,
// which such a page cannot send without a CORS preflight this server never
// grants: the same line keeld's REST side draws. An orchestrator calling from
// its own process sends neither Sec-Fetch-Site nor Origin, and passes.
func (s *Serve) Handler(vehicles http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+FaultPath, s.postFault)
	// Without it any other method would fall through to the native server's
	// 404, naming a path that exists.
	mux.HandleFunc(FaultPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", http.MethodPost)
		httpjson.WriteProblem(w, http.StatusMethodNotAllowed, "faults are posted")
	})
	mux.HandleFunc("GET "+ClockPath, s.getClock)
	mux.HandleFunc("PUT "+ClockPath, s.putClock)
	mux.HandleFunc(ClockPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "GET, PUT")
		httpjson.WriteProblem(w, http.StatusMethodNotAllowed, "the clock is read or put")
	})
	mux.Handle("/", vehicles)
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpjson.WriteProblem(w, http.StatusForbidden, "cross-origin request refused: the origin is not trusted")
	}))
	return cop.Handler(mux)
}

// postFault accepts a fault for the next tick: 202 once queued, 422 when
// Fault.Check refuses it or its vector is not in the fleet, 503 when
// MaxPendingFaults are already waiting. It is checked here rather than at the
// tick, so the caller learns of a refusal from its own answer.
func (s *Serve) postFault(w http.ResponseWriter, r *http.Request) {
	f, ok := httpjson.ReadBody[domain.Fault](w, r)
	if !ok {
		return
	}
	if err := f.Check(); err != nil {
		httpjson.WriteProblem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if s.vehicle(f.Vector) == nil {
		httpjson.WriteProblem(w, http.StatusUnprocessableEntity, fmt.Sprintf("vector %s is not in the fleet", f.Vector))
		return
	}
	s.mu.Lock()
	full := len(s.posted) >= MaxPendingFaults
	if !full {
		s.posted = append(s.posted, f)
	}
	s.mu.Unlock()
	if full {
		w.Header().Set("Retry-After", "1")
		httpjson.WriteProblem(w, http.StatusServiceUnavailable, fmt.Sprintf("%d faults already wait for the next tick", MaxPendingFaults))
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// getClock answers the world's pace.
func (s *Serve) getClock(w http.ResponseWriter, _ *http.Request) {
	speed := 1.0
	if p, ok := s.Pacer.(pacer.Scalable); ok {
		speed = p.Speed()
	}
	httpjson.WriteJSON(w, http.StatusOK, map[string]float64{"speed": speed})
}

// putClock sets the world's pace from the next tick on: 200 with the speed
// set, 422 for a speed outside [1, pacer.MaxLiveSpeed], 409 when the loop is
// not paced in real time. Putting the speed in force already changes nothing.
func (s *Serve) putClock(w http.ResponseWriter, r *http.Request) {
	c, ok := httpjson.ReadBody[pacer.Setting](w, r)
	if !ok {
		return
	}
	if err := pacer.CheckLiveSpeed(c.Speed); err != nil {
		httpjson.WriteProblem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	p, ok := s.Pacer.(pacer.Scalable)
	if !ok {
		httpjson.WriteProblem(w, http.StatusConflict, "the world is not paced in real time")
		return
	}
	if err := p.SetSpeed(float64(c.Speed)); err != nil {
		httpjson.WriteProblem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	httpjson.WriteJSON(w, http.StatusOK, c)
}

// takePosted returns the faults posted since the last call, in arrival order.
func (s *Serve) takePosted() []domain.Fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.posted
	s.posted = nil
	return out
}

// loadServe builds the serve loop of a scenario: its world, one vehicle per
// fleet vector on a native server, the time-triggered faults. It returns the
// coverage-triggered faults it leaves out, for the caller to say so.
func loadServe(scenarioPath string) (*Serve, *native.Server, []world.ScheduledFault, error) {
	s, err := readScenario(scenarioPath)
	if err != nil {
		return nil, nil, nil, err
	}
	sim, err := s.simulate()
	if err != nil {
		return nil, nil, nil, err
	}
	srv, err := native.NewServer(native.ServerOptions{})
	if err != nil {
		return nil, nil, nil, err
	}
	sv := &Serve{World: sim}
	for _, v := range s.world.Fleet {
		// The world executes all four command types (spec section 15.2).
		nv, err := srv.Add(v.Caps, vector.CommandTypes())
		if err != nil {
			_ = srv.Close()
			return nil, nil, nil, err
		}
		sv.Vehicles = append(sv.Vehicles, nv)
	}
	slices.SortFunc(sv.Vehicles, func(a, b *native.Vehicle) int { return strings.Compare(string(a.ID()), string(b.ID())) })
	var skipped []world.ScheduledFault
	for _, f := range s.sc.Faults {
		if f.Trigger == world.TriggerCoverage {
			skipped = append(skipped, f)
			continue
		}
		sv.Faults = append(sv.Faults, f)
	}
	return sv, srv, skipped, nil
}

// checkServeFlags refuses the closed loop's options that --serve cannot
// honour, rather than ignoring them.
func checkServeFlags(cmd *cobra.Command, opts options) error {
	switch {
	case cmd.Flags().Changed("out"):
		return errors.New("--out records the closed loop's log: with --serve the engine, and the log, are keeld's")
	case cmd.Flags().Changed("max-ticks"):
		return errors.New("--max-ticks bounds the closed loop's mission: --serve runs until interrupted")
	case opts.speed != 1:
		// The orchestrator sets the pace on ClockPath, starting in real time:
		// a world starting faster would fly every vehicle faster than the
		// cruise speed it declares.
		return fmt.Errorf("--speed %v: --serve starts in real time, its orchestrator sets the pace on %s", opts.speed, ClockPath)
	}
	return nil
}

// serveFleet serves the scenario's fleet on opts.listen until the context
// ends.
func serveFleet(ctx context.Context, out io.Writer, opts options) error {
	sv, srv, skipped, err := loadServe(opts.scenario)
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()
	if sv.Pacer, err = pacer.NewRealTime(1); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", opts.listen)
	if err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	hs := &http.Server{Handler: sv.Handler(srv), ReadHeaderTimeout: 5 * time.Second}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	served := make(chan error, 1)
	go func() {
		err := hs.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		served <- err
		// A server that fails stops the loop; one closed by the shutdown
		// below finds it stopped already.
		cancel()
	}()

	ids := make([]string, 0, len(sv.Vehicles))
	for _, v := range sv.Vehicles {
		ids = append(ids, string(v.ID()))
	}
	faultURL := "http://" + ln.Addr().String() + FaultPath
	_, _ = fmt.Fprintf(out, "serving %d vectors on ws://%s%s: %s\n", len(ids), ln.Addr(), native.VectorPath, strings.Join(ids, ", "))
	_, _ = fmt.Fprintf(out, "taking faults on POST %s\n", faultURL)
	_, _ = fmt.Fprintf(out, "taking the pace on PUT http://%s%s\n", ln.Addr(), ClockPath)
	for _, f := range skipped {
		_, _ = fmt.Fprintf(out, "skipped: %s on %s at %v %% coverage, coverage is the engine's to observe: post it to %s when due\n",
			f.Fault.Kind, f.Fault.Vector, f.AtCoverage, faultURL)
	}
	if !opts.quiet {
		sv.OnFault = func(nowMs int64, f domain.Fault) {
			_, _ = fmt.Fprintf(out, "%9.1fs  fault  %s on %s\n", float64(nowMs)/1000, f.Kind, f.Vector)
		}
	}

	runErr := sv.Run(ctx)
	// The vehicles' WebSockets are hijacked connections the HTTP server no
	// longer tracks: closing the native server drops them, the shutdown then
	// stops the listener.
	_ = srv.Close()
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer scancel()
	_ = hs.Shutdown(sctx)
	if err := <-served; err != nil {
		return fmt.Errorf("serving on %s: %w", ln.Addr(), err)
	}
	if errors.Is(runErr, context.Canceled) {
		_, _ = fmt.Fprintf(out, "stopped after %.1f s of mission time\n", float64(sv.World.NowMs())/1000)
		return nil
	}
	return runErr
}
