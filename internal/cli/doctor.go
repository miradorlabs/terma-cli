package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/ui/spinner"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

func (app *App) newDoctorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Verify the whole chain end to end and report setup readiness",
		Long: `Says what this machine collects and what the repository has in progress, then
checks every link between a coding agent and the Terma backend: the binary,
your sign-in, whether your team collects this folder, the machine-wide hooks, the
harness export, the event spool, and delivery of what it holds to the backend.

Every failure names the command that fixes it, and the report ends with the
remaining steps to complete setup.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env := app.doctorEnv(cmd.Context())
			printContext(cmd.OutOrStdout(), doctor.Context(env))
			if app.executeDoctor(cmd, env).Failed() {
				return errors.New("some checks failed")
			}
			return nil
		},
	}
	return cmd
}

// printContext prints doctor's context rows, each with its label, then a blank line.
func printContext(w io.Writer, rows []doctor.Row) {
	if len(rows) == 0 {
		return
	}
	out := style.Highlight(w)
	for _, r := range rows {
		label := ""
		if r.Label != "" {
			label = r.Label + ":"
		}
		fmt.Fprintf(out, "%-13s%s\n", label, r.Value)
	}
	fmt.Fprintln(out)
}

// executeDoctor runs doctor's checks, printing each as it finishes.
func (app *App) executeDoctor(cmd *cobra.Command, env doctor.Env) doctor.Report {
	out := cmd.OutOrStdout()
	// Streamed: delivery can take long enough that a report printed at the end looks like a hang.
	sp := spinner.New(cmd.ErrOrStderr())
	report := doctor.Run(cmd.Context(), env, doctor.Progress{
		Start: func(name string) { sp.Start(name + "…") },
		Done: func(c doctor.Check) {
			sp.Stop()
			doctor.RenderCheck(out, c, doctor.NameWidth)
		},
	})
	sp.Stop()
	doctor.RenderSummary(out, report)
	return report
}
