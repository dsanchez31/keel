package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/files"
	"github.com/dsanchez31/keel/internal/missionlog"
)

// errNotVerified reports a verifying replay that did not reproduce the
// recording, or a recording whose chain is broken. The report has already
// been printed when it is returned.
var errNotVerified = errors.New("not verified")

// maxPayloadBytes bounds a payload printed in a divergence report. A plan
// approval carries the whole AO raster; the part that differs is the
// decisions and commands, which are short.
const maxPayloadBytes = 1024

type replayOptions struct {
	doctrineDir string
	verify      bool
}

func newReplayCmd() *cobra.Command {
	var opts replayOptions
	cmd := &cobra.Command{
		Use:   "replay [flags] <log>",
		Short: "Replay a mission log through the engine, and verify it reproduces the recording",
		Long: `Replay a mission log: feed engine.Step, from the state its header records, the
events the recording holds, and print every decision the replay makes. The log
is a file, a segment directory (keeld's data_dir/missions/MSN-NNN), or - for
standard input, such as a log served by GET /api/v1/replays/{id}.

The recorded chain is checked as it is read (I6). With --verify-hash, every
record the replay rebuilds is compared with the recorded one, the first that
differs is reported, and both heads are printed (I7). The plan and every hot
swap resolve against --doctrine-dir, pinned by hash: a pack edited since the
recording shows as a refusal naming it.

Exit status: 0 when the replay ran and, with --verify-hash, reproduced the
recording; 2 when --verify-hash found a divergence or a broken chain; 1 on any
other error.`,
		Example: `  keelctl replay examples/replays/run-042.log --verify-hash
  keelctl replay run/keeld/missions/MSN-001
  curl -s http://127.0.0.1:8080/api/v1/replays/MSN-001 | keelctl replay - --verify-hash`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return replay(cmd, opts, args[0])
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.doctrineDir, "doctrine-dir", "doctrine-packs", "directory of doctrine packs (*.yaml) the recording ran under")
	f.BoolVar(&opts.verify, "verify-hash", false, "compare every replayed record with the recording and report the first that differs")
	return cmd
}

func replay(cmd *cobra.Command, opts replayOptions, path string) error {
	packs, err := files.ReadPacks(opts.doctrineDir)
	if err != nil {
		return err
	}
	src, err := openLog(path, cmd.InOrStdin())
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	w := cmd.OutOrStdout()
	res, err := missionlog.Replay(src, missionlog.Options{
		Packs:  packs,
		Verify: opts.verify,
		OnTick: func(r missionlog.ReplayedTick) {
			for _, d := range r.Decisions {
				_, _ = fmt.Fprintf(w, "%9s  %-15s  %-12s  %s\n", seconds(d.TickMs), d.Kind, d.Subject, d.Rationale)
			}
		},
	})
	if err != nil {
		if opts.verify && errors.Is(err, missionlog.ErrBroken) {
			return fmt.Errorf("%w: %w", errNotVerified, err)
		}
		return err
	}

	h := res.Header
	_, _ = fmt.Fprintf(w, "\nlog       %s, mission %s, seed %d, plan %s\n", h.Name, h.Mission, h.Seed, short(string(h.Plan)))
	_, _ = fmt.Fprintf(w, "replay    %d ticks (%s), %d decisions, %d commands, mission %s, %.1f %% explored\n",
		res.Ticks, seconds(res.Ticks*engine.TickIntervalMs), res.Decisions, res.Commands, res.State.Mission.State, explored(res.State))
	_, _ = fmt.Fprintf(w, "chain     %d records linked (I6)\n", res.Records)
	if !opts.verify {
		_, _ = fmt.Fprintf(w, "head      %s\n", res.Replayed)
		return nil
	}

	if d := res.Divergence; d != nil {
		_, _ = fmt.Fprintf(w, "diverged  at record %d, %s\n", d.Seq, seconds(d.TickMs))
		_, _ = fmt.Fprintf(w, "  recorded  %s\n", describe(d.Recorded, "no record: the replay rebuilt more for this tick"))
		_, _ = fmt.Fprintf(w, "  replayed  %s\n", describe(d.Replayed, "no record: the replay rebuilt less for this tick"))
	}
	_, _ = fmt.Fprintf(w, "recorded  %s\n", res.Recorded)
	_, _ = fmt.Fprintf(w, "replayed  %s\n", res.Replayed)
	if !res.Match() {
		_, _ = fmt.Fprintln(w, "verdict   the replay does not reproduce the recording (I7)")
		if d := res.Divergence; d != nil {
			return fmt.Errorf("%w: the replay leaves the recording at record %d", errNotVerified, d.Seq)
		}
		return fmt.Errorf("%w: the heads differ", errNotVerified)
	}
	_, _ = fmt.Fprintln(w, "verdict   the replay reproduces the recording, record for record (I7)")
	return nil
}

// openLog opens a log file, a segment directory or, for "-", in.
func openLog(path string, in io.Reader) (io.ReadCloser, error) {
	if path == "-" {
		return io.NopCloser(in), nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return eventlog.OpenDir(path)
	}
	return os.Open(path)
}

// explored is the percentage of the AO's cells explored in s.
func explored(s engine.State) float64 {
	n, total := s.Grid().Coverage()
	if total == 0 {
		return 0
	}
	return float64(n) * 100 / float64(total)
}

// seconds renders mission time as seconds with one decimal.
func seconds(ms int64) string {
	return fmt.Sprintf("%.1f s", float64(ms)/1000)
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// describe renders one record of a divergence: its kind and its payload,
// cut at maxPayloadBytes.
func describe(r *eventlog.Record, none string) string {
	if r == nil {
		return none
	}
	p := r.Payload
	if len(p) > maxPayloadBytes {
		return fmt.Sprintf("%s %s... (%d bytes)", r.Kind, p[:maxPayloadBytes], len(p))
	}
	return fmt.Sprintf("%s %s", r.Kind, p)
}
