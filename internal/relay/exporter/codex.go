package exporter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

const codexSetupFile = "codex-setup"

// CodexDaemonRestart is the explicit action needed to reload a running daemon.
const CodexDaemonRestart = "restart Codex's background server with `codex app-server daemon restart` (running work may be interrupted)"

type codex struct{ native }

func (codex) Selections() []string { return []string{"codex", "codex-desktop"} }

func (c codex) Configure(ctx context.Context, cfg Config) (Result, error) {
	result, err := c.native.Configure(ctx, cfg)
	if err != nil {
		return result, err
	}
	_ = config.WriteFileAtomic(filepath.Join(cfg.StateDir, codexSetupFile), []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600)
	if d, ok := CodexDaemonPredates(cfg.StateDir); ok {
		result.Notes = append(result.Notes, fmt.Sprintf("Codex's background server (pid %d) reads its exporter only when it starts, so its threads — Codex Desktop's, and the TUI's since 0.157 — still export where they did: %s.", d.PID, CodexDaemonRestart))
	}
	return result, nil
}

// CodexDaemonPredates reports a daemon started before its exporter was configured.
func CodexDaemonPredates(dir string) (harness.CodexDaemon, bool) {
	d, ok := harness.RunningCodexDaemon()
	if !ok {
		return d, false
	}
	data, err := os.ReadFile(filepath.Join(dir, codexSetupFile))
	if err != nil {
		return d, false
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	return d, err == nil && d.Started.Before(at)
}

var _ Exporter = codex{}
