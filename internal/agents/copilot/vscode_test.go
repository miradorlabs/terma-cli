package copilot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tailscale/hujson"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

var relay = agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "tok-0123456789abcdef"}

// sandbox gives the test a home with a VS Code that Copilot has run in, no Copilot CLI on
// PATH, a login shell terma writes nothing for and no exporter in the environment; it
// returns the agent and VS Code's settings file.
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
	for _, name := range shellEndpoints {
		t.Setenv(name, "")
	}
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(base, "Code", "User")
	if err := os.MkdirAll(filepath.Join(user, "globalStorage", copilotStorage), 0o700); err != nil {
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

func configure(t *testing.T, a Agent) agents.RelayResult {
	t.Helper()
	res, err := a.ConfigureRelay(context.Background(), relay)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func unconfigure(t *testing.T, a Agent, token string) agents.RelayResult {
	t.Helper()
	res, err := a.UnconfigureRelay(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func noted(res agents.RelayResult, text string) bool {
	return slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, text) })
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
// it gives the developer's file back exactly. Copilot Chat's own exporter is never written.
func TestConnectChangesOnlyItsOwnSettings(t *testing.T) {
	a, settings := sandbox(t)
	if err := os.WriteFile(settings, []byte(handWritten), 0o644); err != nil {
		t.Fatal(err)
	}
	res := configure(t, a)
	if len(res.Paths) != 1 || res.Paths[0] != settings || len(res.Notes) != 1 || !noted(res, "Restart VS Code") {
		t.Fatalf("configure: %+v", res)
	}
	data, _ := os.ReadFile(settings)
	added := `
  "chat.agentHost.otel.enabled": true,
  "chat.agentHost.otel.otlpEndpoint": "http://127.0.0.1:43180/tok-0123456789abcdef/v1/traces",
  "chat.agentHost.otel.captureContent": true,
  "settingsSync.ignoredSettings": ["chat.agentHost.otel.enabled","chat.agentHost.otel.otlpEndpoint","chat.agentHost.otel.captureContent"],`
	if want := strings.Replace(handWritten, "{", "{"+added, 1); string(data) != want {
		t.Fatalf("settings after setup:\n%s\nwant:\n%s", data, want)
	}
	if strings.Contains(string(data), "github.copilot.chat.otel") {
		t.Fatal("setup switched on the exporter VS Code copies into its extensions' environment")
	}
	if info, _ := os.Stat(settings); info.Mode().Perm() != 0o600 {
		t.Errorf("settings are %v: the endpoint carries the relay's token", info.Mode().Perm())
	}
	if pointed, known := a.RelayPointed("127.0.0.1:43180"); !pointed || !known {
		t.Errorf("RelayPointed = %v, %v", pointed, known)
	}
	if pointed, _ := a.RelayPointed("127.0.0.1:4319"); pointed {
		t.Error("reads as pointed at an address the relay is not on")
	}
	// A second setup has nothing to change, and nothing to restart for.
	if again := configure(t, a); len(again.Notes) > 0 {
		t.Fatalf("a second configure: %+v", again)
	}
	if undone := unconfigure(t, a, relay.Token); len(undone.Paths) != 1 || len(undone.Notes) > 0 {
		t.Fatalf("unconfigure: %+v", undone)
	}
	if data, _ := os.ReadFile(settings); string(data) != handWritten {
		t.Fatalf("settings after undo:\n%s", data)
	}
	if left, _ := filepath.Glob(filepath.Join(a.ConfigDir, "setup", "*")); len(left) > 0 {
		t.Errorf("the record of what setup replaced outlived the undo: %v", left)
	}
}

// What the developer had set is put back as they wrote it, but for a member they changed
// after setup, which is said, and one they took out, which is not: both are theirs.
func TestDisconnectRestoresTheDevelopersOwn(t *testing.T) {
	a, settings := sandbox(t)
	mine := `{
	"chat.agentHost.otel.otlpEndpoint": "http://localhost:4318", // ours
	"chat.agentHost.otel.captureContent": false,
	"settingsSync.ignoredSettings": [
		"editor.fontSize" // per machine
	]
}
`
	if err := os.WriteFile(settings, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	configure(t, a)
	doc := settingsOf(t, settings)
	ignored, _ := doc[syncIgnored].([]any)
	if doc[hostEndpoint] != "http://127.0.0.1:43180/tok-0123456789abcdef/v1/traces" || doc[hostContent] != true || len(ignored) != 4 || ignored[0] != "editor.fontSize" {
		t.Fatalf("settings after setup: %v", doc)
	}
	if data, _ := os.ReadFile(settings); !strings.Contains(string(data), "// ours") {
		t.Fatalf("the developer's comment went with their value:\n%s", data)
	}
	if undone := unconfigure(t, a, relay.Token); len(undone.Paths) != 1 {
		t.Fatalf("unconfigure: %+v", undone)
	}
	if data, _ := os.ReadFile(settings); string(data) != mine {
		t.Fatalf("settings after undo:\n%s", data)
	}

	// Pointed elsewhere by the developer since, and the content switch taken out.
	configure(t, a)
	data, _ := os.ReadFile(settings)
	edited := strings.Replace(string(data), `"http://127.0.0.1:43180/tok-0123456789abcdef/v1/traces"`, `"http://localhost:9999"`, 1)
	edited = strings.Replace(edited, "\t\"chat.agentHost.otel.captureContent\": true,\n", "", 1)
	if err := os.WriteFile(settings, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	undone := unconfigure(t, a, relay.Token)
	if len(undone.Notes) != 1 || !noted(undone, hostEndpoint) {
		t.Fatalf("unconfigure after the developer's edits: %+v", undone)
	}
	if doc := settingsOf(t, settings); doc[hostEndpoint] != "http://localhost:9999" || doc[hostEnabled] != nil {
		t.Fatalf("settings after undo: %v", doc)
	}
	// The endpoint stays theirs without being said again on every later setup.
	if again := unconfigure(t, a, relay.Token); len(again.Notes) > 1 {
		t.Fatalf("a second unconfigure: %+v", again)
	}
}

// With the record of what it replaced lost, a setup run again takes the exporter already
// pointed at the relay for its own, not for what the developer had: teardown then removes
// it, where restoring it would leave the export and its content switch on.
func TestSetupAgainWithoutItsRecordStillUndoes(t *testing.T) {
	a, settings := sandbox(t)
	if err := os.WriteFile(settings, []byte(handWritten), 0o644); err != nil {
		t.Fatal(err)
	}
	configure(t, a)
	if err := os.RemoveAll(filepath.Join(a.ConfigDir, "setup")); err != nil {
		t.Fatal(err)
	}
	configure(t, a)
	if undone := unconfigure(t, a, relay.Token); len(undone.Paths) != 1 || len(undone.Notes) > 0 {
		t.Fatalf("unconfigure: %+v", undone)
	}
	if data, _ := os.ReadFile(settings); string(data) != handWritten {
		t.Fatalf("settings after undo:\n%s", data)
	}
}

// Settings terma cannot parse are left and said, without failing setup; ones it never
// pointed at the relay are not parsed at all; a VS Code Copilot never ran in gets none.
func TestSettingsTermaLeavesAlone(t *testing.T) {
	a, settings := sandbox(t)
	broken := "{ \"editor.fontSize\": 13,, }\n"
	if err := os.WriteFile(settings, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := configure(t, a); len(res.Paths) > 0 || !noted(res, "does not parse") || noted(res, "Restart") {
		t.Fatalf("a settings file that does not parse: %+v", res)
	}
	if data, _ := os.ReadFile(settings); string(data) != broken {
		t.Fatalf("settings changed:\n%s", data)
	}
	if left, _ := filepath.Glob(filepath.Join(a.ConfigDir, "setup", "*")); len(left) > 0 {
		t.Errorf("a record of settings terma never wrote: %v", left)
	}
	for _, token := range []string{relay.Token, ""} {
		if undone := unconfigure(t, a, token); len(undone.Paths) > 0 {
			t.Fatalf("unconfigure(%q) = %+v", token, undone)
		}
	}
	if err := os.RemoveAll(filepath.Join(filepath.Dir(settings), "globalStorage")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := configure(t, a); len(res.Paths) > 0 || len(res.Notes) > 0 {
		t.Fatalf("configure with no Copilot in VS Code: %+v", res)
	}
	if data, _ := os.ReadFile(settings); string(data) != "{}\n" {
		t.Fatalf("wrote settings for an editor Copilot never ran in:\n%s", data)
	}
	if _, known := a.RelayPointed("127.0.0.1:43180"); known {
		t.Error("claims to know where a CLI's environment points")
	}
}

// A VS Code with no settings file yet gets one holding only terma's members, a write that
// fails leaves no record behind, and a lost record still lets the undo take the members
// out by the relay's token.
func TestConnectCreatesSettingsAndUndoesWithoutItsRecord(t *testing.T) {
	a, settings := sandbox(t)
	// The settings directory cannot be written: nothing is recorded as installed.
	if err := os.Chmod(filepath.Dir(settings), 0o500); err != nil {
		t.Fatal(err)
	}
	if res := configure(t, a); len(res.Paths) > 0 || !noted(res, "exports nothing") {
		t.Fatalf("configure into a read-only directory: %+v", res)
	}
	if left, _ := filepath.Glob(filepath.Join(a.ConfigDir, "setup", "*")); len(left) > 0 {
		t.Fatalf("a failed write left its record: %v", left)
	}
	if err := os.Chmod(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}

	configure(t, a)
	if doc := settingsOf(t, settings); len(doc) != 4 || doc[hostEndpoint] != "http://127.0.0.1:43180/tok-0123456789abcdef/v1/traces" {
		t.Fatalf("new settings: %v", doc)
	}
	if err := os.RemoveAll(filepath.Join(a.ConfigDir, "setup")); err != nil {
		t.Fatal(err)
	}
	if undone := unconfigure(t, a, "another-token"); len(undone.Paths) > 0 {
		t.Fatalf("unconfigure with another relay's token: %+v", undone)
	}
	if undone := unconfigure(t, a, relay.Token); len(undone.Paths) != 1 {
		t.Fatalf("unconfigure without the record: %+v", undone)
	}
	if data, _ := os.ReadFile(settings); string(data) != "{}\n" {
		t.Fatalf("settings after undo:\n%q", data)
	}
}

// A sync-ignore list that is not a list is left as it is, and the developer told what to
// add to it: unsaid, the endpoint and its token would follow them to another machine.
func TestConnectSaysWhenItCannotKeepSettingsOffSync(t *testing.T) {
	a, settings := sandbox(t)
	if err := os.WriteFile(settings, []byte("{\n  \"settingsSync.ignoredSettings\": \"editor.fontSize\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := configure(t, a)
	if !noted(res, syncIgnored) || !noted(res, hostEndpoint) {
		t.Fatalf("notes: %q", res.Notes)
	}
	if doc := settingsOf(t, settings); doc[syncIgnored] != "editor.fontSize" || doc[hostEnabled] != true {
		t.Fatalf("settings: %v", doc)
	}
}

// VS Code reads an exporter endpoint from its environment ahead of its settings, and
// takes that environment from the developer's shell. With one exported that leaves this
// machine, setup writes no settings, takes back the ones it wrote before, and says why:
// its content switch would otherwise send what was said to that collector. An endpoint on
// this machine, such as the relay as another agent's exporter names it, is no reason.
func TestSetupLeavesVSCodeAloneUnderAShellEndpoint(t *testing.T) {
	a, settings := sandbox(t)
	if err := os.WriteFile(settings, []byte(handWritten), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:43180")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://localhost:4318/v1/traces")
	if res := configure(t, a); len(res.Paths) != 1 {
		t.Fatalf("configure beside endpoints on this machine: %+v", res)
	}
	if _, _, problem := a.RelayProblem(); problem {
		t.Fatal("a problem beside endpoints on this machine")
	}

	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://user:secret@otel.example.com/v1/traces")
	res := configure(t, a)
	if len(res.Paths) > 0 || !noted(res, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is exported in your shell") {
		t.Fatalf("configure beside a collector elsewhere: %+v", res)
	}
	if strings.Contains(strings.Join(res.Notes, " "), "secret") {
		t.Fatalf("a note printed the endpoint, which holds a credential: %q", res.Notes)
	}
	if data, _ := os.ReadFile(settings); string(data) != handWritten {
		t.Fatalf("the settings an earlier setup wrote are still there:\n%s", data)
	}
	if pointed, known := a.RelayPointed("127.0.0.1:43180"); pointed || known {
		t.Errorf("RelayPointed = %v, %v: setup running again would not fix this", pointed, known)
	}
	detail, fix, problem := a.RelayProblem()
	if !problem || !strings.Contains(detail, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") || !strings.Contains(fix, "unset") || strings.Contains(detail+fix, "secret") {
		t.Fatalf("RelayProblem = %q, %q, %v", detail, fix, problem)
	}
}
