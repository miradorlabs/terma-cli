package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// Repository install can wrap the machine's Claude status line without ever
// connecting machine-wide telemetry. Disconnect must still undo that wrap.
func TestDisconnectClaudeRestoresStatusLineWithoutTelemetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TERMA_ENV", "dev")
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	c := harness.Claude{}
	path, err := c.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"statusLine":{"type":"command","command":"echo original","padding":2}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := c.InstallStatusLine(); err != nil || !changed {
		t.Fatalf("install status line: changed=%v err=%v", changed, err)
	}
	out, err := runTerma(t, "disconnect", "claude", "--yes")
	if err != nil || !strings.Contains(out, "Status line: restored") {
		t.Fatalf("disconnect: %v\n%s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		StatusLine struct {
			Command string `json:"command"`
			Padding int    `json:"padding"`
		} `json:"statusLine"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.StatusLine.Command != "echo original" || settings.StatusLine.Padding != 2 {
		t.Fatalf("original status line was not restored: %s", data)
	}
}

func TestDisconnectClaudeCleansTelemetryWithCorruptStatusLineRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TERMA_ENV", "dev")
	configDir := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", configDir)
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "OTEL_") {
			t.Setenv(key, "")
		}
	}
	if out, err := runTerma(t, "connect", "claude", "--api-key", "ter_srv_leftover", "--project", testProjectID, "--yes"); err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	settings := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(filepath.Join(configDir, "statusline.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "disconnect", "claude", "--yes")
	if err != nil {
		t.Fatalf("corrupt status-line record blocked telemetry cleanup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Warning: could not restore the status line") {
		t.Fatalf("missing status-line warning: %s", out)
	}
	data, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ter_srv_leftover") {
		t.Fatalf("telemetry key survived disconnect: %s", data)
	}
}
