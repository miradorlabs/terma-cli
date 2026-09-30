package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// The local relay (docs/RELAY.md): the agents' global exporters send to
// terma on loopback, and only what a hook in an opted-in repository claimed goes on
// to Terma. Every command here is hidden: `terma install` and `terma setup` run them.

// defaultRelayAddr is where the relay listens by default (claim.DefaultAddr).
const defaultRelayAddr = claim.DefaultAddr

const (
	relayAddrFile  = "addr"
	relayLockFile  = "relay.lock"
	relayStatsFile = "stats.json"
	relayPIDFile   = "pid"
	relayErrorFile = "last-error"
	// relayStopFile asks the relay whose pid it holds to stop: how stopRelay reaches a
	// relay on Windows, which has no SIGTERM. The relay looks every second.
	relayStopFile = "stop"
)

func newRelayCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "relay",
		Short:  "The local OTLP relay that forwards only opted-in repositories' telemetry",
		Hidden: true,
	}
	cmd.AddCommand(newRelayRunCommand(), newRelaySetupCommand(), newRelayStatusCommand(), newRelayDaemonCommand(), newRelaySuperviseCommand())
	return cmd
}

func relayDir() (string, error) {
	dir, err := claim.Dir()
	if err != nil {
		return "", err
	}
	return dir, os.MkdirAll(dir, 0o700)
}

// relayAddr is the address the relay listens on: what setup recorded, else the default.
func relayAddr(dir string) string {
	if data, err := os.ReadFile(filepath.Join(dir, relayAddrFile)); err == nil {
		if a := strings.TrimSpace(string(data)); a != "" {
			return a
		}
	}
	return defaultRelayAddr
}

func relayToken() (string, error) {
	path, err := claim.TokenPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("the relay is not set up on this machine — run `terma relay setup`")
	}
	return strings.TrimSpace(string(data)), nil
}
