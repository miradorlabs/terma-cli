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
