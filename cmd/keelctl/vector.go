package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bluenviron/gomavlib/v4"
	"github.com/spf13/cobra"

	"github.com/dsanchez31/keel/adapters/mavlink"
	"github.com/dsanchez31/keel/adapters/native"
	"github.com/dsanchez31/keel/internal/conformance"
	"github.com/dsanchez31/keel/internal/files"
	"github.com/dsanchez31/keel/vector"
)

// errNotConformant reports a validation in which a case failed or did not
// run. The report has already been printed when it is returned.
var errNotConformant = errors.New("not conformant")

// target is the vehicle a vector command reaches, and how.
type target struct {
	native  string
	mavlink string
	sysid   uint8
	vector  string
	world   string
	sepM    float64
	// degradedAfter and lostAfter configure the adapter's Health, and what
	// validate's C6 holds it to.
	degradedAfter time.Duration
	lostAfter     time.Duration
}

func (t *target) flags(cmd *cobra.Command) {
	f := cmd.PersistentFlags()
	f.StringVar(&t.native, "native", "", "native vehicle URL, ws://HOST/v1/vectors/ID")
	f.StringVar(&t.mavlink, "mavlink", "", "UDP address a MAVLink vehicle pushes to, as ArduPilot SITL's udpclient (e.g. 127.0.0.1:14550)")
	f.Uint8Var(&t.sysid, "sysid", 1, "with --mavlink, the vehicle's MAVLink system id")
	f.StringVar(&t.vector, "vector", "", "with --mavlink, the fleet entry of --world whose capabilities the vehicle declares")
	f.StringVar(&t.world, "world", "examples/worlds/reference.yaml", "with --mavlink, the world file holding the fleet entry")
	f.Float64Var(&t.sepM, "geoid-separation", 0, "with --mavlink, the geoid separation at the operating area in metres (spec section 7.4)")
	f.DurationVar(&t.degradedAfter, "degraded-after", vector.DefaultDegradedAfter, "silence before the adapter's Health reports degraded")
	f.DurationVar(&t.lostAfter, "lost-after", vector.DefaultLostAfter, "silence before the adapter's Health reports lost")
}

func (t *target) agent() vector.Options {
	return vector.Options{DegradedAfter: t.degradedAfter, LostAfter: t.lostAfter}
}

func (t *target) check() error {
	switch {
	case (t.native == "") == (t.mavlink == ""):
		return errors.New("name the vehicle with exactly one of --native and --mavlink")
	case t.mavlink != "" && t.vector == "":
		return errors.New("--mavlink needs --vector: MAVLink declares no capabilities, the world's fleet entry does")
	}
	return nil
}

// mavlinkConfig is the adapter configuration of a --mavlink target.
func (t *target) mavlinkConfig() (mavlink.Config, error) {
	w, err := files.ReadWorld(t.world)
	if err != nil {
		return mavlink.Config{}, err
	}
	for _, v := range w.Fleet {
		if string(v.Caps.ID) == t.vector {
			return mavlink.Config{SystemID: t.sysid, Capabilities: v.Caps, GeoidSeparationM: t.sepM, Agent: t.agent()}, nil
		}
	}
	return mavlink.Config{}, fmt.Errorf("%s has no fleet entry %q", t.world, t.vector)
}

// open connects to the target directly, without the conformance proxy.
func (t *target) open(ctx context.Context) (vector.Vector, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	if t.native != "" {
		return native.Dial(ctx, t.native, native.Options{Agent: t.agent()})
	}
	cfg, err := t.mavlinkConfig()
	if err != nil {
		return nil, err
	}
	link, err := mavlink.NewLink(mavlink.LinkOptions{Endpoints: []gomavlib.Endpoint{&gomavlib.EndpointUDPServer{Address: t.mavlink}}})
	if err != nil {
		return nil, err
	}
	a, err := mavlink.NewAdapter(link, cfg)
	if err != nil {
		_ = link.Close()
		return nil, err
	}
	return &linkedVector{Adapter: a, link: link}, nil
}

// linkedVector closes its link with the adapter.
type linkedVector struct {
	*mavlink.Adapter
	link *mavlink.Link
}

func (v *linkedVector) Close() error { return errors.Join(v.Adapter.Close(), v.link.Close()) }

// subject is the target as the conformance suite reaches it, through its
// proxy.
func (t *target) subject() (*conformance.Subject, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	if t.native != "" {
		return conformance.NativeSubject(t.native, native.Options{Agent: t.agent()})
	}
	cfg, err := t.mavlinkConfig()
	if err != nil {
		return nil, err
	}
	return conformance.MAVLinkSubject(t.mavlink, cfg)
}

func newVectorCmd() *cobra.Command {
	var t target
	cmd := &cobra.Command{
		Use:   "vector",
		Short: "Connect to a vehicle through its adapter: describe, watch, validate",
		Long: `Connect to one vehicle through its adapter.

A native vehicle is named by its URL (spec section 7.3). A MAVLink vehicle by
the UDP address it pushes to, its system id, and the world fleet entry whose
capabilities it declares, since MAVLink carries none (spec section 7.4).`,
	}
	t.flags(cmd)
	cmd.AddCommand(newVectorDescribeCmd(&t), newVectorPlugCmd(&t), newVectorValidateCmd(&t))
	return cmd
}

func newVectorDescribeCmd(t *target) *cobra.Command {
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "describe",
		Short: "Print a vehicle's capabilities, command types, health and first frame",
		Long: `Print what the adapter knows of the vehicle: its capabilities, the command types
it executes, its health and its first frame.

Exit status: 0 when a frame arrived within --wait, 1 otherwise, the health
saying why.`,
		Example: `  keelctl vector describe --native ws://127.0.0.1:8090/v1/vectors/DRONE-02
  keelctl vector describe --mavlink 127.0.0.1:14550 --sysid 1 --vector DRONE-01`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			v, err := t.open(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = v.Close() }()
			out := cmd.OutOrStdout()
			printCapabilities(out, v)
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case s := <-v.Telemetry():
				_, _ = fmt.Fprintf(out, "frame        %s\n", frameLine(s))
				printHealth(out, v.Health())
				return nil
			case <-timer.C:
				h := v.Health()
				printHealth(out, h)
				return fmt.Errorf("no frame within %v: %s", wait, h.Detail)
			}
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", 15*time.Second, "how long to wait for the first frame")
	return cmd
}

func newVectorPlugCmd(t *target) *cobra.Command {
	return &cobra.Command{
		Use:   "plug",
		Short: "Stream a vehicle's frames and health changes until interrupted",
		Long: `Connect to the vehicle and print every frame and every change of the adapter's
health until interrupted: the first look at a vehicle before validating it.`,
		Example: `  keelctl vector plug --native ws://127.0.0.1:8090/v1/vectors/DRONE-02`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			v, err := t.open(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = v.Close() }()
			out := cmd.OutOrStdout()
			printCapabilities(out, v)
			// Health is checked after every frame, which is when it usually
			// changes, and on a poll, which catches a link going silent.
			poll := time.NewTicker(250 * time.Millisecond)
			defer poll.Stop()
			var last vector.HealthStatus
			reportHealth := func() {
				if h := v.Health(); h.Kind != last.Kind || h.Detail != last.Detail {
					last = h
					_, _ = fmt.Fprintf(out, "%s  health %s %s\n", time.Now().Format("15:04:05.000"), h.Kind, h.Detail)
				}
			}
			for {
				select {
				case <-ctx.Done():
					return nil
				case s, open := <-v.Telemetry():
					if !open {
						return errors.New("the telemetry channel closed")
					}
					_, _ = fmt.Fprintf(out, "%s  %s\n", time.Now().Format("15:04:05.000"), frameLine(s))
					reportHealth()
				case <-poll.C:
					reportHealth()
				}
			}
		},
	}
}

func newVectorValidateCmd(t *target) *cobra.Command {
	var allowMotion bool
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Run the nine conformance cases against a vehicle's adapter",
		Long: `Run the adapter conformance suite (spec section 13) against the vehicle.

The suite sits between the adapter and the vehicle, as a TCP proxy for a native
vehicle and a UDP proxy for a MAVLink one, to withhold telemetry (C6) and cut
the link (C7) without the vehicle's help. For --mavlink, it listens on the
address the vehicle pushes to: nothing else may hold it, keeld included.

C3, C4 and C5 command the vehicle to move, and a landed copter takes off: they
run only with --allow-motion. Without it they are skipped, and a skipped case
is not a pass.

Exit status: 0 when all nine cases passed, 2 when any failed or was skipped,
1 when the suite could not run.`,
		Example: `  keelctl vector validate --native ws://127.0.0.1:8090/v1/vectors/DRONE-02 --allow-motion
  keelctl vector validate --mavlink 127.0.0.1:14550 --sysid 1 --vector DRONE-01 --allow-motion`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			s, err := t.subject()
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "validating %s\n", s.Name)
			rs, err := conformance.Run(ctx, s, conformance.Options{
				AllowMotion:   allowMotion,
				DegradedAfter: t.degradedAfter,
				LostAfter:     t.lostAfter,
				OnResult: func(r conformance.Result) {
					_, _ = fmt.Fprintf(out, "%s  %-24s  %-4s  %s (%v)\n", r.Case, r.Name, r.Status, r.Detail, r.Elapsed)
				},
			})
			if err != nil {
				return err
			}
			var failed, skipped []string
			for _, r := range rs {
				switch r.Status {
				case conformance.Fail:
					failed = append(failed, string(r.Case))
				case conformance.Skip:
					skipped = append(skipped, string(r.Case))
				}
			}
			switch {
			case len(failed) > 0:
				_, _ = fmt.Fprintf(out, "not conformant: %s failed\n", strings.Join(failed, ", "))
				return errNotConformant
			case len(skipped) > 0:
				_, _ = fmt.Fprintf(out, "not conformant: %s skipped, run with --allow-motion\n", strings.Join(skipped, ", "))
				return errNotConformant
			}
			_, _ = fmt.Fprintf(out, "conformant: all %d cases passed\n", len(rs))
			return nil
		},
	}
	cmd.Flags().BoolVar(&allowMotion, "allow-motion", false, "let C3, C4 and C5 command the vehicle to move")
	return cmd
}

func printCapabilities(out io.Writer, v vector.Vector) {
	c := v.Describe()
	_, _ = fmt.Fprintf(out, "vector       %s, %s, tags [%s]\n", c.ID, c.Domain, strings.Join(c.Tags, ", "))
	_, _ = fmt.Fprintf(out, "performance  cruise %g m/s, range %g m, sensor radius %g m\n", c.CruiseSpeed, c.MaxRangeM, c.SensorRadiusM)
	if d, ok := v.(interface{ Supports() []vector.CommandType }); ok {
		var types []string
		for _, t := range d.Supports() {
			types = append(types, string(t))
		}
		_, _ = fmt.Fprintf(out, "supports     %s\n", strings.Join(types, ", "))
	}
}

func printHealth(out io.Writer, h vector.HealthStatus) {
	_, _ = fmt.Fprintf(out, "health       %s %s\n", h.Kind, h.Detail)
}

func frameLine(s vector.VectorState) string {
	return fmt.Sprintf("%-7s %.6f, %.6f  %7.1f m  %5.1f m/s  hdg %5.1f  battery %3d%%  ack %d",
		s.Mode, s.Position.Lat, s.Position.Lon, s.Position.AltM, s.Speed, s.Heading, s.BatteryPct, s.AckSeq)
}
