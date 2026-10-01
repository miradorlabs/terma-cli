package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/ui/spinner"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

func (app *App) newDoctorCommand() *cobra.Command {
	var skipCommit bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Verify the whole chain end to end and report setup readiness",
		Long: `Checks every link between a coding agent and the Terma backend: the binary,
your sign-in, the repository binding, the installed hooks and adapters, the
harness export, a scratch commit in a temporary worktree (does the hook actually
stamp a trailer?), the event spool, and the backend round-trip.

Every failure names the command that fixes it, and the report ends with the
remaining steps to complete setup.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app.executeDoctor(cmd, skipCommit).Failed() {
				return errors.New("some checks failed")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&skipCommit, "skip-commit", false, "do not make a scratch commit in a temporary worktree")
	return cmd
}

// executeDoctor is shared by `terma doctor` and install's verification; the caller decides
// what a failure means.
func (app *App) executeDoctor(cmd *cobra.Command, skipCommit bool) doctor.Report {
	out := cmd.OutOrStdout()
	if notice := captureNotice(); notice != "" {
		fmt.Fprintln(style.Highlight(out), notice)
	}
	// Streamed: the round-trip wait is long enough that a report printed at the end looks
	// like a hang.
	sp := spinner.New(cmd.ErrOrStderr())
	report := app.runDoctor(cmd.Context(), skipCommit, doctor.Progress{
		Start: func(name string) { sp.Start(name + "…") },
		Note:  sp.Update,
		Done: func(c doctor.Check) {
			sp.Stop()
			doctor.RenderCheck(out, c, doctor.NameWidth)
		},
	})
	sp.Stop()
	doctor.RenderSummary(out, report)
	return report
}
