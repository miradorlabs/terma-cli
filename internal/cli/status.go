package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

func (app *App) newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "status",
		Hidden: true,
		Short:  "Show connections, queued events, and setup readiness",
		Long: `A quick, local view of this machine and repository: sign-in, the repository's admission,
hook wiring, connected agents, the event spool, and remaining setup steps.
Nothing is written and nothing is sent — run
` + "`terma doctor`" + ` for the end-to-end verification.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := style.Highlight(cmd.OutOrStdout())
			env := app.doctorEnv(cmd.Context())
			if env.ConfigErr != nil {
				return env.ConfigErr
			}
			rep, err := doctor.Local(cmd.Context(), env)
			if err != nil {
				return err
			}
			for _, r := range rep.Rows {
				label := ""
				if r.Label != "" {
					label = r.Label + ":"
				}
				fmt.Fprintf(out, "%-13s%s\n", label, r.Value)
			}
			doctor.RenderSummary(out, doctor.Build(rep.Checks))
			fmt.Fprintln(out, "Run `terma doctor` to verify hook execution and backend delivery.")
			return nil
		},
	}
}
