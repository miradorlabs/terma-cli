package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

func newRelayStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the relay is running and what it has done",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := daemon.Dir()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			snap, running, err := daemon.Stats(dir)
			if err != nil {
				return err
			}
			if path, ok := daemon.ServiceInstalled(); ok {
				fmt.Fprintf(out, "Service:  installed (%s)\n", path)
			}
			if running {
				fmt.Fprintf(out, "Running on %s since %s.\n", daemon.Addr(dir), snap.Since.Format(time.RFC3339))
			} else {
				if daemon.Squatted(daemon.Addr(dir)) {
					fmt.Fprintf(out, "Warning: another process is listening on %s. The agents' telemetry goes to it, not to terma — stop it, or move the relay with `terma relay setup --addr`.\n", daemon.Addr(dir))
				}
				if data, err := os.ReadFile(filepath.Join(dir, daemon.ErrorFile)); err == nil {
					fmt.Fprintf(out, "The relay last failed to start: %s", data)
				}
				fmt.Fprintln(out, "Not running. Last run:")
			}
			for _, k := range snap.Keys() {
				fmt.Fprintf(out, "  %-44s %d\n", k, snap.Counters[k])
			}
			// What waits on disk for delivery: accepted for a claimed session, not yet
			// taken by its project's host. The next relay sends it.
			for _, q := range relay.Backlog(filepath.Join(dir, relay.OutboxDir)) {
				who := q.Project
				if q.Tool != "" {
					who += " (" + q.Tool + ")"
				}
				fmt.Fprintf(out, "Queued for %s: %d records in %d parts.\n", who, q.Records, q.Parts)
			}
			return nil
		},
	}
}
