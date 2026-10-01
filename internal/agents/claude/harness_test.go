package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// claudeIn points the config, and terma's journal directory, at a temp dir.
func claudeIn(t *testing.T, contents string) (exporter, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(dir, "terma"))

	path := filepath.Join(dir, "settings.json")
	if contents != "" {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("seed settings: %v", err)
		}
	}
	return exporter{}, path
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

func fullExporter() harness.Exporter {
	return harness.Exporter{
		Endpoint:  "https://otel.terma.ai",
		APIKey:    "ter_srv_0123456789abcdef",
		ProjectID: "proj_123",
		Signals:   harness.AllSignals,
		ResourceAttributes: map[string]string{
			harness.AttrServiceName: "claude-code",
			harness.AttrEnduserID:   "dev@example.com",
			harness.AttrProjectID:   "proj_123",
		},
	}
}

func TestRenderDefaultsExcludeContent(t *testing.T) {
	env := exporter{}.render(fullExporter())

	want := map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY":        "1",
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "1",
		"OTEL_TRACES_EXPORTER":                "otlp",
		"OTEL_LOGS_EXPORTER":                  "otlp",
		"OTEL_METRICS_EXPORTER":               "otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL":         "http/protobuf",
		"OTEL_EXPORTER_OTLP_ENDPOINT":         "https://otel.terma.ai",
		"OTEL_EXPORTER_OTLP_HEADERS":          "Authorization=Bearer ter_srv_0123456789abcdef",
		"OTEL_LOG_USER_PROMPTS":               "0",
		"OTEL_LOG_ASSISTANT_RESPONSES":        "0",
		"OTEL_LOG_TOOL_DETAILS":               "0",
		"OTEL_LOG_TOOL_CONTENT":               "0",
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("Render() =\n%#v\nwant\n%#v", env, want)
	}
}

// Each content flag turns on only its own switches.
func TestRenderContentSwitchesAreIndependent(t *testing.T) {
	prompts := exporter{}.render(harness.Exporter{Signals: harness.AllSignals, IncludePrompts: true})
	if prompts["OTEL_LOG_USER_PROMPTS"] != "1" || prompts["OTEL_LOG_ASSISTANT_RESPONSES"] != "1" {
		t.Error("--include-prompts did not enable prompt and response capture")
	}
	if prompts["OTEL_LOG_TOOL_DETAILS"] != "0" || prompts["OTEL_LOG_TOOL_CONTENT"] != "0" {
		t.Error("--include-prompts also enabled tool content; the switches must be independent")
	}

	tools := exporter{}.render(harness.Exporter{Signals: harness.AllSignals, IncludeToolContent: true})
	if tools["OTEL_LOG_TOOL_DETAILS"] != "1" || tools["OTEL_LOG_TOOL_CONTENT"] != "1" {
		t.Error("--include-tool-content did not enable tool capture")
	}
	if tools["OTEL_LOG_USER_PROMPTS"] != "0" || tools["OTEL_LOG_ASSISTANT_RESPONSES"] != "0" {
		t.Error("--include-tool-content also enabled prompts; the switches must be independent")
	}
}

// A signal left out is written as an explicit "none".
func TestRenderDisablesUnselectedSignals(t *testing.T) {
	env := exporter{}.render(harness.Exporter{Signals: []harness.Signal{harness.SignalLogs}})

	if env["OTEL_LOGS_EXPORTER"] != "otlp" {
		t.Errorf("logs exporter = %q, want otlp", env["OTEL_LOGS_EXPORTER"])
	}
	for _, key := range []string{"OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER"} {
		if env[key] != "none" {
			t.Errorf("%s = %q, want an explicit none", key, env[key])
		}
	}
	if _, ok := env["CLAUDE_CODE_ENHANCED_TELEMETRY_BETA"]; ok {
		t.Error("the enhanced-telemetry beta was enabled for a connect that asked for no traces")
	}
}

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
	perms, _ := doc["permissions"].(map[string]any)
	allow, _ := perms["allow"].([]any)
	if len(allow) != 1 || allow[0] != "Bash(git:*)" {
		t.Errorf("permissions.allow = %v, want it preserved verbatim", perms["allow"])
	}

	env := envOf(t, path)
	if env["EDITOR"] != "vim" {
		t.Errorf("unrelated env var EDITOR = %q, want vim", env["EDITOR"])
	}
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

// The settings file now holds a live server key.
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

// Merging into an unparseable file would discard settings it cannot see.
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

// Connect then disconnect on an empty file leaves no orphaned `"env": {}`.
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
	if !reflect.DeepEqual(after.Signals, harness.AllSignals) {
		t.Errorf("signals = %v, want %v", after.Signals, harness.AllSignals)
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

// Without the beta flag Claude Code emits no spans, so traces are not on.
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
		if s == harness.SignalTraces {
			t.Fatal("traces reported as on without CLAUDE_CODE_ENHANCED_TELEMETRY_BETA; no span would ever be emitted")
		}
	}
	if !reflect.DeepEqual(st.Signals, []harness.Signal{harness.SignalLogs}) {
		t.Errorf("signals = %v, want just logs", st.Signals)
	}
}

// Status output lands in screenshots.
func TestStatusNeverReturnsTheWholeKey(t *testing.T) {
	const key = "ter_srv_0123456789abcdef0123456789abcdef"
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
	if !strings.HasPrefix(st.KeyPrefix, "ter_srv_") {
		t.Errorf("key prefix %q should stay recognizable enough to match in the web app", st.KeyPrefix)
	}
}

// Writing to ~/.claude while Claude Code reads elsewhere would silently do nothing.
func TestConfigPathHonoursClaudeConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/elsewhere")

	path, err := exporter{}.ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if path != filepath.Join("/tmp/elsewhere", "settings.json") {
		t.Fatalf("ConfigPath = %q, want it under CLAUDE_CONFIG_DIR", path)
	}
}

// A comma or equals sign would corrupt the other OTEL_RESOURCE_ATTRIBUTES entries.
func TestResourceAttributesRejectSeparators(t *testing.T) {
	e := harness.Exporter{ResourceAttributes: map[string]string{
		harness.AttrServiceName: "claude-code",
		harness.AttrEnduserID:   "not,an=email",
		"empty":                 "",
	}}
	if got := e.ResourceAttributesValue(); got != "service.name=claude-code" {
		t.Fatalf("ResourceAttributesValue() = %q, want the malformed and empty pairs dropped", got)
	}
}

func TestParseSignals(t *testing.T) {
	tests := []struct {
		in      string
		want    []harness.Signal
		wantErr bool
	}{
		{in: "", want: harness.AllSignals},
		{in: "traces", want: []harness.Signal{harness.SignalTraces}},
		{in: "metrics,traces", want: []harness.Signal{harness.SignalTraces, harness.SignalMetrics}},
		{in: "logs,logs", want: []harness.Signal{harness.SignalLogs}},
		{in: " TRACES , logs ", want: []harness.Signal{harness.SignalTraces, harness.SignalLogs}},
		{in: "spans", wantErr: true},
		{in: ",", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := harness.ParseSignals(tc.in)
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
	// An empty PATH must read as not installed, not as an error.
	t.Setenv("PATH", t.TempDir())

	if d := (exporter{}).Detect(context.Background()); d.Found {
		t.Fatalf("Detect reported a harness found on an empty PATH: %+v", d)
	}
}
