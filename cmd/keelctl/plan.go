package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/files"
	"github.com/dsanchez31/keel/internal/planner"
)

type compileOptions struct {
	backend     string
	model       string
	world       string
	doctrineDir string
	ollamaURL   string
	timeout     time.Duration
	think       bool
	dryRun      bool
}

func newPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Compile and inspect mission plans",
	}
	cmd.AddCommand(newPlanCompileCmd())
	return cmd
}

func newPlanCompileCmd() *cobra.Command {
	var opts compileOptions
	cmd := &cobra.Command{
		Use:   "compile [flags] <intent...>",
		Short: "Compile operator intent into a validated, content-addressed plan",
		Long: `Compile operator intent into Plan IR through a language model, validate it
through the four gates (schema, resolution, feasibility, doctrine) and repair
it from the diagnostics, for at most 3 attempts. A short triage call first
declines an intent that asks for no mission, with its reason.

The outcome is printed as JSON on stdout. Exit status: 0 when a plan was
produced, 2 when none was (every attempt refused or the intent declined, a
valid outcome the printed outcome explains), 1 on any other error.`,
		Example: `  keelctl plan compile "Grid-search the unexplored area to lift the fog of war"
  keelctl plan compile --backend claude "Grid-search the unexplored area"
  keelctl plan compile --dry-run "Grid-search the unexplored area"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return compile(cmd, opts, strings.Join(args, " "))
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.backend, "backend", planner.BackendOllama, "planner backend: ollama or claude")
	f.StringVar(&opts.model, "model", "", fmt.Sprintf("model name (default %s for ollama, %s for claude)", planner.DefaultOllamaModel, planner.DefaultClaudeModel))
	f.StringVar(&opts.world, "world", "examples/worlds/reference.yaml", "world file: named areas, ground stations and the fleet snapshot")
	f.StringVar(&opts.doctrineDir, "doctrine-dir", "doctrine-packs", "directory of doctrine packs (*.yaml)")
	f.StringVar(&opts.ollamaURL, "ollama-url", "", "Ollama server URL (default $OLLAMA_HOST, else "+planner.DefaultOllamaURL+")")
	f.DurationVar(&opts.timeout, "timeout", 5*time.Minute, "bound on the whole compilation, every attempt included")
	f.BoolVar(&opts.think, "think", false, "let the model reason before replying: Ollama thinks, Claude keeps its default effort instead of low")
	f.BoolVar(&opts.dryRun, "dry-run", false, "print the prompts and the schemas, call no backend")
	return cmd
}

func compile(cmd *cobra.Command, opts compileOptions, intent string) error {
	if strings.TrimSpace(intent) == "" {
		return errors.New("intent is empty")
	}
	world, err := files.ReadWorld(opts.world)
	if err != nil {
		return err
	}
	packs, err := files.ReadPacks(opts.doctrineDir)
	if err != nil {
		return err
	}

	if opts.dryRun {
		return printDryRun(cmd, planner.TriageConversation(intent), planner.Opening(intent, world, packs))
	}

	p, err := newBackend(opts)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()

	out, cerr := planner.Compile(ctx, p, planner.Input{Intent: intent, World: world, Doctrines: packs, Arrival: engine.DefaultConfig().Arrival()})
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("printing the outcome: %w", err)
	}
	switch {
	case cerr != nil:
		return cerr
	case out.Declined():
		return fmt.Errorf("%w: the intent asks for no mission: %s", errNoPlan, out.Triage.Reason)
	case !out.OK():
		return fmt.Errorf("%w within the repair budget", errNoPlan)
	}
	return nil
}

// printDryRun prints the two conversations a compilation opens with: the
// triage call, then the first plan attempt.
func printDryRun(cmd *cobra.Command, triage, plan planner.Conversation) error {
	w := cmd.OutOrStdout()
	for _, s := range []struct {
		prefix string
		c      planner.Conversation
	}{{"triage ", triage}, {"", plan}} {
		schema, err := json.MarshalIndent(s.c.Schema, "", "  ")
		if err != nil {
			return fmt.Errorf("printing the schema: %w", err)
		}
		if _, err := fmt.Fprintf(w, "# %ssystem\n%s\n# %suser\n%s\n\n# %sschema\n%s\n\n", s.prefix, s.c.System, s.prefix, s.c.Messages[0].Content, s.prefix, schema); err != nil {
			return err
		}
	}
	return nil
}

func newBackend(opts compileOptions) (planner.Planner, error) {
	u := opts.ollamaURL
	if u == "" {
		u = planner.OllamaHostURL(os.Getenv("OLLAMA_HOST"))
	}
	return planner.NewBackend(opts.backend, planner.BackendOptions{Model: opts.model, OllamaURL: u, Think: opts.think})
}
