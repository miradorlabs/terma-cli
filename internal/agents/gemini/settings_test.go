package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pointing Gemini at the relay changes only the telemetry block of its settings, keeps
// the file private, and writes an extension whose hooks run terma's own command; a
// settings file that does not parse is left alone.
func TestConnectRelayChangesOnlyTelemetry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GEMINI_CLI_HOME", home)
	t.Setenv("HOME", home)
	settings, err := settingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"theme":"dark","telemetry":{"useCollector":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gotSettings, extension, err := connectRelay("http://127.0.0.1:43180/tok", []string{"/Users/x y/terma", "hook"})
	if err != nil || gotSettings != settings {
		t.Fatalf("connect: %q %v", gotSettings, err)
	}
	var doc struct {
		Theme     string         `json:"theme"`
		Telemetry map[string]any `json:"telemetry"`
	}
	data, _ := os.ReadFile(settings)
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Theme != "dark" || doc.Telemetry["useCollector"] != true || doc.Telemetry["otlpEndpoint"] != "http://127.0.0.1:43180/tok" || doc.Telemetry["enabled"] != true {
		t.Fatalf("settings: %s", data)
	}
	if info, _ := os.Stat(settings); info.Mode().Perm() != 0o600 {
		t.Errorf("settings are %v: the endpoint carries the relay's token", info.Mode().Perm())
	}
	hooks, _ := os.ReadFile(filepath.Join(extension, "hooks", "hooks.json"))
	for _, e := range geminiHookEvents {
		if !strings.Contains(string(hooks), `'/Users/x y/terma' 'hook' `+e.terma) {
			t.Errorf("extension hooks lack %s:\n%s", e.terma, hooks)
		}
	}
	if err := os.WriteFile(settings, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := connectRelay("http://127.0.0.1:43180/tok", []string{"terma", "hook"}); err == nil {
		t.Fatal("a settings file that does not parse was rewritten")
	}
}
