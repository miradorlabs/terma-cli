package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/config"
)

const (
	setupFile = "codex-setup"
	// daemonRestart reloads a daemon that predates its exporter.
	daemonRestart = "restart Codex's background server with `codex app-server daemon restart` (running work may be interrupted)"
)

// ConfigureRelay points Codex's exporter at the local relay, noting a daemon that keeps
// exporting where it did until restarted.
func (a Agent) ConfigureRelay(_ context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	result, err := relayexport.Native(Codex{}, cfg)
	if err != nil {
		return result, err
	}
	_ = config.WriteFileAtomic(filepath.Join(cfg.StateDir, setupFile), []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600)
	if d, ok := daemonPredates(cfg.StateDir); ok {
		result.Notes = append(result.Notes, fmt.Sprintf("Codex's background server (pid %d) reads its exporter only when it starts, so its threads — Codex Desktop's, and the TUI's since 0.157 — still export where they did: %s.", d.PID, daemonRestart))
	}
	return result, nil
}

// RelayPointed reports whether Codex's exporter sends to the relay at addr.
func (Agent) RelayPointed(addr string) (bool, bool) {
	return relayexport.NativePointed(Codex{}, addr), true
}

// RelayProblem reports a daemon started before Codex was pointed at the relay.
func (Agent) RelayProblem(stateDir string) (string, string, bool) {
	d, ok := daemonPredates(stateDir)
	if !ok {
		return "", "", false
	}
	return fmt.Sprintf("Codex's background server (pid %d) started before Codex was pointed at the relay, and its threads still export where they did", d.PID), daemonRestart, true
}

func daemonPredates(dir string) (CodexDaemon, bool) {
	d, ok := RunningCodexDaemon()
	if !ok {
		return d, false
	}
	data, err := os.ReadFile(filepath.Join(dir, setupFile))
	if err != nil {
		return d, false
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	return d, err == nil && d.Started.Before(at)
}

var (
	_ agents.RelayExporter = Agent{}
	_ agents.RelayChecker  = Agent{}
)
