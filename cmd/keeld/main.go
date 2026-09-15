// Command keeld is the KEEL orchestrator: it binds the fleet its
// configuration names, compiles operator intent into plans, runs the engine
// on the plan an operator approved, records every mission on the hash chain
// and serves the REST API and the stream to the operator (spec sections 8
// and 16). The logic lives in internal/daemon; this is its process.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsanchez31/keel/internal/buildinfo"
	"github.com/dsanchez31/keel/internal/daemon"
	"github.com/dsanchez31/keel/internal/files"
	"github.com/dsanchez31/keel/internal/pacer"
	"github.com/dsanchez31/keel/internal/planner"
	"github.com/dsanchez31/keel/internal/transport"
	"github.com/dsanchez31/keel/vector"
)

const programName = "keeld"

// shutdownTimeout bounds the HTTP server's graceful stop. A compilation
// still running then is cut.
const shutdownTimeout = 5 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes one command line and returns the process exit code: 0 after a
// clean shutdown, 1 on any error. It is the whole program minus os.Exit, so
// tests drive it directly.
func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := newRootCmd(stdout, stderr)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return 1
	}
	return 0
}

type options struct {
	config string
	listen string
}

func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts options
	cmd := &cobra.Command{
		Use:   programName,
		Short: "Run the KEEL orchestrator daemon",
		Long: `Run the KEEL orchestrator daemon.

The configuration (keel.daemon/v1) names the world, the doctrine packs, the
data directory missions are recorded under, and how each vector of the fleet
is reached: the native protocol, or MAVLink. keeld serves the REST API and
the stream on its listen address until interrupted. A mission still running
then is aborted through the engine, which stops every vector it set in
motion, and its log is closed.

The Ollama backend reads OLLAMA_HOST when the configuration names no URL; the
Claude backend reads ANTHROPIC_API_KEY.`,
		Example: `  keelsim --scenario examples/sims/reference.yaml --serve &
  keeld --config examples/keeld/keelsim.yaml`,
		Version:       buildinfo.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), cmd.ErrOrStderr(), opts)
		},
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	f := cmd.Flags()
	f.StringVar(&opts.config, "config", "examples/keeld/keelsim.yaml", "keel.daemon/v1 configuration file")
	f.StringVar(&opts.listen, "listen", "", "address to serve the API on, overriding the configuration's")
	return cmd
}

// serve runs the daemon until the context ends.
//
// Shutdown runs in order: the loop's last tick, which aborts a running
// mission and closes its log; the hub, which closes every stream with 1001;
// the HTTP server's graceful stop; the fleet, which closes every adapter.
func serve(ctx context.Context, logw io.Writer, opts options) error {
	logger := slog.New(slog.NewTextHandler(logw, nil))
	cfg, err := daemon.ReadConfig(opts.config)
	if err != nil {
		return err
	}
	if opts.listen != "" {
		cfg.Listen = opts.listen
	}
	if cfg.Planner.OllamaURL == "" {
		// Ambient input is read here, at the edge, and nowhere below.
		cfg.Planner.OllamaURL = planner.OllamaHostURL(os.Getenv("OLLAMA_HOST"))
	}
	world, err := files.ReadWorld(cfg.WorldPath)
	if err != nil {
		return err
	}
	packs, err := files.ReadPacks(cfg.DoctrineDir)
	if err != nil {
		return err
	}
	// One pacer paces the loop and clocks the fleet's agents, so link health
	// counts in the fleet's time at whatever pace the operator sets.
	pace, err := pacer.NewRealTime(1)
	if err != nil {
		return err
	}
	fleet, err := daemon.NewFleet(cfg, world, daemon.FleetOptions{Logger: logger, Agent: vector.Options{Clock: pace.Now}})
	if err != nil {
		return err
	}
	defer func() { _ = fleet.Close() }()
	hub, err := transport.NewHub(transport.HubOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = hub.Close() }()
	d, err := daemon.New(daemon.Options{Config: cfg, World: world, Packs: packs, Fleet: fleet, Hub: hub, Pacer: pace, Logger: logger})
	if err != nil {
		return err
	}
	handler, err := transport.NewHandler(d, hub, transport.Options{TrustedOrigins: cfg.TrustedOrigins, Logger: logger})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hs := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
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
	logger.Info("keeld serving", "api", "http://"+ln.Addr().String()+"/api/v1", "stream", "ws://"+ln.Addr().String()+transport.StreamPath,
		"config", opts.config, "vectors", len(cfg.Vectors), "data", cfg.DataDir, "version", buildinfo.String())

	runErr := d.Run(ctx)
	_ = hub.Close()
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer scancel()
	if err := hs.Shutdown(sctx); err != nil {
		_ = hs.Close()
	}
	serveErr := <-served
	if serveErr != nil {
		serveErr = fmt.Errorf("serving on %s: %w", ln.Addr(), serveErr)
	}
	logger.Info("keeld stopped")
	return errors.Join(runErr, serveErr)
}
