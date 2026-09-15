package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/files"
	"github.com/dsanchez31/keel/internal/missionlog"
)

// errPacksDiffer reports a diff in which the two packs decided or commanded
// differently on at least one tick. The report has already been printed when
// it is returned.
var errPacksDiffer = errors.New("the packs decide differently")

func newDoctrineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctrine",
		Short: "Compare doctrine packs against recorded missions",
	}
	cmd.AddCommand(newDoctrineDiffCmd())
	return cmd
}

func newDoctrineDiffCmd() *cobra.Command {
	var doctrineDir string
	cmd := &cobra.Command{
		Use:   "diff [flags] <log> <name@version> <name@version>",
		Short: "Replay a mission log under two packs and report the decisions that diverge",
		Long: `Replay a mission log twice in lockstep, the plan approved under the first pack
in one replay and under the second in the other, and print every tick on which
they decide or command differently: "-" for a decision only the first made,
"+" for one only the second made. Hot swaps are replayed as recorded. The log
is a file, a segment directory or - for standard input, as for keelctl replay.

Decisions are compared modulo the starting pack's name: an approval naming its
pack is not a difference. The replay is open loop: the recorded telemetry
answers the commands the recording issued, so after the first divergence the
diff shows what each pack decides on the same inputs, not what the mission
would have done.

Exit status: 0 when the packs decide the same on every tick, 2 when they
diverge, 1 on any other error.`,
		Example: `  keelctl doctrine diff examples/replays/run-042.log recon-standard@2.1.0 recon-standard@2.2.0
  curl -s http://127.0.0.1:8080/api/v1/replays/MSN-001 | keelctl doctrine diff - recon-standard@2.1.0 recon-standard@2.2.0`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return doctrineDiff(cmd, doctrineDir, args[0], args[1], args[2])
		},
	}
	cmd.Flags().StringVar(&doctrineDir, "doctrine-dir", "doctrine-packs", "directory of doctrine packs (*.yaml) both references resolve in")
	return cmd
}

func doctrineDiff(cmd *cobra.Command, doctrineDir, path, refA, refB string) error {
	packs, err := files.ReadPacks(doctrineDir)
	if err != nil {
		return err
	}
	a, err := resolvePack(packs, refA)
	if err != nil {
		return err
	}
	b, err := resolvePack(packs, refB)
	if err != nil {
		return err
	}
	src, err := openLog(path, cmd.InOrStdin())
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	w := cmd.OutOrStdout()
	res, err := missionlog.Diff(src, packs, a, b, func(d missionlog.TickDiff) {
		_, _ = fmt.Fprintf(w, "%9s\n", seconds(d.TickMs))
		for _, x := range d.OnlyA {
			_, _ = fmt.Fprintf(w, "  - %-15s  %-12s  %s\n", x.Kind, x.Subject, x.Rationale)
		}
		for _, x := range d.OnlyB {
			_, _ = fmt.Fprintf(w, "  + %-15s  %-12s  %s\n", x.Kind, x.Subject, x.Rationale)
		}
		if d.CommandsDiffer {
			_, _ = fmt.Fprintf(w, "  ~ commands differ: %d under the first pack, %d under the second\n", d.CommandsA, d.CommandsB)
		}
	})
	if err != nil {
		return err
	}

	h := res.Header
	_, _ = fmt.Fprintf(w, "\nlog       %s, mission %s, seed %d, plan %s\n", h.Name, h.Mission, h.Seed, short(string(h.Plan)))
	for _, s := range []struct {
		name string
		side missionlog.Side
	}{{"first", res.A}, {"second", res.B}} {
		_, _ = fmt.Fprintf(w, "%-9s %s, %d decisions, %d commands, mission %s, %.1f %% explored\n",
			s.name, engine.PackLabel(s.side.Pack.Ref, s.side.Pack.Hash), s.side.Decisions, s.side.Commands, s.side.State.Mission.State, explored(s.side.State))
	}
	if res.Same() {
		_, _ = fmt.Fprintf(w, "ticks     %d compared, none diverging\n", res.Ticks)
		_, _ = fmt.Fprintln(w, "verdict   the packs decide the same on this recording")
		return nil
	}
	_, _ = fmt.Fprintf(w, "ticks     %d compared, %d diverging, the first at %s\n", res.Ticks, res.Diverging, seconds(res.FirstMs))
	_, _ = fmt.Fprintln(w, "verdict   the packs decide differently on this recording (open loop after the first divergence)")
	return fmt.Errorf("%w: %d of %d ticks diverge", errPacksDiffer, res.Diverging, res.Ticks)
}

// resolvePack resolves a "<name>@<version>" reference in the registry.
func resolvePack(packs *doctrine.Registry, s string) (*doctrine.Pack, error) {
	ref, err := doctrine.ParseRef(s)
	if err != nil {
		return nil, err
	}
	return packs.Resolve(ref)
}
