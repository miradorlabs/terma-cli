package harness

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// localClaudeIn builds a repository with a project settings file already holding
// something Terma did not write — the session hooks `terma install` puts there — and
// returns the harness bound to it. The user-level file and Terma's own directory are
// sandboxed too, so nothing here can touch the developer's real configuration.
func localClaudeIn(t *testing.T, settings string) (Harness, string) {
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
	return Claude{}.Local(repo), path
}

const hooksOnly = `{
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "terma hook session-start"}]}]
  }
}
`

func localExporter() Exporter {
	e := fullExporter()
	e.IncludePrompts, e.IncludeToolContent = true, true
	return e
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// A project file is committed and read by everyone who clones the repository. It may
// say what to ship; it must never say where, or with which key.
func TestLocalRenderCarriesOnlyWhatToShip(t *testing.T) {
	local, _ := localClaudeIn(t, "")
	h := local.(Claude)
	env := h.render(localExporter())

	for _, forbidden := range []string{
		claudeEnableTelemetry, otelEndpoint, otelHeaders, otelProtocol, otelResourceAttributes,
	} {
		if v, ok := env[forbidden]; ok {
			t.Errorf("local render wrote %s=%q — a project file must not carry it", forbidden, v)
		}
	}
	want := append([]string{}, claudeLocalKeys...)
	sort.Strings(want)
	if got := sortedKeys(env); !reflect.DeepEqual(got, want) {
		t.Fatalf("local render keys = %v, want exactly %v", got, want)
	}
	if env[otelTracesExporter] != exporterOTLP || env[claudeEnhancedTelemetry] != "1" {
		t.Error("traces on must write the exporter and the beta switch it depends on")
	}

	e := localExporter()
	e.Signals = []Signal{SignalLogs}
	env = h.render(e)
	if _, ok := env[claudeEnhancedTelemetry]; ok {
		t.Error("traces off must not opt the repository into the beta")
	}
	if env[otelTracesExporter] != exporterNone {
		t.Error("an unselected signal is written as an explicit none, so the file reads as a policy")
	}
}

// The global render is unchanged by the new field: the zero value is the old harness.
func TestGlobalRenderStillCarriesEverything(t *testing.T) {
	env := Claude{}.render(localExporter())
	for _, key := range []string{claudeEnableTelemetry, otelEndpoint, otelProtocol, otelHeaders} {
		if _, ok := env[key]; !ok {
			t.Errorf("global render lacks %s", key)
		}
	}
}

func TestScopeOfAndParse(t *testing.T) {
	if ScopeOf(Claude{}) != ScopeGlobal || ScopeOf(Codex{}) != ScopeGlobal {
		t.Error("a bare harness is global")
	}
	if ScopeOf(Claude{}.Local("/repo")) != ScopeLocal {
		t.Error("a bound harness is local")
	}
	if _, ok := Harness(Codex{}).(Scoped); ok {
		t.Error("Codex has one config file and must not claim a repository scope")
	}
	for raw, want := range map[string]Scope{"": ScopeGlobal, "global": ScopeGlobal, " Local ": ScopeLocal} {
		got, err := ParseScope(raw)
		if err != nil || got != want {
			t.Errorf("ParseScope(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := ParseScope("repo"); err == nil {
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

// The project file already carries the session hooks; a local connect adds an env
// block beside them and touches nothing else. Disconnect puts the file back exactly.
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
	env := envOf(t, path)
	if env[otelLogUserPrompts] != "0" || env[otelLogToolContent] != "1" || env[otelLogsExporter] != exporterOTLP {
		t.Fatalf("env = %v", env)
	}
	for _, forbidden := range []string{claudeEnableTelemetry, otelEndpoint, otelHeaders} {
		if _, ok := env[forbidden]; ok {
			t.Errorf("%s landed in the project file", forbidden)
		}
	}
	if _, ok := doc[claudeOtelHeadersHelper]; ok {
		t.Error("a headers helper has no place in a project file")
	}
	// No credential in the file, so its mode is the user's own, not 0600.
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o, want 0644 — nothing secret was written", info.Mode().Perm())
	}

	// The user file was never touched — not even created.
	userPath, _ := Claude{}.ConfigPath()
	if _, err := os.Stat(userPath); err == nil {
		t.Error("a local connect wrote the user-level settings file")
	}

	result, err := h.Disconnect()
	if err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if result.Removed != len(claudeLocalKeys) || result.Unjournaled {
		t.Fatalf("disconnect = %+v, want %d removals from the journal", result, len(claudeLocalKeys))
	}
	after, _ := os.ReadFile(path)
	if envOf(t, path) != nil {
		t.Fatalf("env block survived disconnect:\n%s", after)
	}
	if _, ok := readJSON(t, path)["hooks"]; !ok {
		t.Fatalf("hooks lost on disconnect:\n%s", after)
	}
}

// Someone else's endpoint in the project file is a conflict for the connect to report —
// never something the local layer deletes on the way past because its own render
// happens not to include that key.
func TestLocalConnectLeavesAProjectEndpointAlone(t *testing.T) {
	h, path := localClaudeIn(t, `{"env":{"OTEL_EXPORTER_OTLP_ENDPOINT":"https://other.example.com","FOO":"bar"}}`)

	conflicts, err := h.ConflictsWith(localExporter())
	if err != nil {
		t.Fatal(err)
	}
	c := findConflict(conflicts, otelEndpoint)
	if c == nil {
		t.Fatalf("expected the project endpoint to be reported, got %+v", conflicts)
	}
	if c.Scope != ScopeRepositorySettings || !c.Clearable {
		t.Errorf("got scope=%q clearable=%v; a setting in the file being written is the repository's, and --force may clear it", c.Scope, c.Clearable)
	}

	if err := h.Connect(localExporter(), false); err != nil {
		t.Fatal(err)
	}
	env := envOf(t, path)
	if env[otelEndpoint] != "https://other.example.com" || env["FOO"] != "bar" {
		t.Fatalf("local connect changed keys it does not own: %v", env)
	}
}

// A local layer says what to ship. It is present, not connected: the destination and
// the key are the global connect's, and status has to let the caller tell the two apart.
func TestLocalStatusReportsPresenceNotConnection(t *testing.T) {
	h, _ := localClaudeIn(t, hooksOnly)
	e := localExporter()
	e.Signals = []Signal{SignalTraces, SignalLogs}
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
	if !reflect.DeepEqual(st.Signals, []Signal{SignalTraces, SignalLogs}) {
		t.Errorf("signals = %v", st.Signals)
	}
	if !st.IncludePrompts || st.IncludeToolContent {
		t.Errorf("prompts=%v tool=%v, want on/off", st.IncludePrompts, st.IncludeToolContent)
	}
	if st.ManagedKeys != len(claudeLocalKeys) {
		t.Errorf("managed keys = %d, want %d", st.ManagedKeys, len(claudeLocalKeys))
	}
	if len(st.Conflicts) != 0 {
		t.Errorf("unexpected conflicts: %+v", st.Conflicts)
	}
}

// Without a journal only a value Terma writes is Terma's to remove: a developer's own
// exporter ("console") and switch values Terma never renders stay where they are.
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

	// Nothing of Terma's: nothing removed, and not reported as an unjournaled removal.
	h, path = localClaudeIn(t, `{"env":{"OTEL_LOGS_EXPORTER":"console"}}`)
	if result, err := h.Disconnect(); err != nil || result.Removed != 0 || result.Unjournaled {
		t.Fatalf("result = %+v, err %v", result, err)
	}
	if env := envOf(t, path); env[otelLogsExporter] != "console" {
		t.Fatalf("env %v", env)
	}
}

// A colleague's committed layer arrives without this machine's journal. Disconnect still
// removes exactly the local keys, and leaves the rest of the env block — including an
// endpoint a global disconnect would have owned — where it found it.
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
	if env[otelEndpoint] == "" || env[claudeEnableTelemetry] == "" {
		t.Fatalf("local disconnect removed global keys: %v", env)
	}
	if _, ok := env[otelTracesExporter]; ok {
		t.Fatal("the local exporter key survived")
	}
}

// What outranks the repository's settings.json is its settings.local.json and the shell.
// The user file is below it and the working directory is irrelevant: a redirect in
// either must not be reported against a local connect.
func TestLocalConflictsComeFromWhatOutranksTheProjectFile(t *testing.T) {
	h, path := localClaudeIn(t, "")
	repo := filepath.Dir(filepath.Dir(path))

	// A user-file redirect: below the project file, so not a conflict here.
	userPath, _ := Claude{}.ConfigPath()
	if err := os.WriteFile(userPath, []byte(`{"env":{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"https://elsewhere.example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Run from an unrelated repository with its own project override: that is not the
	// repository being connected, so it is noise.
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

	// The same repository's settings.local.json does outrank it.
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.local.json"),
		[]byte(`{"env":{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"https://mine.example.com","OTEL_LOG_TOOL_CONTENT":"0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	conflicts, err = h.ConflictsWith(localExporter())
	if err != nil {
		t.Fatal(err)
	}
	redirect := findConflict(conflicts, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if redirect == nil || redirect.Clearable || redirect.Scope != ScopeProject {
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

// Seen from the global connect, a repository's Terma policy is still an override of the
// user file — and is named as one the user set on purpose, with the command to change it.
func TestGlobalConflictsNameTermaOwnedLocalLayer(t *testing.T) {
	h, path := localClaudeIn(t, hooksOnly)
	e := localExporter()
	e.IncludePrompts = false
	if err := h.Connect(e, false); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(filepath.Dir(path)))

	conflicts, err := Claude{}.ConflictsWith(localExporter())
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
	// The local layer's exporter switches are not redirects: nothing blocking.
	if blocking := unclearableKeys(conflicts); len(blocking) != 0 {
		t.Errorf("a Terma-written local layer must never block the global connect: %v", blocking)
	}
}

func unclearableKeys(conflicts []Conflict) []string {
	var out []string
	for _, c := range conflicts {
		if !c.Clearable && !c.Advisory {
			out = append(out, c.Key)
		}
	}
	return out
}
