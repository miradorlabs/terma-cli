package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

func newRelayDaemonCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the relay as a per-user service (launchd, systemd --user, Windows's Run key)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Install and start the relay service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			relayServiceWanted("on") // an explicit install clears an opt-out
			path, err := daemon.InstallService(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "The relay runs as a service (%s): always on, restarted by the system.\n", path)
			return nil
		},
	}, &cobra.Command{
		Use:   "remove",
		Short: "Stop and remove the relay service (hooks start the relay on demand again)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			removed, err := daemon.RemoveService(cmd.Context())
			if err != nil {
				return err
			}
			relayServiceWanted("off") // and install does not put it back
			if removed {
				fmt.Fprintln(cmd.OutOrStdout(), "The relay service is removed; hooks start the relay on demand.")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "No relay service was installed.")
			}
			return nil
		},
	})
	return cmd
}
