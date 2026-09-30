package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
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

// relayDoctorCheck is doctor's "agent exporting to Terma" on a machine that exports
// through the local relay: the relay can run (or runs) on its address with no one else
// there, each of the developer's agents sends to it, and this repository's sessions
// can leave — it is bound and this machine holds its project's key.
func relayDoctorCheck(projectID string, selected []string) doctor.Check {
	dir, err := claim.Dir()
	if err != nil {
		return doctor.Check{Status: doctor.Fail, Detail: err.Error()}
	}
	addr := daemon.Addr(dir)
	running := false
	if unlock, err := flock.TryLock(filepath.Join(dir, daemon.LockFile)); err == nil {
		unlock()
	} else if flock.IsBusy(err) {
		running = true
	}
	if !running && daemon.Squatted(addr) {
		return doctor.Check{Status: doctor.Fail, Detail: "another process is listening on " + addr + " and receives the selected' telemetry",
			Fix: "stop it, or move the relay with `terma relay setup --addr`"}
	}
	// The developer's selected, or the supported ones when none are recorded.
	mine := func(e agents.Agent) bool {
		if len(selected) == 0 {
			return registered.IsSupported(e.Name())
		}
		return slices.ContainsFunc(agents.Selections(e), func(s string) bool { return slices.Contains(selected, s) })
	}
	var wrong []string
	for _, e := range registered.With[agents.RelayExporter]() {
		if pointed, known := e.RelayPointed(addr); mine(e) && known && !pointed {
			wrong = append(wrong, e.DisplayName())
		}
	}
	if len(wrong) > 0 {
		return doctor.Check{Status: doctor.Fail, Detail: strings.Join(wrong, " and ") + " not exporting to the local relay", Fix: "terma relay setup"}
	}
	for _, e := range registered.With[agents.RelayExporter]() {
		if c, ok := e.(agents.RelayChecker); ok && mine(e) {
			if detail, fix, problem := c.RelayProblem(dir); problem {
				return doctor.Check{Status: doctor.Warn, Detail: detail, Fix: fix}
			}
		}
	}
	state := "starts with the next hook"
	if running {
		state = "running"
	}
	switch {
	case projectID == "":
		return doctor.Check{Status: doctor.Warn, Detail: "local relay on " + addr + " (" + state + "); this repository is not bound, so its sessions are never forwarded", Fix: "terma install"}
	case keystore.Get(projectID) == "" && !slices.ContainsFunc(registered.With[agents.RelayExporter](), func(e agents.RelayExporter) bool { return keystore.GetFor(e.Name(), projectID) != "" }):
		return doctor.Check{Status: doctor.Warn, Detail: "local relay on " + addr + " (" + state + "); no key for this project on this machine, so its sessions are dropped", Fix: "terma install"}
	}
	return doctor.Check{Status: doctor.Pass, Detail: "through the local relay on " + addr + " (" + state + "); only this repository's sessions are forwarded"}
}
