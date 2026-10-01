package cli

import (
	"github.com/spf13/cobra"
)

// The local relay forwards only what hooks in opted-in repositories
// claimed; its commands are hidden because install and setup run them.

func (app *App) newRelayCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "relay",
		Short:  "The local OTLP relay that forwards only opted-in repositories' telemetry",
		Hidden: true,
	}
	cmd.AddCommand(app.newRelayRunCommand(), app.newRelaySetupCommand(), newRelayStatusCommand(), newRelayDaemonCommand(), newRelaySuperviseCommand())
	return cmd
}
