package cmd

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

func serveRelayForRefresh(t *testing.T) {
	t.Helper()
	dir, err := relayDir()
	if err != nil {
		t.Fatal(err)
	}
	token, err := ensureRelayToken()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newTestRelay(relay.Options{Token: token}).Handler())
	t.Cleanup(srv.Close)
	if err := config.WriteFileAtomic(filepath.Join(dir, relayAddrFile), []byte(strings.TrimPrefix(srv.URL, "http://")), 0o600); err != nil {
		t.Fatal(err)
	}
	unlock, err := flock.TryLock(filepath.Join(dir, relayLockFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
}

func TestRefreshContinuesAfterExporterMigrationFails(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	sandboxMachine(t)
	shim := plantLegacyShim(t, "codex")
	dir, err := routing.RoutingDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "team.json"), []byte("broken json"), 0o600); err != nil {
		t.Fatal(err)
	}
	claude := claudeHarness(t)
	if _, err := claude.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	statusPath, _ := claude.ConfigPath()
	if err := os.WriteFile(statusPath, []byte(`{"statusLine":{"type":"command","command":"exec terma hook statusline"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	opencode := harnessOf(t, "opencode")
	if err := opencode.Connect(harness.Exporter{Endpoint: "https://example.invalid", APIKey: policyTestKey, ProjectID: "team", Signals: harness.AllSignals}, false); err != nil {
		t.Fatal(err)
	}
	pluginPath, _ := opencode.ConfigPath()
	wantPlugin, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginPath, append(wantPlugin, []byte("\n// stale plugin\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := refreshMachine()
	if err == nil || !strings.Contains(err.Error(), "migrate PATH-shim exporters") {
		t.Fatalf("lost migration failure: %v", err)
	}
	if len(changed) != 2 {
		t.Fatalf("unrelated refresh steps were skipped: %v", changed)
	}
	status, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct{ StatusLine struct{ Command string } }
	if err := json.Unmarshal(status, &settings); err != nil || settings.StatusLine.Command == "exec terma hook statusline" || !strings.Contains(settings.StatusLine.Command, "terma hook statusline") {
		t.Fatalf("Claude status line stayed stale: %s (%v)", status, err)
	}
	if got, _ := os.ReadFile(pluginPath); !bytes.Equal(got, wantPlugin) {
		t.Fatal("OpenCode plugin stayed stale")
	}
	if _, err := os.Stat(shim); err != nil {
		t.Fatalf("migration failure removed legacy shim: %v", err)
	}
}
