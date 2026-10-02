package codex

import (
	"bytes"
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
	already := relayexport.NativePointed(exporter{}, strings.TrimPrefix(cfg.Endpoint, "http://"))
	before := configBytes()
	result, err := relayexport.Native(exporter{}, cfg)
	if err != nil {
		return result, err
	}
	// Only a changed config makes a running daemon stale; an idempotent re-run keeps the
	// time the config last changed.
	if !bytes.Equal(before, configBytes()) {
		_ = config.WriteFileAtomic(filepath.Join(cfg.StateDir, setupFile), []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600)
	}
	// The background server and the desktop app's own server read the exporter only at start.
	_, daemon := daemonPredates(cfg.StateDir)
	app := !already && desktopInstalled(context.Background())
	switch {
	case daemon && app:
		result.Notes = append(result.Notes, "Restart Codex so it starts sending to Terma: quit and reopen the desktop app, and run `codex app-server daemon restart` (running work may be interrupted).")
	case daemon:
		result.Notes = append(result.Notes, "Restart Codex's background server so it starts sending to Terma: `codex app-server daemon restart` (running work may be interrupted).")
	case app:
		result.Notes = append(result.Notes, "Quit and reopen the Codex desktop app so it starts sending to Terma.")
	}
	return result, nil
}

// configBytes is config.toml as it stands, nil when it cannot be read.
func configBytes() []byte {
	path, err := exporter{}.ConfigPath()
	if err != nil {
		return nil
	}
	data, _ := os.ReadFile(path)
	return data
}

// RelayPointed reports whether Codex's exporter sends to the relay at addr.
func (Agent) RelayPointed(addr string) (bool, bool) {
	return relayexport.NativePointed(exporter{}, addr), true
}

// RelayProblem reports a daemon started before Codex was pointed at the relay.
func (Agent) RelayProblem(stateDir string) (string, string, bool) {
	d, ok := daemonPredates(stateDir)
	if !ok {
		return "", "", false
	}
	return fmt.Sprintf("Codex's background server (pid %d) started before Codex was pointed at the relay, and its threads still export where they did", d.PID), daemonRestart, true
}

func daemonPredates(dir string) (daemon, bool) {
	d, ok := runningDaemon()
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
