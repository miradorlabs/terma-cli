package cli

import (
	"github.com/spf13/cobra"
)

// The local relay (docs/RELAY.md): the agents' global exporters send to
// terma on loopback, and only what a hook in an opted-in repository claimed goes on
// to Terma. Every command here is hidden: `terma install` and `terma setup` run them.

func (app *App) newRelayCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "relay",
		Short:  "The local OTLP relay that forwards only opted-in repositories' telemetry",
		Hidden: true,
	}
	cmd.AddCommand(app.newRelayRunCommand(), app.newRelaySetupCommand(), newRelayStatusCommand(), newRelayDaemonCommand(), newRelaySuperviseCommand())
	return cmd
}
