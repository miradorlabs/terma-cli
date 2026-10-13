package copilot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tailscale/hujson"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

var relay = agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "tok-0123456789abcdef"}

// sandbox gives the test a home with a VS Code that has run in it, no Copilot CLI on PATH
// and a login shell terma writes nothing for; it returns the agent and VS Code's settings file.
func sandbox(t *testing.T) (Agent, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("AppData", filepath.Join(home, "appdata"))
	t.Setenv("VSCODE_APPDATA", "")
	t.Setenv("COPILOT_HOME", filepath.Join(home, "copilot"))
	t.Setenv("ZDOTDIR", "")
	t.Setenv("SHELL", "/bin/nu")
	t.Setenv("PATH", "")
	t.Setenv("TERMA_SHELL_LAUNCHERS", "")
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(base, "Code", "User")
	if err := os.MkdirAll(user, 0o700); err != nil {
		t.Fatal(err)
	}
	return Agent{ConfigDir: filepath.Join(home, "terma")}, filepath.Join(user, "settings.json")
}

func settingsOf(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := hujson.Standardize(data)
	if err != nil {
		t.Fatalf("settings no longer parse: %v\n%s", err, data)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(plain, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// A settings file the developer wrote by hand, with what JSON proper refuses: comments, a
// trailing comma, and a layout of its own.
const handWritten = `// My settings
{
  // Editor
  "editor.fontSize": 13, /* was 12 */
  "files.exclude": {
    "**/.git": true, // noise
  },
  "workbench.colorTheme": "Default Dark Modern",
}
`

// Pointing VS Code at the relay adds terma's members and changes no other byte; undoing
// it gives the developer's file back exactly.
func TestConnectChangesOnlyItsOwnSettings(t *testing.T) {
	a, settings := sandbox(t)
	if err := os.WriteFile(settings, []byte(handWritten), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := a.ConfigureRelay(context.Background(), relay)
	if err != nil || len(res.Paths) != 1 || res.Paths[0] != settings {
		t.Fatalf("configure: %+v, %v", res, err)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "Restart VS Code") {
		t.Errorf("notes: %q", res.Notes)
	}
	data, _ := os.ReadFile(settings)
	added := `
  "github.copilot.chat.otel.enabled": true,
  "github.copilot.chat.otel.otlpEndpoint": "http://127.0.0.1:43180/tok-0123456789abcdef",
  "github.copilot.chat.otel.captureContent": true,
  "chat.agentHost.otel.enabled": true,
  "chat.agentHost.otel.otlpEndpoint": "http://127.0.0.1:43180/tok-0123456789abcdef/v1/traces",
  "chat.agentHost.otel.captureContent": true,
  "settingsSync.ignoredSettings": ["github.copilot.chat.otel.enabled","github.copilot.chat.otel.otlpEndpoint","github.copilot.chat.otel.captureContent","chat.agentHost.otel.enabled","chat.agentHost.otel.otlpEndpoint","chat.agentHost.otel.captureContent"],`
	if want := strings.Replace(handWritten, "{", "{"+added, 1); string(data) != want {
		t.Fatalf("settings after setup:\n%s\nwant:\n%s", data, want)
	}
	if info, _ := os.Stat(settings); info.Mode().Perm() != 0o600 {
		t.Errorf("settings are %v: the endpoint carries the relay's token", info.Mode().Perm())
	}
	if doc := settingsOf(t, settings); doc["editor.fontSize"] != float64(13) || doc[chatEnabled] != true {
		t.Fatalf("settings read back as %v", doc)
	}
	if pointed, known := a.RelayPointed("127.0.0.1:43180"); !pointed || !known {
		t.Errorf("RelayPointed = %v, %v", pointed, known)
	}
	if pointed, _ := a.RelayPointed("127.0.0.1:4319"); pointed {
		t.Error("reads as pointed at an address the relay is not on")
	}
	// A second setup has nothing to change, and nothing to restart for.
	if again, err := a.ConfigureRelay(context.Background(), relay); err != nil || len(again.Notes) > 0 {
		t.Fatalf("a second configure: %+v, %v", again, err)
	}
	undone, err := a.UnconfigureRelay(context.Background(), relay.Token)
	if err != nil || len(undone.Paths) != 1 || len(undone.Notes) > 0 {
		t.Fatalf("unconfigure: %+v, %v", undone, err)
	}
	if data, _ := os.ReadFile(settings); string(data) != handWritten {
		t.Fatalf("settings after undo:\n%s", data)
	}
	if left, _ := filepath.Glob(filepath.Join(a.ConfigDir, "setup", "*")); len(left) > 0 {
		t.Errorf("the record of what setup replaced outlived the undo: %v", left)
	}
}

// What the developer had set is put back as they wrote it, but for a member they changed
// after setup: that edit is theirs.
func TestDisconnectRestoresTheDevelopersOwn(t *testing.T) {
	a, settings := sandbox(t)
	mine := `{
	"github.copilot.chat.otel.otlpEndpoint": "https://otel.example.com", // ours
	"github.copilot.chat.otel.captureContent": false,
	"settingsSync.ignoredSettings": [
		"editor.fontSize" // per machine
	]
}
`
	if err := os.WriteFile(settings, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ConfigureRelay(context.Background(), relay); err != nil {
		t.Fatal(err)
	}
	doc := settingsOf(t, settings)
	ignored, _ := doc[syncIgnored].([]any)
	if doc[chatEndpoint] != "http://127.0.0.1:43180/tok-0123456789abcdef" || doc[chatContent] != true || len(ignored) != 7 || ignored[0] != "editor.fontSize" {
		t.Fatalf("settings after setup: %v", doc)
	}
	data, _ := os.ReadFile(settings)
	if !strings.Contains(string(data), "// ours") {
		t.Fatalf("the developer's comment went with their value:\n%s", data)
	}
	if undone, err := a.UnconfigureRelay(context.Background(), relay.Token); err != nil || len(undone.Paths) != 1 {
		t.Fatalf("unconfigure: %+v, %v", undone, err)
	}
	if data, _ := os.ReadFile(settings); string(data) != mine {
		t.Fatalf("settings after undo:\n%s", data)
	}

	// Pointed elsewhere by the developer since: not terma's to restore.
	if _, err := a.ConfigureRelay(context.Background(), relay); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(settings)
	edited := strings.Replace(string(data), `"http://127.0.0.1:43180/tok-0123456789abcdef"`, `"https://collector.example.com"`, 1)
	if err := os.WriteFile(settings, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	undone, err := a.UnconfigureRelay(context.Background(), relay.Token)
	if err != nil || len(undone.Notes) != 1 || !strings.Contains(undone.Notes[0], chatEndpoint) {
		t.Fatalf("unconfigure after an edit: %+v, %v", undone, err)
	}
	if doc := settingsOf(t, settings); doc[chatEndpoint] != "https://collector.example.com" || doc[chatContent] != false || doc[hostEnabled] != nil {
		t.Fatalf("settings after undo: %v", doc)
	}
}

// Settings terma cannot parse, or never pointed at the relay, stay as they are; an editor
// that never ran gets no settings at all.
func TestSettingsTermaLeavesAlone(t *testing.T) {
	a, settings := sandbox(t)
	broken := "{ \"editor.fontSize\": 13,, }\n"
	if err := os.WriteFile(settings, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ConfigureRelay(context.Background(), relay); err == nil || !strings.Contains(err.Error(), "leaves it alone") {
		t.Fatalf("a settings file that does not parse: %v", err)
	}
	if data, _ := os.ReadFile(settings); string(data) != broken {
		t.Fatalf("settings changed:\n%s", data)
	}
	for _, token := range []string{relay.Token, ""} {
		if undone, err := a.UnconfigureRelay(context.Background(), token); err != nil || len(undone.Paths) > 0 {
			t.Fatalf("unconfigure(%q) = %+v, %v", token, undone, err)
		}
	}
	if err := os.RemoveAll(filepath.Dir(filepath.Dir(settings))); err != nil {
		t.Fatal(err)
	}
	res, err := a.ConfigureRelay(context.Background(), relay)
	if err != nil || len(res.Paths) > 0 || len(res.Notes) > 0 {
		t.Fatalf("configure with no VS Code: %+v, %v", res, err)
	}
	if _, err := os.Stat(settings); err == nil {
		t.Fatal("wrote settings for an editor that never ran here")
	}
	if _, known := a.RelayPointed("127.0.0.1:43180"); known {
		t.Error("claims to know where a CLI's environment points")
	}
}

// A VS Code with no settings file yet gets one holding only terma's members, and a lost
// record of what setup replaced still lets the undo take them out by the relay's token.
func TestConnectCreatesSettingsAndUndoesWithoutItsRecord(t *testing.T) {
	a, settings := sandbox(t)
	if _, err := a.ConfigureRelay(context.Background(), relay); err != nil {
		t.Fatal(err)
	}
	if doc := settingsOf(t, settings); len(doc) != 7 || doc[hostEndpoint] != "http://127.0.0.1:43180/tok-0123456789abcdef/v1/traces" {
		t.Fatalf("new settings: %v", doc)
	}
	if err := os.RemoveAll(filepath.Join(a.ConfigDir, "setup")); err != nil {
		t.Fatal(err)
	}
	if undone, err := a.UnconfigureRelay(context.Background(), "another-token"); err != nil || len(undone.Paths) > 0 {
		t.Fatalf("unconfigure with another relay's token: %+v, %v", undone, err)
	}
	if undone, err := a.UnconfigureRelay(context.Background(), relay.Token); err != nil || len(undone.Paths) != 1 {
		t.Fatalf("unconfigure without the record: %+v, %v", undone, err)
	}
	data, _ := os.ReadFile(settings)
	if strings.Contains(string(data), relay.Token) || strings.Contains(string(data), "otel.enabled") {
		t.Fatalf("settings after undo:\n%s", data)
	}
}

// VS Code's own telemetry switched off also switches Copilot's export off: doctor says so.
func TestRelayProblemNamesTelemetryOff(t *testing.T) {
	a, settings := sandbox(t)
	if _, _, ok := a.RelayProblem(); ok {
		t.Fatal("a problem on a machine with nothing wrong")
	}
	if err := os.WriteFile(settings, []byte("{\n  \"telemetry.telemetryLevel\": \"off\" // privacy\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	detail, fix, ok := a.RelayProblem()
	if !ok || !strings.Contains(detail, "telemetry.telemetryLevel") || !strings.Contains(fix, "restart VS Code") {
		t.Fatalf("RelayProblem = %q, %q, %v", detail, fix, ok)
	}
}
