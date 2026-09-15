// Command keelctl is the operator's command line: compile a plan, describe,
// watch and validate a vehicle through its adapter, replay a mission log and
// verify it reproduces the recording, and diff two doctrine packs over a
// recorded mission.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/dsanchez31/keel/internal/buildinfo"
)

// Exit codes. A compilation that ends without a plan, after the repair budget
// or with the intent declined, is a valid outcome (spec section 8.1), so it
// has its own code rather than sharing the one for errors.
const (
	exitOK     = 0
	exitError  = 1
	exitNoPlan = 2
	// exitNotConformant is a validation that ran and did not pass: an
	// outcome, like a compilation without a plan.
	exitNotConformant = 2
	// exitNotVerified is a verifying replay that ran and did not reproduce
	// the recording.
	exitNotVerified = 2
	// exitPacksDiffer is a doctrine diff that ran and found the packs
	// deciding differently.
	exitPacksDiffer = 2
	programName     = "keelctl"
)

// errNoPlan reports a compilation that produced no plan: every attempt was
// refused, or the triage declined the intent. The outcome has already been
// printed when it is returned.
var errNoPlan = errors.New("no plan produced")

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run executes one command line and returns the process exit code. It is the
// whole program minus os.Exit, so tests drive it directly.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	root := newRootCmd(stdout, stderr)
	root.SetIn(stdin)
	root.SetArgs(args)
	err := root.Execute()
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errNoPlan):
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitNoPlan
	case errors.Is(err, errNotConformant):
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitNotConformant
	case errors.Is(err, errNotVerified):
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitNotVerified
	case errors.Is(err, errPacksDiffer):
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitPacksDiffer
	default:
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", programName, err)
		return exitError
	}
}

func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           programName,
		Short:         "Operate KEEL: compile plans from intent, connect and validate vehicles, replay missions, diff doctrine",
		Version:       buildinfo.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.AddCommand(newPlanCmd(), newVectorCmd(), newReplayCmd(), newDoctrineCmd())
	return root
}
