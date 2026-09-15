// Command keelsim runs a simulated mission end to end: the engine below the
// fence, the simulated world it commands, the plan an operator approved and
// the faults a scenario injects. It records the hash-chained log and prints
// its head, headless-fast by default and in real time on request, through
// one loop.
//
// With --serve, keelsim is the fleet and not the loop: the scenario's world
// alone, in real time, each vehicle served over the native protocol for an
// orchestrator to dial, and faults taken over HTTP beside them (serve.go,
// spec section 15.4).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dsanchez31/keel/internal/buildinfo"
	"github.com/dsanchez31/keel/internal/coverage"
	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/files"
	"github.com/dsanchez31/keel/internal/pacer"
	"github.com/dsanchez31/keel/internal/planir"
	"github.com/dsanchez31/keel/internal/world"
)

// Exit codes. A mission that runs and does not complete is an outcome, not
// an error, so it has its own code.
const (
	exitOK         = 0
	exitError      = 1
	exitIncomplete = 2
	programName    = "keelsim"
)

// errIncomplete reports a run that ended without completing its mission.
var errIncomplete = errors.New("mission did not complete")

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes one command line and returns the process exit code. It is the
// whole program minus os.Exit, so tests drive it directly.
func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := newRootCmd(stdout, stderr)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errIncomplete):
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitIncomplete
	default:
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitError
	}
}

type options struct {
	scenario    string
	doctrineDir string
	realtime    bool
	speed       float64
	out         string
	maxTicks    int64
	quiet       bool
	serve       bool
	listen      string
}

func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts options
	cmd := &cobra.Command{
		Use:   programName,
		Short: "Run a simulated mission through the engine and record it",
		Long: `Run a simulated mission through the engine and record it.

The scenario names a world, a Plan IR and the physics and faults of the run.
The plan is validated through the four gates as keelctl would, approved under
the scenario's mission id, and flown by the simulated fleet. Every event,
decision and command goes into the hash-chained log, whose head is printed at
the end: two runs of one scenario print the same head, fast or in real time.

With --serve, keelsim runs no engine: the scenario's world runs alone in real
time and serves each vehicle over the native protocol at
ws://LISTEN/v1/vectors/{id}, for keeld to drive, and takes faults as JSON on
POST http://LISTEN/v1/faults, injected at the next tick. It runs until
interrupted.

Exit status: 0 the mission completed (or --serve was interrupted), 2 it failed
or did not finish, 1 any other error.`,
		Example: `  keelsim --scenario examples/sims/reference.yaml
  keelsim --scenario examples/sims/reference.yaml --realtime --speed 10 --out /tmp/run-042
  keelsim --scenario examples/sims/reference.yaml --serve --listen 127.0.0.1:8090`,
		Version:       buildinfo.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.serve {
				if err := checkServeFlags(cmd, opts); err != nil {
					return err
				}
				return serveFleet(cmd.Context(), cmd.OutOrStdout(), opts)
			}
			return simulate(cmd.Context(), cmd.OutOrStdout(), opts)
		},
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	f := cmd.Flags()
	f.StringVar(&opts.scenario, "scenario", "examples/sims/reference.yaml", "keel.sim/v1 scenario to run")
	f.StringVar(&opts.doctrineDir, "doctrine-dir", "doctrine-packs", "directory of doctrine packs the plan and hot swaps resolve against")
	f.BoolVar(&opts.realtime, "realtime", false, "pace ticks on the wall clock instead of running headless-fast")
	f.Float64Var(&opts.speed, "speed", 1, "with --realtime, how many times faster than real time")
	f.StringVar(&opts.out, "out", "", "directory to write the log to; the log is kept in memory when empty")
	f.Int64Var(&opts.maxTicks, "max-ticks", engine.DefaultConfig().MaxTicks, "tick ceiling: reaching it fails the mission")
	f.BoolVar(&opts.quiet, "quiet", false, "print the summary only, not every decision")
	f.BoolVar(&opts.serve, "serve", false, "serve the fleet over the native protocol instead of running the engine")
	f.StringVar(&opts.listen, "listen", DefaultListen, "with --serve, the address to serve the vehicles, the fault and the clock endpoints on")
	return cmd
}

func simulate(ctx context.Context, out io.Writer, opts options) error {
	if opts.maxTicks <= 0 {
		return fmt.Errorf("--max-ticks %d is not positive", opts.maxTicks)
	}
	r, err := load(opts.scenario, opts.doctrineDir, opts.maxTicks)
	if err != nil {
		return err
	}
	r.Pacer = pacer.Fast{}
	if opts.realtime {
		if r.Pacer, err = pacer.NewRealTime(opts.speed); err != nil {
			return fmt.Errorf("--speed: %w", err)
		}
	}
	if opts.out != "" {
		l, err := eventlog.Open(opts.out, eventlog.Options{})
		if err != nil {
			return fmt.Errorf("opening the log: %w", err)
		}
		defer func() { _ = l.Close() }()
		if l.Seq() > 0 {
			return fmt.Errorf("%s already holds a log, a run records into an empty directory", opts.out)
		}
		r.Log = l
	}
	if !opts.quiet {
		r.OnTick = func(tr engine.TickResult) {
			for _, d := range tr.Decisions {
				_, _ = fmt.Fprintf(out, "%9.1fs  %-15s  %-12s  %s\n", float64(d.TickMs)/1000, d.Kind, d.Subject, d.Rationale)
			}
		}
	}

	sum, err := r.Execute(ctx)
	printSummary(out, r, sum, opts.out)
	if errors.Is(err, context.Canceled) {
		// An interrupted run did not finish: an outcome, as the tick ceiling
		// is, not an error.
		return fmt.Errorf("%w: interrupted after %d ticks", errIncomplete, sum.Ticks)
	}
	if err != nil {
		return err
	}
	if sum.State != domain.MissionComplete {
		return fmt.Errorf("%w: %s after %d ticks", errIncomplete, sum.State, sum.Ticks)
	}
	return nil
}

func printSummary(out io.Writer, r *Run, sum Summary, dir string) {
	pct := 0.0
	if sum.Total > 0 {
		pct = float64(sum.Explored) * 100 / float64(sum.Total)
	}
	var faults []string
	for _, f := range sum.Injected {
		faults = append(faults, fmt.Sprintf("%s on %s", f.Kind, f.Vector))
	}
	if len(faults) == 0 {
		faults = []string{"none"}
	}
	where := "in memory"
	if dir != "" {
		where = dir
	}
	_, _ = fmt.Fprintf(out, `
scenario   %s
plan       %s
mission    %s, %s after %.1f s of mission time (%d ticks)
coverage   %d of %d cells, %.1f %%
faults     %s
recorded   %d decisions, %d commands, %s
head       %s
`, r.Name, r.Plan.Hash, r.Plan.Mission, sum.State, float64(sum.MissionMs)/1000, sum.Ticks,
		sum.Explored, sum.Total, pct, strings.Join(faults, ", "), sum.Decisions, sum.Commands, where, sum.Head)
}

// scenario is a scenario file with the world it names, read and resolved.
type scenario struct {
	path      string
	sc        *world.Scenario
	world     *planir.World
	worldPath string
}

// readScenario parses a scenario file and the world file it names.
func readScenario(scenarioPath string) (scenario, error) {
	src, err := os.ReadFile(scenarioPath)
	if err != nil {
		return scenario{}, fmt.Errorf("reading the scenario: %w", err)
	}
	sc, err := world.ParseScenario(src)
	if err != nil {
		return scenario{}, fmt.Errorf("%s: %w", scenarioPath, err)
	}
	s := scenario{path: scenarioPath, sc: sc}
	s.worldPath = s.resolve(sc.WorldPath)
	if s.world, err = files.ReadWorld(s.worldPath); err != nil {
		return scenario{}, err
	}
	return s, nil
}

// resolve reads a path of the scenario relative to the scenario file, not to
// wherever keelsim was started, so a scenario runs the same from any
// directory.
func (s scenario) resolve(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(filepath.Dir(s.path), p)
}

// simulate builds the seeded world of the scenario's fleet, and checks that
// every fault names a vector of it.
func (s scenario) simulate() (*world.World, error) {
	var fleet []world.Vehicle
	for _, v := range s.world.Fleet {
		fleet = append(fleet, world.Vehicle{Caps: v.Caps, State: v.State})
	}
	var extent []domain.Position
	for _, a := range s.world.Areas {
		extent = append(extent, a.Area.Polygon.Ring...)
	}
	sim, err := world.New(world.Config{Seed: s.sc.Seed, Physics: s.sc.Physics, Stations: s.world.Stations, Fleet: fleet, Extent: extent})
	if err != nil {
		return nil, err
	}
	for _, f := range s.sc.Faults {
		if _, ok := sim.Truth(f.Fault.Vector); !ok {
			return nil, fmt.Errorf("%s: a fault names %s, which is not in the fleet of %s", s.path, f.Fault.Vector, s.worldPath)
		}
	}
	return sim, nil
}

// load builds a run from a scenario file: the world and the Plan IR it names,
// resolved relative to it, the doctrine packs of doctrineDir, the plan
// validated and approved under the scenario's mission id, the simulated world
// and the engine state, both seeded by the scenario.
func load(scenarioPath, doctrineDir string, maxTicks int64) (*Run, error) {
	s, err := readScenario(scenarioPath)
	if err != nil {
		return nil, err
	}
	sc := s.sc
	packs, err := files.ReadPacks(doctrineDir)
	if err != nil {
		return nil, err
	}
	cfg := engine.DefaultConfig()
	cfg.MaxTicks = maxTicks
	plan, err := approve(s.resolve(sc.PlanPath), s.world, packs, sc.Mission, cfg.Arrival())
	if err != nil {
		return nil, err
	}
	sim, err := s.simulate()
	if err != nil {
		return nil, err
	}

	return &Run{
		Name:     sc.Name,
		State:    engine.NewState(sc.Seed, cfg, packs),
		World:    sim,
		Plan:     plan,
		Faults:   sc.Faults,
		Log:      eventlog.NewMemLog(),
		Pacer:    pacer.Fast{},
		MaxTicks: maxTicks,
	}, nil
}

// approve validates a Plan IR file through the four gates, as keelctl plan
// compile would, against the arrival budget of the engine that will run it,
// and stamps the mission id the human gate assigns. The
// intent checked is the file's own: a scenario replays a plan, it does not
// compile one.
func approve(path string, w *planir.World, packs *doctrine.Registry, mission domain.MissionID, arrival coverage.Arrival) (domain.ApprovedPlan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.ApprovedPlan{}, fmt.Errorf("reading the plan: %w", err)
	}
	var head struct {
		Intent string `json:"intent"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return domain.ApprovedPlan{}, fmt.Errorf("%s: %w", path, err)
	}
	res := planir.Validate(planir.Input{Intent: head.Intent, Raw: raw, World: w, Doctrines: packs, Arrival: arrival})
	if !res.OK() {
		var b strings.Builder
		for _, d := range res.Diagnostics {
			fmt.Fprintf(&b, "\n  %s %s %s: %s", d.Gate, d.Code, d.Pointer, d.Message)
		}
		return domain.ApprovedPlan{}, fmt.Errorf("%s does not validate:%s", path, b.String())
	}
	plan := res.Plan.Clone()
	plan.Mission = mission
	return plan, nil
}
