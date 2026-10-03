package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// Re-running setup with the config already in place must not ask to restart a daemon
// that started after the config last changed.
func TestConfigureRelayRestartsOnlyAfterAChange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	state := t.TempDir()
	cfg := agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "tok", StateDir: state}
	writeDaemon := func(started time.Time) {
		dir := filepath.Join(home, "app-server-daemon")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		rec := fmt.Sprintf(`{"pid":%d,"processIdentity":{"startSeconds":%d}}`, os.Getpid(), started.Unix())
		if err := os.WriteFile(filepath.Join(dir, "daemon.pid"), []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restarts := func(res agents.RelayResult) bool {
		return strings.Contains(strings.Join(res.Notes, "\n"), "daemon restart")
	}

	writeDaemon(time.Now().Add(-2 * time.Hour))
	res, err := Agent{}.ConfigureRelay(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !restarts(res) {
		t.Fatalf("a daemon older than the new config was not flagged: %v", res.Notes)
	}

	// The config changed an hour ago and the daemon has been restarted since.
	stamp := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano) + "\n"
	if err := os.WriteFile(filepath.Join(state, setupFile), []byte(stamp), 0o600); err != nil {
		t.Fatal(err)
	}
	writeDaemon(time.Now().Add(-30 * time.Minute))
	res, err = Agent{}.ConfigureRelay(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if restarts(res) {
		t.Fatalf("an unchanged config asked for a restart: %v", res.Notes)
	}
	if _, _, stale := (Agent{}).RelayProblem(state); stale {
		t.Fatal("doctor would flag the restarted daemon")
	}

	// A new token rewrites the config, so the restarted daemon is stale again.
	cfg.Token = "tok2"
	res, err = Agent{}.ConfigureRelay(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !restarts(res) {
		t.Fatalf("a changed config did not ask for a restart: %v", res.Notes)
	}
}
