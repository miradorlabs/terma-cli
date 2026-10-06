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
	a := Agent{ConfigDir: t.TempDir()}
	cfg := agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "tok"}
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
	res, err := a.ConfigureRelay(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !restarts(res) {
		t.Fatalf("a daemon older than the new config was not flagged: %v", res.Notes)
	}

	// The config changed an hour ago and the daemon has been restarted since.
	stamp := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano) + "\n"
	if err := os.WriteFile(a.configChangedPath(), []byte(stamp), 0o600); err != nil {
		t.Fatal(err)
	}
	writeDaemon(time.Now().Add(-30 * time.Minute))
	res, err = a.ConfigureRelay(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if restarts(res) {
		t.Fatalf("an unchanged config asked for a restart: %v", res.Notes)
	}
	if _, _, stale := a.RelayProblem(); stale {
		t.Fatal("doctor would flag the restarted daemon")
	}

	// A new token rewrites the config, so the restarted daemon is stale again.
	cfg.Token = "tok2"
	res, err = a.ConfigureRelay(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !restarts(res) {
		t.Fatalf("a changed config did not ask for a restart: %v", res.Notes)
	}

	// The marker was lost but the config still points at the relay: the missing marker is
	// stamped, once.
	marker := a.configChangedPath()
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	writeDaemon(time.Now().Add(-2 * time.Hour))
	res, err = a.ConfigureRelay(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !restarts(res) {
		t.Fatalf("a daemon older than a wiped marker was not flagged: %v", res.Notes)
	}
	stamped, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	writeDaemon(time.Now().Add(time.Minute))
	res, err = a.ConfigureRelay(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if restarts(res) {
		t.Fatalf("an unchanged config asked for a restart: %v", res.Notes)
	}
	if again, _ := os.ReadFile(marker); string(again) != string(stamped) {
		t.Fatal("an unchanged config re-stamped the marker")
	}

	// Undoing it takes the marker along: Codex no longer points at the relay.
	if _, err := a.UnconfigureRelay(context.Background(), cfg.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("undoing the relay left the marker: %v", err)
	}
}
