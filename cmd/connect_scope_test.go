package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// localRepo is a git repository whose .claude/settings.json already carries the hooks
// `terma install` writes, with the user-level file and Terma's directory sandboxed so
// nothing here reaches the developer's real configuration. The test runs from inside it.
func localRepo(t *testing.T) (repo, settings string) {
	t.Helper()
	repo = t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings = filepath.Join(repo, ".claude", "settings.json")
	if err := os.WriteFile(settings, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"terma hook session-start"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Chdir(repo)
	return repo, settings
}

func envIn(t *testing.T, path string) (map[string]string, map[string]json.RawMessage) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	env := map[string]string{}
	if raw, ok := doc["env"]; ok {
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("parse env: %v", err)
		}
	}
	return env, doc
}

// A local connect is the offline half: no project, no key, no sign-in — this test passes
// none of them and would fail with "not signed in" if the command tried to mint.
func TestTelemetryConnectLocalWritesOnlyWhatToShip(t *testing.T) {
	_, settings := localRepo(t)

	out, err := runTerma(t, "connect", "claude", "--scope", "local", "--yes", "--exclude-prompts", "--signals", "traces,logs")
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Written") || !strings.Contains(out, "Commit it") {
		t.Errorf("output should say what was written and to commit it:\n%s", out)
	}
	if !strings.Contains(out, "not connected to Terma on this machine") {
		t.Errorf("with no global connect, the plan must say nothing ships yet:\n%s", out)
	}

	env, doc := envIn(t, settings)
	if _, ok := doc["hooks"]; !ok {
		t.Fatal("the hooks block was lost")
	}
	want := map[string]string{
		"OTEL_TRACES_EXPORTER":         "otlp",
		"OTEL_LOGS_EXPORTER":           "otlp",
		"OTEL_METRICS_EXPORTER":        "none",
		"OTEL_LOG_USER_PROMPTS":        "0",
		"OTEL_LOG_ASSISTANT_RESPONSES": "0",
		"OTEL_LOG_TOOL_DETAILS":        "1",
		"OTEL_LOG_TOOL_CONTENT":        "1",
	}
	for key, value := range want {
		if env[key] != value {
			t.Errorf("%s = %q, want %q", key, env[key], value)
		}
	}
	for _, forbidden := range []string{"CLAUDE_CODE_ENABLE_TELEMETRY", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_RESOURCE_ATTRIBUTES"} {
		if v, ok := env[forbidden]; ok {
			t.Errorf("%s=%q landed in a committed file", forbidden, v)
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")); err == nil {
		t.Error("a local connect touched the user-level settings file")
	}
}

func TestTelemetryConnectLocalRefusesCodex(t *testing.T) {
	localRepo(t)
	out, err := runTerma(t, "connect", "codex", "--scope", "local", "--yes")
	if err == nil || !strings.Contains(err.Error(), "no repository settings") {
		t.Fatalf("err = %v, want a refusal naming the missing repository scope\n%s", err, out)
	}
	// Refused before anything is written, for every harness named.
	_, err = runTerma(t, "connect", "claude", "codex", "--scope", "local", "--yes")
	if err == nil || !strings.Contains(err.Error(), "Codex") {
		t.Fatalf("err = %v, want the multi-harness connect refused up front", err)
	}
}

func TestTelemetryConnectLocalNeedsARepository(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Chdir(t.TempDir())
	_, err := runTerma(t, "connect", "claude", "--scope", "local", "--yes")
	if err == nil || !strings.Contains(err.Error(), "git checkout") {
		t.Fatalf("err = %v, want a message about running inside a repository", err)
	}
}

// The key, its name, the identity and the delivery mode are the global connect's. Taking
// them on a local connect would silently drop them; refusing says where they belong.
func TestTelemetryConnectLocalRejectsGlobalOnlyFlags(t *testing.T) {
	localRepo(t)
	for _, args := range [][]string{
		{"--api-key", "ter_srv_test"},
		{"--identity", "someone@example.com"},
		{"--key-name", "x"},
		{"--inline-key"},
	} {
		_, err := runTerma(t, append([]string{"connect", "claude", "--scope", "local", "--yes"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "global connect") {
			t.Errorf("%v: err = %v, want a refusal", args, err)
		}
	}
	if _, err := runTerma(t, "connect", "claude", "--scope", "repo", "--yes"); err == nil || !strings.Contains(err.Error(), "unknown scope") {
		t.Errorf("unknown scope accepted: %v", err)
	}
}

// The local layer is its own row under the global one, in both renderings. It is never
// "connected": with no global connect it says so.
func TestTelemetryStatusListsTheLocalLayer(t *testing.T) {
	localRepo(t)
	if out, err := runTerma(t, "connect", "claude", "--scope", "local", "--yes", "--exclude-tool-content"); err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}

	out, err := runTerma(t, "harness", "status", "claude", "-o", "json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var report telemetryStatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if len(report.Harnesses) != 2 {
		t.Fatalf("got %d entries, want the global row and the local one:\n%s", len(report.Harnesses), out)
	}
	global, local := report.Harnesses[0], report.Harnesses[1]
	if global.Scope != "global" || global.State != "not connected" {
		t.Errorf("global row = %+v", global)
	}
	if local.Scope != "local" || local.State != "local settings, not exporting" {
		t.Errorf("local row = %+v", local)
	}
	if local.Signals != "logs,metrics,traces" || local.Prompts != "on" || local.ToolContent != "off" {
		t.Errorf("local row data = %s / %s / %s", local.Signals, local.Prompts, local.ToolContent)
	}
	if !strings.HasSuffix(local.ConfigPath, filepath.FromSlash(".claude/settings.json")) {
		t.Errorf("local config path = %q", local.ConfigPath)
	}

	table, err := runTerma(t, "harness", "status", "claude", "-o", "table")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(table, "claude (local)") {
		t.Errorf("table lacks the local row:\n%s", table)
	}

	// The one-line view says the same thing beside the agent.
	status, err := runTerma(t, "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, status)
	}
	if !strings.Contains(status, "Local:       Claude Code ships logs,metrics,traces; prompts on; tool content off from this repository (.claude/settings.json)") {
		t.Errorf("terma status lacks the local line:\n%s", status)
	}
}

// Outside a repository, or in one without a layer, nothing extra is reported — the JSON
// shape existing consumers read is unchanged apart from the scope field.
func TestTelemetryStatusWithoutALocalLayerIsOneRow(t *testing.T) {
	localRepo(t)
	out, err := runTerma(t, "harness", "status", "claude", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var report telemetryStatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Harnesses) != 1 || report.Harnesses[0].Scope != "global" {
		t.Fatalf("got %+v, want one global row", report.Harnesses)
	}
}

func TestTelemetryDisconnectLocalRestoresTheFile(t *testing.T) {
	_, settings := localRepo(t)
	before, _ := os.ReadFile(settings)
	if out, err := runTerma(t, "connect", "claude", "--scope", "local", "--yes"); err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}

	// The global file has nothing to remove, and says so about the right layer.
	out, err := runTerma(t, "disconnect", "claude", "--yes")
	if err != nil || !strings.Contains(out, "no Terma telemetry settings. Nothing to do.") {
		t.Fatalf("global disconnect: %v\n%s", err, out)
	}
	env, _ := envIn(t, settings)
	if env["OTEL_TRACES_EXPORTER"] != "otlp" {
		t.Fatal("a global disconnect reached into the repository's file")
	}

	out, err = runTerma(t, "disconnect", "claude", "--scope", "local", "--yes")
	if err != nil {
		t.Fatalf("local disconnect: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Removed") || !strings.Contains(out, "Commit the change") || strings.Contains(out, "Disconnected.") {
		t.Errorf("local disconnect should report a removal, not a disconnect:\n%s", out)
	}
	if strings.Contains(out, "server key") {
		t.Errorf("no key was involved, none should be mentioned:\n%s", out)
	}
	after, _ := os.ReadFile(settings)
	var a, b map[string]any
	if err := json.Unmarshal(before, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &b); err != nil {
		t.Fatal(err)
	}
	if _, hasEnv := b["env"]; hasEnv || len(a) != len(b) {
		t.Fatalf("file not restored:\nbefore %s\nafter  %s", before, after)
	}

	out, err = runTerma(t, "disconnect", "claude", "--scope", "local", "--yes")
	if err != nil || !strings.Contains(out, "no Terma telemetry settings in this repository") {
		t.Fatalf("second local disconnect: %v\n%s", err, out)
	}
}

// The checklist itself needs a terminal, but the refusal that comes before it does not: a
// --scope local that cannot be honoured is an error, never a quiet flip to global.
func TestAskConnectOptionsRefusesImpossibleLocalBeforePrompting(t *testing.T) {
	_, err := askConnectOptions(connectFlags{scope: "local"}, []harness.Harness{harness.Codex{}}, connectForm{root: "/repo"})
	if err == nil || !strings.Contains(err.Error(), "Codex has no repository settings") {
		t.Fatalf("err = %v", err)
	}
	_, err = askConnectOptions(connectFlags{scope: "local"}, []harness.Harness{harness.Claude{}}, connectForm{})
	if err == nil || !strings.Contains(err.Error(), "not inside a git repository") {
		t.Fatalf("err = %v", err)
	}
	if got := localUnavailable([]harness.Harness{harness.Claude{}}, "/repo"); got != "" {
		t.Errorf("Claude Code in a repository must be offered local scope, got %q", got)
	}
	for names, want := range map[string]string{
		"":                  "your coding agents",
		"Claude Code":       "Claude Code",
		"Claude Code,Codex": "Claude Code and Codex",
		"a,b,c":             "a, b and c",
	} {
		var list []string
		if names != "" {
			list = strings.Split(names, ",")
		}
		if got := joinNames(list); got != want {
			t.Errorf("joinNames(%q) = %q, want %q", names, got, want)
		}
	}
}
