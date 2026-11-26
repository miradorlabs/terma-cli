package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/ui/spinner"
)

func (app *App) newDoctorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Verify the whole chain end to end and report setup readiness",
		Long: `Says what this machine collects and what the repository has in progress, then
checks every link between a coding agent and the Terma backend: the binary,
your sign-in, whether your team collects this repository, the machine-wide hooks, the
harness export, the event spool, and delivery of what it holds to the backend.

Every failure names the command that fixes it, and the report ends with the
remaining steps to complete setup.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app.executeDoctor(cmd, app.doctorEnv(cmd.Context())).Failed() {
				return errors.New("some checks failed")
			}
			return nil
		},
	}
	return cmd
}

// executeDoctor prints doctor's context, then runs its checks, printing each as it
// finishes, and closes with what needs the developer.
func (app *App) executeDoctor(cmd *cobra.Command, env doctor.Env) doctor.Report {
	pr := doctor.NewPrinter(cmd.OutOrStdout())
	pr.Sections(doctor.Context(env))
	pr.Title("Checks")
	// Streamed: delivery can take long enough that a report printed at the end looks like a hang.
	sp := spinner.New(cmd.ErrOrStderr())
	report := doctor.Run(cmd.Context(), env, doctor.Progress{
		Start: func(name string) { sp.Start(name + "…") },
		Done: func(c doctor.Check) {
			sp.Stop()
			if !doctor.InContext(c) {
				pr.Check(c)
			}
		},
	})
	sp.Stop()
	pr.Summary(report)
	return report
}
