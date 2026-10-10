package cli

import (
	"github.com/spf13/cobra"
)

// The local relay forwards only what hooks claimed in the repositories the team collects; its
// commands are hidden because setup runs them.

func (app *App) newRelayCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "relay",
		Short:  "The local OTLP relay that forwards only the team's repositories' telemetry",
		Hidden: true,
	}
	cmd.AddCommand(app.newRelayRunCommand(), app.newRelaySetupCommand(), app.newRelayDaemonCommand(), app.newRelaySuperviseCommand(), app.newRelayClassifyCommand())
	return cmd
}
