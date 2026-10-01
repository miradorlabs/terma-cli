package claude

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// localClaudeIn builds a sandboxed repository whose project file already holds terma's session
// hooks and returns the harness bound to it.
func localClaudeIn(t *testing.T, settings string) (harness.Harness, string) {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, ".claude", "settings.json")
	if settings != "" {
		if err := os.WriteFile(path, []byte(settings), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sandbox := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", sandbox)
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(sandbox, "terma"))
	h, _ := exporter{}.Local(repo)
	return h, path
}

const hooksOnly = `{
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "terma hook session-start"}]}]
  }
}
`

func localExporter() harness.Exporter {
	e := fullExporter()
	e.IncludePrompts, e.IncludeToolContent = true, true
	return e
}

// A committed project file can only switch telemetry off: Claude Code ignores one that
// enables or redirects it, so all-on writes nothing.
func TestLocalRenderCarriesOnlyOffValues(t *testing.T) {
	local, _ := localClaudeIn(t, "")
	h := local.(exporter)
	if env := h.render(localExporter()); len(env) != 0 {
		t.Fatalf("an all-on repository wrote %v", env)
	}

	e := localExporter()
	e.Signals = []harness.Signal{harness.SignalLogs}
	e.IncludePrompts = false
	want := map[string]string{
		otelTracesExporter: exporterNone, otelMetricsExporter: exporterNone,
		otelLogUserPrompts: "0", otelLogAssistantResponse: "0",
	}
	if env := h.render(e); !reflect.DeepEqual(env, want) {
		t.Fatalf("local render = %v, want %v", env, want)
	}
}

// The zero value is still the global harness.
func TestGlobalRenderStillCarriesEverything(t *testing.T) {
	env := exporter{}.render(localExporter())
	for _, key := range []string{claudeEnableTelemetry, harness.EnvOTLPEndpoint, harness.EnvOTLPProtocol, harness.EnvOTLPHeaders} {
		if _, ok := env[key]; !ok {
			t.Errorf("global render lacks %s", key)
		}
	}
}

func TestScopeOfAndParse(t *testing.T) {
	if _, ok := (exporter{}).Local("/repo"); !ok {
		t.Error("Claude Code has a repository scope")
	}
	for raw, want := range map[string]harness.Scope{"": harness.ScopeGlobal, "global": harness.ScopeGlobal, " Local ": harness.ScopeLocal} {
		got, err := harness.ParseScope(raw)
		if err != nil || got != want {
			t.Errorf("ParseScope(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := harness.ParseScope("repo"); err == nil {
		t.Error("an unknown scope must be refused")
	}
}

func TestLocalConfigPathIsTheProjectFile(t *testing.T) {
	h, path := localClaudeIn(t, "")
	got, err := h.ConfigPath()
	if err != nil || got != path {
		t.Fatalf("ConfigPath = %q, %v; want %q", got, err, path)
	}
	// CLAUDE_CONFIG_DIR moves the user file, not the repository's.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if got, _ := h.ConfigPath(); got != path {
		t.Fatalf("ConfigPath moved with CLAUDE_CONFIG_DIR to %q", got)
	}
}

// A local connect adds its off values beside the hooks, and disconnect puts the file back exactly.
func TestLocalConnectAndDisconnectRoundTrip(t *testing.T) {
	h, path := localClaudeIn(t, hooksOnly)

	e := localExporter()
	e.IncludePrompts = false
	if err := h.Connect(e, false); err != nil {
		t.Fatalf("connect: %v", err)
	}
	doc := readJSON(t, path)
	if _, ok := doc["hooks"]; !ok {
		t.Fatal("connect dropped the hooks block")
	}
	if env := envOf(t, path); !reflect.DeepEqual(env, map[string]string{otelLogUserPrompts: "0", otelLogAssistantResponse: "0"}) {
		t.Fatalf("env = %v", env)
	}
	if _, ok := doc[claudeOtelHeadersHelper]; ok {
		t.Error("a headers helper has no place in a project file")
	}
	// No credential in the file, so its mode is the user's own, not 0600.
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o, want 0644 — nothing secret was written", info.Mode().Perm())
	}

	userPath, _ := exporter{}.ConfigPath()
	if _, err := os.Stat(userPath); err == nil {
		t.Error("a local connect wrote the user-level settings file")
	}

	result, err := h.Disconnect()
	if err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if result.Removed != 2 || result.Unjournaled {
		t.Fatalf("disconnect = %+v, want 2 removals from the journal", result)
	}
	after, _ := os.ReadFile(path)
	if envOf(t, path) != nil {
		t.Fatalf("env block survived disconnect:\n%s", after)
	}
	if _, ok := readJSON(t, path)["hooks"]; !ok {
		t.Fatalf("hooks lost on disconnect:\n%s", after)
	}
}

// An earlier terma's on values are cleared by the next connect, the hooks kept.
func TestLocalConnectClearsAnEarlierTermasOnValues(t *testing.T) {
	h, path := localClaudeIn(t, `{"env":{"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA":"1","OTEL_LOGS_EXPORTER":"otlp","OTEL_METRICS_EXPORTER":"otlp","OTEL_TRACES_EXPORTER":"otlp","OTEL_LOG_USER_PROMPTS":"1","OTEL_LOG_ASSISTANT_RESPONSES":"1","OTEL_LOG_TOOL_DETAILS":"1","OTEL_LOG_TOOL_CONTENT":"1"},"hooks":{"Stop":[]}}`)
	if st, err := h.Status(); err != nil || st.HasPolicy {
		t.Fatalf("on values read as a policy: %+v, %v", st, err)
	}
	if err := h.Connect(localExporter(), false); err != nil {
		t.Fatal(err)
	}
	if env := envOf(t, path); env != nil {
		t.Fatalf("on values survived: %v", env)
	}
	if _, ok := readJSON(t, path)["hooks"]; !ok {
		t.Fatal("hooks lost")
	}
}

// A project file's own endpoint is a conflict to report, never something the local layer deletes.
func TestLocalConnectLeavesAProjectEndpointAlone(t *testing.T) {
	h, path := localClaudeIn(t, `{"env":{"OTEL_EXPORTER_OTLP_ENDPOINT":"https://other.example.com","FOO":"bar"}}`)

	conflicts, err := h.ConflictsWith(localExporter())
	if err != nil {
		t.Fatal(err)
	}
	c := findConflict(conflicts, harness.EnvOTLPEndpoint)
	if c == nil {
		t.Fatalf("expected the project endpoint to be reported, got %+v", conflicts)
	}
	if c.Scope != scopeRepositorySettings || !c.Clearable {
		t.Errorf("got scope=%q clearable=%v; a setting in the file being written is the repository's, and --force may clear it", c.Scope, c.Clearable)
	}

	if err := h.Connect(localExporter(), false); err != nil {
		t.Fatal(err)
	}
	env := envOf(t, path)
	if env[harness.EnvOTLPEndpoint] != "https://other.example.com" || env["FOO"] != "bar" {
		t.Fatalf("local connect changed keys it does not own: %v", env)
	}
}

// A local layer is a policy: present, not connected, and what it leaves unsaid is inherited.
func TestLocalStatusReportsPresenceNotConnection(t *testing.T) {
	h, _ := localClaudeIn(t, hooksOnly)
	e := localExporter()
	e.Signals = []harness.Signal{harness.SignalTraces, harness.SignalLogs}
	e.IncludeToolContent = false
	if err := h.Connect(e, false); err != nil {
		t.Fatal(err)
	}

	st, err := h.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Connected || st.Endpoint != "" || st.KeyPrefix != "" {
		t.Errorf("a local layer must not report itself connected: %+v", st)
	}
	if !reflect.DeepEqual(st.Signals, []harness.Signal{harness.SignalTraces, harness.SignalLogs}) {
		t.Errorf("signals = %v", st.Signals)
	}
	if !st.IncludePrompts || st.IncludeToolContent {
		t.Errorf("prompts=%v tool=%v, want on/off", st.IncludePrompts, st.IncludeToolContent)
	}
	if !st.HasPolicy || st.ManagedKeys != 3 {
		t.Errorf("policy=%v managed keys = %d, want a policy of 3", st.HasPolicy, st.ManagedKeys)
	}
	if len(st.Conflicts) != 0 {
		t.Errorf("unexpected conflicts: %+v", st.Conflicts)
	}
}

// Without a journal only a value terma writes is removed; a developer's own "console" stays.
func TestLocalDisconnectWithoutJournalKeepsValuesTermaNeverWrites(t *testing.T) {
	h, path := localClaudeIn(t, `{"env":{
		"OTEL_LOGS_EXPORTER":"console",
		"OTEL_TRACES_EXPORTER":"otlp",
		"OTEL_LOG_USER_PROMPTS":"true",
		"OTEL_LOG_TOOL_DETAILS":"0"
	}}`)
	result, err := h.Disconnect()
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 2 || !result.Unjournaled {
		t.Fatalf("result = %+v, want the 2 Terma values removed", result)
	}
	env := envOf(t, path)
	if env[otelLogsExporter] != "console" || env[otelLogUserPrompts] != "true" {
		t.Fatalf("a developer's own value was removed: %v", env)
	}
	if _, ok := env[otelTracesExporter]; ok {
		t.Fatal("a value Terma writes survived")
	}
	if _, ok := env[otelLogToolDetails]; ok {
		t.Fatal("a switch value Terma writes survived")
	}

	h, path = localClaudeIn(t, `{"env":{"OTEL_LOGS_EXPORTER":"console"}}`)
	if result, err := h.Disconnect(); err != nil || result.Removed != 0 || result.Unjournaled {
		t.Fatalf("result = %+v, err %v", result, err)
	}
	if env := envOf(t, path); env[otelLogsExporter] != "console" {
		t.Fatalf("env %v", env)
	}
}

// A colleague's committed layer, with no journal here, loses only the local keys.
func TestLocalDisconnectWithoutJournalRemovesOnlyLocalKeys(t *testing.T) {
	h, path := localClaudeIn(t, `{"env":{
		"OTEL_TRACES_EXPORTER":"otlp",
		"OTEL_LOG_USER_PROMPTS":"0",
		"OTEL_EXPORTER_OTLP_ENDPOINT":"https://otel.terma.ai",
		"CLAUDE_CODE_ENABLE_TELEMETRY":"1"
	}}`)
	result, err := h.Disconnect()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Unjournaled || result.Removed != 2 {
		t.Fatalf("result = %+v, want 2 unjournaled removals", result)
	}
	env := envOf(t, path)
	if env[harness.EnvOTLPEndpoint] == "" || env[claudeEnableTelemetry] == "" {
		t.Fatalf("local disconnect removed global keys: %v", env)
	}
	if _, ok := env[otelTracesExporter]; ok {
		t.Fatal("the local exporter key survived")
	}
}

// Only settings.local.json and the shell outrank a repository's settings.json.
func TestLocalConflictsComeFromWhatOutranksTheProjectFile(t *testing.T) {
	h, path := localClaudeIn(t, "")
	repo := filepath.Dir(filepath.Dir(path))

	userPath, _ := exporter{}.ConfigPath()
	if err := os.WriteFile(userPath, []byte(`{"env":{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"https://elsewhere.example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(other, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ".claude", "settings.json"),
		[]byte(`{"env":{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":"https://noise.example.com"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(other)

	conflicts, err := h.ConflictsWith(localExporter())
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %+v", conflicts)
	}

	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.local.json"),
		[]byte(`{"env":{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"https://mine.example.com","OTEL_LOG_TOOL_CONTENT":"0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	conflicts, err = h.ConflictsWith(localExporter())
	if err != nil {
		t.Fatal(err)
	}
	redirect := findConflict(conflicts, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if redirect == nil || redirect.Clearable || redirect.Scope != harness.ScopeProject {
		t.Fatalf("settings.local.json redirect = %+v, want an unclearable project conflict", redirect)
	}
	if !strings.Contains(redirect.Reason, claudeProjectSettings) {
		t.Errorf("reason should name the file it overrides, got %q", redirect.Reason)
	}
	capture := findConflict(conflicts, otelLogToolContent)
	if capture == nil || !capture.Advisory {
		t.Fatalf("settings.local.json capture-off = %+v, want advisory", capture)
	}
}

// From the global connect, a repository's terma policy is named as one, with the command to change it.
func TestGlobalConflictsNameTermaOwnedLocalLayer(t *testing.T) {
	h, path := localClaudeIn(t, hooksOnly)
	e := localExporter()
	e.IncludePrompts = false
	if err := h.Connect(e, false); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(filepath.Dir(path)))

	conflicts, err := exporter{}.ConflictsWith(localExporter())
	if err != nil {
		t.Fatal(err)
	}
	c := findConflict(conflicts, otelLogUserPrompts)
	if c == nil || !c.Advisory {
		t.Fatalf("expected an advisory for the repository policy, got %+v", conflicts)
	}
	if !strings.Contains(c.Reason, "Terma policy") || !strings.Contains(c.Reason, "--scope local") {
		t.Errorf("reason = %q, want it to name the policy and the command", c.Reason)
	}
	if blocking := unclearableKeys(conflicts); len(blocking) != 0 {
		t.Errorf("a Terma-written local layer must never block the global connect: %v", blocking)
	}
}

func unclearableKeys(conflicts []harness.Conflict) []string {
	var out []string
	for _, c := range conflicts {
		if !c.Clearable && !c.Advisory {
			out = append(out, c.Key)
		}
	}
	return out
}
