package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/desktoprelay"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

func TestDesktopManualConnectAndDisconnectRestoreCodexSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(home, "terma"))
	if err := os.MkdirAll(os.Getenv("CODEX_HOME"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("CODEX_HOME"), "config.toml")
	original := "model = \"test-model\"\n[otel]\nlog_user_prompt = false\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	// An earlier desktop connection suppressed content machine-wide. A reconnect
	// must give the repository relay enough data to apply its own content policy.
	if err := (harness.Codex{}).Connect(harness.Exporter{
		Endpoint: desktoprelay.Endpoint, Signals: []harness.Signal{harness.SignalLogs},
	}, false); err != nil {
		t.Fatal(err)
	}
	command, _, err := NewRootCommand().Find([]string{"desktop", "connect"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	command.SetOut(&output)
	if err := connectDesktop(command, true); err != nil {
		t.Fatal(err)
	}
	if err := connectDesktop(command, true); err != nil {
		t.Fatalf("repeat connect: %v", err)
	}
	status, err := (harness.Codex{}).Status()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Connected || status.Endpoint != desktoprelay.Endpoint || !status.IncludePrompts || !status.IncludeToolContent || len(status.Signals) != 1 || status.Signals[0] != harness.SignalLogs {
		t.Fatalf("desktop config = %+v", status)
	}
	configured, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(configured), `model = "test-model"`) {
		t.Fatalf("unrelated Codex setting was lost: %s, %v", configured, err)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", desktopServiceLabel+".plist")); !os.IsNotExist(err) {
		t.Fatalf("manual mode installed a service: %v", err)
	}
	if err := disconnectDesktop(command, nil); err != nil {
		t.Fatal(err)
	}
	status, err = (harness.Codex{}).Status()
	if err != nil || status.Connected || status.IncludePrompts {
		t.Fatalf("original Codex exporter/prompt setting not restored: %+v, %v", status, err)
	}
}
