package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/routing"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/exporter"
)

func newRelayStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the relay is running and what it has done",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := relayDir()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			snap, running, err := relayStats(dir)
			if err != nil {
				return err
			}
			if path, ok := relayServiceInstalled(); ok {
				fmt.Fprintf(out, "Service:  installed (%s)\n", path)
			}
			if running {
				fmt.Fprintf(out, "Running on %s since %s.\n", relayAddr(dir), snap.Since.Format(time.RFC3339))
			} else {
				if squatted(relayAddr(dir)) {
					fmt.Fprintf(out, "Warning: another process is listening on %s. The agents' telemetry goes to it, not to terma — stop it, or move the relay with `terma relay setup --addr`.\n", relayAddr(dir))
				}
				if data, err := os.ReadFile(filepath.Join(dir, relayErrorFile)); err == nil {
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

// squatted reports whether something answers on the relay's address while the relay
// is not running: the agents' exporters would be sending to it.
func squatted(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// relayStats reads the running relay's stats, else those the last run left behind.
func relayStats(dir string) (relay.Snapshot, bool, error) {
	var snap relay.Snapshot
	unlock, err := flock.TryLock(filepath.Join(dir, relayLockFile))
	if err == nil {
		unlock()
		data, err := os.ReadFile(filepath.Join(dir, relayStatsFile))
		if err != nil {
			return snap, false, nil
		}
		return snap, false, json.Unmarshal(data, &snap)
	}
	token, err := relayToken()
	if err != nil {
		return snap, true, err
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+relayAddr(dir)+"/stats", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return snap, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return snap, true, fmt.Errorf("relay stats: HTTP %s", resp.Status)
	}
	body, _ := io.ReadAll(resp.Body)
	return snap, true, json.Unmarshal(body, &snap)
}

// relayDoctorCheck is doctor's "agent exporting to Terma" on a machine that exports
// through the local relay: the relay can run (or runs) on its address with no one else
// there, each of the developer's agents sends to it, and this repository's sessions
// can leave — it is bound and this machine holds its project's key.
func relayDoctorCheck(projectID string, agents []string) doctor.Check {
	dir, err := claim.Dir()
	if err != nil {
		return doctor.Check{Status: doctor.Fail, Detail: err.Error()}
	}
	addr := relayAddr(dir)
	running := false
	if unlock, err := flock.TryLock(filepath.Join(dir, relayLockFile)); err == nil {
		unlock()
	} else if flock.IsBusy(err) {
		running = true
	}
	if !running && squatted(addr) {
		return doctor.Check{Status: doctor.Fail, Detail: "another process is listening on " + addr + " and receives the agents' telemetry",
			Fix: "stop it, or move the relay with `terma relay setup --addr`"}
	}
	var wrong []string
	for _, name := range []string{routing.AgentClaude, routing.AgentCodex, "opencode"} {
		// OpenCode only for a developer who named it: its plugin is not set up by
		// default, and most machines have no OpenCode.
		if (len(agents) > 0 || name == "opencode") && !slices.Contains(agents, name) {
			continue
		}
		h, err := harness.Lookup(name)
		if err != nil {
			continue
		}
		if st, err := h.Status(); err != nil || !st.Connected || strings.TrimRight(st.Endpoint, "/") != "http://"+addr {
			wrong = append(wrong, h.DisplayName())
		}
	}
	if len(wrong) > 0 {
		return doctor.Check{Status: doctor.Fail, Detail: strings.Join(wrong, " and ") + " not exporting to the local relay", Fix: "terma relay setup"}
	}
	if slices.Contains(agents, routing.AgentCodex) || len(agents) == 0 {
		if d, ok := exporter.CodexDaemonPredates(dir); ok {
			return doctor.Check{Status: doctor.Warn, Detail: fmt.Sprintf("Codex's background server (pid %d) started before Codex was pointed at the relay, and its threads still export where they did", d.PID),
				Fix: exporter.CodexDaemonRestart}
		}
	}
	state := "starts with the next hook"
	if running {
		state = "running"
	}
	switch {
	case projectID == "":
		return doctor.Check{Status: doctor.Warn, Detail: "local relay on " + addr + " (" + state + "); this repository is not bound, so its sessions are never forwarded", Fix: "terma install"}
	case keystore.Get(projectID) == "" && keystore.GetFor("claude", projectID) == "" && keystore.GetFor("codex", projectID) == "":
		return doctor.Check{Status: doctor.Warn, Detail: "local relay on " + addr + " (" + state + "); no key for this project on this machine, so its sessions are dropped", Fix: "terma install"}
	}
	return doctor.Check{Status: doctor.Pass, Detail: "through the local relay on " + addr + " (" + state + "); only this repository's sessions are forwarded"}
}
