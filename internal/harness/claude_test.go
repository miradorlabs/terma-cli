package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// claudeIn points Claude Code's config at a temp dir, so every test here works against
// a throwaway file rather than the developer's real ~/.claude/settings.json.
func claudeIn(t *testing.T, contents string) (Claude, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	// Connect also writes its ownership journal. Keep that out of the developer's real
	// ~/.config/terma directory: overwriting a live journal here would make a later real
	// disconnect lose the values it is supposed to restore.
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(dir, "terma"))

	path := filepath.Join(dir, "settings.json")
	if contents != "" {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("seed settings: %v", err)
		}
	}
	return Claude{}, path
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

func envOf(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, ok := readJSON(t, path)["env"]
	if !ok {
		return nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("env is %T, want an object", raw)
	}
	out := map[string]string{}
	for k, v := range obj {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("env[%q] is %T, want string — Claude Code rejects non-string env values", k, v)
		}
		out[k] = s
	}
	return out
}

func fullExporter() Exporter {
	return Exporter{
		Endpoint:  "https://otel.terma.ai",
		APIKey:    "mir_srv_0123456789abcdef",
		ProjectID: "proj_123",
		Signals:   AllSignals,
		ResourceAttributes: map[string]string{
			AttrServiceName: "claude-code",
			AttrEnduserID:   "dev@example.com",
			AttrProjectID:   "proj_123",
		},
	}
}

func TestRenderDefaultsExcludeContent(t *testing.T) {
	env := Claude{}.render(fullExporter())

	want := map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY":        "1",
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "1",
		"OTEL_TRACES_EXPORTER":                "otlp",
		"OTEL_LOGS_EXPORTER":                  "otlp",
		"OTEL_METRICS_EXPORTER":               "otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL":         "http/protobuf",
		"OTEL_EXPORTER_OTLP_ENDPOINT":         "https://otel.terma.ai",
		"OTEL_EXPORTER_OTLP_HEADERS":          "Authorization=Bearer mir_srv_0123456789abcdef",
		"OTEL_LOG_USER_PROMPTS":               "0",
		"OTEL_LOG_ASSISTANT_RESPONSES":        "0",
		"OTEL_LOG_TOOL_DETAILS":               "0",
		"OTEL_LOG_TOOL_CONTENT":               "0",
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("Render() =\n%#v\nwant\n%#v", env, want)
	}
}

// The content switches are the privacy contract. Each must be off unless its own flag
// asked for it, and neither flag may turn on the other's variables.
func TestRenderContentSwitchesAreIndependent(t *testing.T) {
	prompts := Claude{}.render(Exporter{Signals: AllSignals, IncludePrompts: true})
	if prompts["OTEL_LOG_USER_PROMPTS"] != "1" || prompts["OTEL_LOG_ASSISTANT_RESPONSES"] != "1" {
		t.Error("--include-prompts did not enable prompt and response capture")
	}
	if prompts["OTEL_LOG_TOOL_DETAILS"] != "0" || prompts["OTEL_LOG_TOOL_CONTENT"] != "0" {
		t.Error("--include-prompts also enabled tool content; the switches must be independent")
	}

	tools := Claude{}.render(Exporter{Signals: AllSignals, IncludeToolContent: true})
	if tools["OTEL_LOG_TOOL_DETAILS"] != "1" || tools["OTEL_LOG_TOOL_CONTENT"] != "1" {
		t.Error("--include-tool-content did not enable tool capture")
	}
	if tools["OTEL_LOG_USER_PROMPTS"] != "0" || tools["OTEL_LOG_ASSISTANT_RESPONSES"] != "0" {
		t.Error("--include-tool-content also enabled prompts; the switches must be independent")
	}
}

// A signal left out is written as an explicit "none" rather than omitted, so the file
// states what it does instead of leaning on an upstream default.
func TestRenderDisablesUnselectedSignals(t *testing.T) {
	env := Claude{}.render(Exporter{Signals: []Signal{SignalLogs}})

	if env["OTEL_LOGS_EXPORTER"] != "otlp" {
		t.Errorf("logs exporter = %q, want otlp", env["OTEL_LOGS_EXPORTER"])
	}
	for _, key := range []string{"OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER"} {
		if env[key] != "none" {
			t.Errorf("%s = %q, want an explicit none", key, env[key])
		}
	}
	// The beta flag opts into an unreleased feature; a logs-only connect must not.
	if _, ok := env["CLAUDE_CODE_ENHANCED_TELEMETRY_BETA"]; ok {
		t.Error("the enhanced-telemetry beta was enabled for a connect that asked for no traces")
	}
}

// Merging must never cost the user a setting this CLI does not know about.
func TestConnectPreservesUnrelatedSettings(t *testing.T) {
	c, path := claudeIn(t, `{
  "model": "opus",
  "permissions": {"allow": ["Bash(git:*)"], "deny": []},
  "hooks": {"Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "echo done"}]}]},
  "env": {"EDITOR": "vim", "OTEL_LOG_USER_PROMPTS": "1"}
}`)

	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	doc := readJSON(t, path)
	if doc["model"] != "opus" {
		t.Errorf("model = %v, want it preserved", doc["model"])
	}
	for _, key := range []string{"permissions", "hooks"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("%q was dropped by the merge", key)
		}
	}
	// A nested object must survive intact, not be flattened or reordered into nonsense.
	perms, _ := doc["permissions"].(map[string]any)
	allow, _ := perms["allow"].([]any)
	if len(allow) != 1 || allow[0] != "Bash(git:*)" {
		t.Errorf("permissions.allow = %v, want it preserved verbatim", perms["allow"])
	}

	env := envOf(t, path)
	if env["EDITOR"] != "vim" {
		t.Errorf("unrelated env var EDITOR = %q, want vim", env["EDITOR"])
	}
	// A stale value from a previous connect must be overwritten, not merged around.
	if env["OTEL_LOG_USER_PROMPTS"] != "0" {
		t.Errorf("OTEL_LOG_USER_PROMPTS = %q, want the new value 0", env["OTEL_LOG_USER_PROMPTS"])
	}
}

func TestConnectCreatesFileWhenAbsent(t *testing.T) {
	c, path := claudeIn(t, "")

	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if envOf(t, path)["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
		t.Error("telemetry was not enabled in a freshly created settings file")
	}
}

// The settings file now holds a live server key. Leaving it world-readable would put
// that credential in reach of every account on the machine.
func TestConnectTightensFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	c, path := claudeIn(t, `{"model":"opus"}`)

	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("settings mode = %#o, want no group/other access — the file holds a server key", mode)
	}
}

// A file we cannot parse must not be written to: merging into it would mean discarding
// settings we were never able to see.
func TestConnectRefusesMalformedSettings(t *testing.T) {
	const original = `{"model": "opus",,,`
	c, path := claudeIn(t, original)

	if err := c.Connect(fullExporter(), false); err == nil {
		t.Fatal("Connect succeeded against an unparseable settings file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != original {
		t.Fatal("the unparseable file was modified; it must be left exactly as found")
	}
}

func TestDisconnectRemovesOnlyManagedKeys(t *testing.T) {
	c, path := claudeIn(t, `{"model":"opus","env":{"EDITOR":"vim","MY_VAR":"keep"}}`)

	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	// Every managed key was absent before this connect, so undoing it removes them all
	// rather than restoring anything.
	if result.Removed != len(claudeManagedKeys) {
		t.Errorf("removed %d keys, want %d", result.Removed, len(claudeManagedKeys))
	}

	env := envOf(t, path)
	if env["EDITOR"] != "vim" || env["MY_VAR"] != "keep" {
		t.Errorf("unrelated env survived as %v, want EDITOR and MY_VAR intact", env)
	}
	for _, key := range claudeManagedKeys {
		if _, ok := env[key]; ok {
			t.Errorf("%s survived disconnect", key)
		}
	}
	if readJSON(t, path)["model"] != "opus" {
		t.Error("disconnect dropped an unrelated top-level setting")
	}
}

// Connect then disconnect on an otherwise-empty file should leave no trace, not an
// orphaned `"env": {}`.
func TestDisconnectRestoresAnUntouchedFile(t *testing.T) {
	c, path := claudeIn(t, `{"model":"opus"}`)

	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	doc := readJSON(t, path)
	if _, ok := doc["env"]; ok {
		t.Errorf("an empty env object was left behind: %v", doc)
	}
	if doc["model"] != "opus" {
		t.Errorf("model = %v, want it preserved", doc["model"])
	}
}

func TestDisconnectOnCleanFileIsANoop(t *testing.T) {
	c, _ := claudeIn(t, `{"model":"opus"}`)

	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if result.Removed != 0 || result.Restored != 0 {
		t.Errorf("disconnect changed %d keys in a file that was never connected", result.Removed+result.Restored)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	c, _ := claudeIn(t, "")

	before, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if before.Connected || before.Exists {
		t.Errorf("a missing settings file reported as connected=%v exists=%v", before.Connected, before.Exists)
	}

	e := fullExporter()
	e.IncludeToolContent = true
	if err := c.Connect(e, false); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	after, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !after.Connected {
		t.Error("status did not report a freshly connected harness")
	}
	if after.Endpoint != "https://otel.terma.ai" {
		t.Errorf("endpoint = %q", after.Endpoint)
	}
	if !reflect.DeepEqual(after.Signals, AllSignals) {
		t.Errorf("signals = %v, want %v", after.Signals, AllSignals)
	}
	if after.IncludePrompts {
		t.Error("prompts reported on when they were never enabled")
	}
	if !after.IncludeToolContent {
		t.Error("tool content reported off when it was enabled")
	}
	if after.ProjectID != "proj_123" {
		t.Errorf("project = %q, want it read back from the connect journal", after.ProjectID)
	}
}

// The exporter can be set while the beta flag is not, in which case Claude Code emits
// no spans at all. Reporting traces as on there would send someone hunting in Terma
// for data that was never sent.
func TestStatusDoesNotClaimTracesWithoutTheBetaFlag(t *testing.T) {
	c, _ := claudeIn(t, `{"env":{
		"CLAUDE_CODE_ENABLE_TELEMETRY":"1",
		"OTEL_EXPORTER_OTLP_ENDPOINT":"https://otel.terma.ai",
		"OTEL_TRACES_EXPORTER":"otlp",
		"OTEL_LOGS_EXPORTER":"otlp"
	}}`)

	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, s := range st.Signals {
		if s == SignalTraces {
			t.Fatal("traces reported as on without CLAUDE_CODE_ENHANCED_TELEMETRY_BETA; no span would ever be emitted")
		}
	}
	if !reflect.DeepEqual(st.Signals, []Signal{SignalLogs}) {
		t.Errorf("signals = %v, want just logs", st.Signals)
	}
}

// Status output lands in terminals, screenshots and bug reports.
func TestStatusNeverReturnsTheWholeKey(t *testing.T) {
	const key = "mir_srv_0123456789abcdef0123456789abcdef"
	c, _ := claudeIn(t, "")

	e := fullExporter()
	e.APIKey = key
	if err := c.Connect(e, false); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.KeyPrefix == "" {
		t.Fatal("no key prefix reported")
	}
	if strings.Contains(st.KeyPrefix, key) || len(st.KeyPrefix) >= len(key) {
		t.Fatalf("key prefix %q exposes too much of the credential", st.KeyPrefix)
	}
	if !strings.HasPrefix(st.KeyPrefix, "mir_srv_") {
		t.Errorf("key prefix %q should stay recognizable enough to match in the web app", st.KeyPrefix)
	}
}

// Writing to ~/.claude while Claude Code reads elsewhere is a connect that silently
// does nothing.
func TestConfigPathHonoursClaudeConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/elsewhere")

	path, err := Claude{}.ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if path != filepath.Join("/tmp/elsewhere", "settings.json") {
		t.Fatalf("ConfigPath = %q, want it under CLAUDE_CONFIG_DIR", path)
	}
}

// A value carrying a comma or an equals sign would corrupt the whole
// OTEL_RESOURCE_ATTRIBUTES encoding, silently changing the other attributes.
func TestResourceAttributesRejectSeparators(t *testing.T) {
	e := Exporter{ResourceAttributes: map[string]string{
		AttrServiceName: "claude-code",
		AttrEnduserID:   "not,an=email",
		"empty":         "",
	}}
	if got := e.ResourceAttributesValue(); got != "service.name=claude-code" {
		t.Fatalf("ResourceAttributesValue() = %q, want the malformed and empty pairs dropped", got)
	}
}

func TestParseSignals(t *testing.T) {
	tests := []struct {
		in      string
		want    []Signal
		wantErr bool
	}{
		{in: "", want: AllSignals},
		{in: "traces", want: []Signal{SignalTraces}},
		// Canonical order regardless of input order, and deduplicated.
		{in: "metrics,traces", want: []Signal{SignalTraces, SignalMetrics}},
		{in: "logs,logs", want: []Signal{SignalLogs}},
		{in: " TRACES , logs ", want: []Signal{SignalTraces, SignalLogs}},
		{in: "spans", wantErr: true},
		{in: ",", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseSignals(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSignals(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSignals(%q): %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseSignals(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestLookupRejectsUnknownHarness(t *testing.T) {
	if _, err := Lookup("gemini"); err == nil {
		t.Fatal("Lookup accepted an unknown harness")
	}
	for _, name := range Names() {
		if _, err := Lookup(name); err != nil {
			t.Errorf("Lookup(%q): %v", name, err)
		}
	}
}

// Every registered harness must answer a status query against an empty sandbox — a
// stub that errors would make `telemetry status` report it as broken.
func TestEveryHarnessReportsStatusInASandbox(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CODEX_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(dir, "terma"))

	for _, h := range All() {
		st, err := h.Status()
		if err != nil {
			t.Errorf("%s.Status: %v", h.Name(), err)
			continue
		}
		if st.Connected || st.Exists {
			t.Errorf("%s reported connected=%v exists=%v in an empty sandbox", h.Name(), st.Connected, st.Exists)
		}
	}
}

func TestBackupCopiesTheOriginal(t *testing.T) {
	const original = `{"model":"opus"}`
	c, path := claudeIn(t, original)

	backup, err := c.Backup(termaEndpoint)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if backup == "" {
		t.Fatal("no backup path returned for an existing file")
	}
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(data) != original {
		t.Fatalf("backup = %q, want a verbatim copy", data)
	}

	// Connecting afterwards must not disturb the backup.
	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if again, _ := os.ReadFile(backup); string(again) != original {
		t.Fatal("the backup was overwritten by the connect it exists to protect against")
	}
	_ = path
}

func TestBackupIsSkippedWhenThereIsNoFile(t *testing.T) {
	c, _ := claudeIn(t, "")

	backup, err := c.Backup(termaEndpoint)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if backup != "" {
		t.Fatalf("Backup returned %q for a file that does not exist", backup)
	}
}

func TestDetectDoesNotFailWhenClaudeIsAbsent(t *testing.T) {
	// An empty PATH guarantees the lookup misses, which must read as "not installed"
	// rather than as an error — connecting an uninstalled harness is allowed.
	t.Setenv("PATH", t.TempDir())

	if d := (Claude{}).Detect(context.Background()); d.Found {
		t.Fatalf("Detect reported a harness found on an empty PATH: %+v", d)
	}
}
