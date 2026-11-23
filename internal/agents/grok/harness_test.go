package grok

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
)

const (
	relayEndpoint = "http://127.0.0.1:43180"
	relayToken    = "relay-token-0123456789abcdef"
)

// otelVars are every variable a conflict rule reads, cleared for a test.
var otelVars = append([]string{envLogsEndpoint, envLogsExporter, envExternalOTel, envLogsProtocol, envCompression,
	envMetricsExporter, envMetricsEndpoint, harness.EnvOTLPEndpoint, harness.EnvOTLPProtocol, sandboxEnv}, contentSwitches...)

// sandbox is a scratch home with grok's home in it, no OTEL_* of the developer's, and on the
// PATH a grok that prints version (none at all when version is empty). It returns grok's home.
func sandbox(t *testing.T, version string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	grokDir := filepath.Join(home, "grokhome")
	t.Setenv(homeEnv, grokDir)
	for _, key := range otelVars {
		t.Setenv(key, "")
	}
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if version != "" {
		script := "#!/bin/sh\necho 'grok " + version + " (b06756fd6256)'\n"
		if err := os.WriteFile(filepath.Join(bin, "grok"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return grokDir
}

func relayConfig() agents.RelayConfig {
	return agents.RelayConfig{Endpoint: relayEndpoint, Token: relayToken, HookCommand: []string{"terma", "hook"}}
}

func writeConfig(t *testing.T, grokDir, text string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(grokDir, configFileName)
	if err := os.MkdirAll(grokDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func readConfig(t *testing.T, path string) (string, map[string]any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := toml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	table, _ := doc[telemetryTable].(map[string]any)
	return string(data), table
}

// installHooks writes terma's hooks file as setup's hooks step does, for this test binary.
func installHooks(t *testing.T) {
	t.Helper()
	path, err := Agent{}.UserHooksPath()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := procinfo.AbsExecutable()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Agent{}.PlanUserHooks(filepath.Dir(path), hookmgr.UserHookCommand(exe), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(filepath.Dir(path), plan); err != nil {
		t.Fatal(err)
	}
}

const developerConfig = `# my grok
[models]
default = "grok-4.7"

[model.fake-m]
base_url = "http://127.0.0.1:9/v1" # a local model
`

// Connect writes the eight keys and nothing else, keeps the developer's comments, tables and
// keys, and makes the file private; Disconnect brings an untouched file back byte for byte,
// mode included.
func TestConnectAndDisconnectRoundTrip(t *testing.T) {
	grokDir := sandbox(t, "")
	path := writeConfig(t, grokDir, developerConfig, 0o644)
	terma := filepath.Join(t.TempDir(), "terma")
	a := Agent{ConfigDir: terma}
	result, err := a.ConfigureRelay(context.Background(), relayConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(result.Paths, path) || len(result.Notes) != 1 || !strings.Contains(result.Notes[0], "Do you trust the contents of this directory?") {
		t.Errorf("result %+v", result)
	}
	text, table := readConfig(t, path)
	if !strings.HasPrefix(text, developerConfig) {
		t.Errorf("the developer's part changed:\n%s", text)
	}
	want := map[string]any{
		keyEnabled: true, keyLogsExporter: "otlp", keyLogsEndpoint: relayEndpoint + "/" + relayToken + "/v1/logs",
		keyLogsProtocol: "http/protobuf", keyLogUserPrompts: true, keyLogAssistantResponses: true,
		keyLogToolDetails: true, keyLogToolContent: true,
	}
	if len(table) != len(want) {
		t.Errorf("[telemetry] = %v", table)
	}
	for k, v := range want {
		if table[k] != v {
			t.Errorf("%s = %v, want %v", k, table[k], v)
		}
	}
	for _, never := range []string{keyEndpoint, "otel_protocol", keyMetricsExporter} {
		if _, ok := table[never]; ok {
			t.Errorf("wrote %s", never)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v with the relay's token in it", info.Mode().Perm())
	}
	st, err := a.Harness().Status()
	if err != nil || !st.Connected || st.Endpoint != relayEndpoint+"/"+relayToken || st.ManagedKeys != 8 || !slices.Equal(st.Signals, []harness.Signal{harness.SignalLogs}) {
		t.Errorf("status %+v, %v", st, err)
	}
	if ok, known := a.RelayTokenCurrent(relayToken); !ok || !known {
		t.Errorf("token current %v, known %v", ok, known)
	}
	if ok, known := a.RelayTokenCurrent("another"); ok || !known {
		t.Errorf("another token current %v, known %v", ok, known)
	}

	undone, err := a.UnconfigureRelay(context.Background(), relayToken)
	if err != nil || !slices.Contains(undone.Paths, path) || len(undone.Notes) != 0 {
		t.Fatalf("unconfigure %+v, %v", undone, err)
	}
	if text, _ := readConfig(t, path); text != developerConfig {
		t.Errorf("after teardown:\n%s\nwant\n%s", text, developerConfig)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Errorf("mode %v after teardown", info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(filepath.Join(terma, "setup")); len(entries) != 0 {
		t.Errorf("journal left: %v", entries)
	}
	if again, err := a.UnconfigureRelay(context.Background(), relayToken); err != nil || len(again.Paths) != 0 {
		t.Errorf("second teardown %+v, %v", again, err)
	}
}

// With no config.toml, setup creates one holding the eight keys, and teardown removes it.
func TestConnectCreatesAndRemovesTheFile(t *testing.T) {
	grokDir := sandbox(t, "")
	a := Agent{ConfigDir: t.TempDir()}
	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(grokDir, configFileName)
	if _, table := readConfig(t, path); len(table) != 8 {
		t.Errorf("[telemetry] = %v", table)
	}
	if _, err := a.UnconfigureRelay(context.Background(), relayToken); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("config.toml left: %v", err)
	}
}

// Keys the developer set before are journaled and put back as they were; a key changed since
// setup is the developer's, left as it is and named.
func TestDisconnectRestoresEarlierValuesAndSkipsChangedOnes(t *testing.T) {
	grokDir := sandbox(t, "")
	path := writeConfig(t, grokDir, "[telemetry]\notel_enabled = false\notel_log_tool_content = false\notel_endpoint = \"http://collector:4318\"\n", 0o600)
	a := Agent{ConfigDir: t.TempDir()}
	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
		t.Fatal(err)
	}
	text, _ := readConfig(t, path)
	if err := os.WriteFile(path, []byte(strings.Replace(text, "otel_log_user_prompts = true", "otel_log_user_prompts = false", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	undone, err := a.UnconfigureRelay(context.Background(), relayToken)
	if err != nil || len(undone.Notes) != 1 || !strings.Contains(undone.Notes[0], keyLogUserPrompts) {
		t.Fatalf("unconfigure %+v, %v", undone, err)
	}
	_, table := readConfig(t, path)
	want := map[string]any{keyEnabled: false, keyLogToolContent: false, keyEndpoint: "http://collector:4318", keyLogUserPrompts: false}
	if len(table) != len(want) {
		t.Errorf("[telemetry] = %v, want %v", table, want)
	}
	for k, v := range want {
		if table[k] != v {
			t.Errorf("%s = %v, want %v", k, table[k], v)
		}
	}
}

// grok rewrites config.toml in its own form when it saves a setting: every key kept, every
// comment gone. Terma marks nothing with comments, so the file still reads as connected;
// one key changed or gone reads as drift.
func TestStatusReadsTheKeysByValue(t *testing.T) {
	grokDir := sandbox(t, "")
	path := writeConfig(t, grokDir, developerConfig, 0o600)
	a := Agent{ConfigDir: t.TempDir()}
	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
		t.Fatal(err)
	}
	connected := readFile(t, path)
	normalise := func(edit func(map[string]any)) {
		t.Helper()
		doc := map[string]any{}
		if err := toml.Unmarshal([]byte(connected), &doc); err != nil {
			t.Fatal(err)
		}
		if edit != nil {
			edit(doc[telemetryTable].(map[string]any))
		}
		out, err := toml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "#") {
			t.Fatal("the normalised file kept a comment")
		}
		if err := os.WriteFile(path, out, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	normalise(nil)
	if st, err := a.Harness().Status(); err != nil || !st.Connected {
		t.Errorf("a normalised file reads as %+v, %v", st, err)
	}
	for name, edit := range map[string]func(map[string]any){
		"changed":   func(t map[string]any) { t[keyLogToolContent] = false },
		"missing":   func(t map[string]any) { delete(t, keyLogsProtocol) },
		"elsewhere": func(t map[string]any) { t[keyLogsEndpoint] = "http://collector:4318/v1/logs" },
		"no token":  func(t map[string]any) { t[keyLogsEndpoint] = relayEndpoint + "/v1/logs" },
	} {
		normalise(edit)
		if st, err := a.Harness().Status(); err != nil || st.Connected {
			t.Errorf("%s: reads as connected: %+v, %v", name, st, err)
		}
	}
}

// Each shell variable that would send grok's logs elsewhere, switch them off or break them,
// and a metrics exporter that would reach the relay, blocks; a content switch turned off is
// advisory; the generic endpoint and protocol are no conflict, nor the relay's own logs URL.
func TestConflicts(t *testing.T) {
	relayLogs := relayEndpoint + "/" + relayToken + "/v1/logs"
	for _, c := range []struct {
		name     string
		env      map[string]string
		config   string
		key      string
		advisory bool
	}{
		{"logs endpoint", map[string]string{envLogsEndpoint: "http://collector:4318/v1/logs"}, "", envLogsEndpoint, false},
		{"logs exporter", map[string]string{envLogsExporter: "none"}, "", envLogsExporter, false},
		{"stream off", map[string]string{envExternalOTel: "0"}, "", envExternalOTel, false},
		{"logs protocol", map[string]string{envLogsProtocol: "grpc"}, "", envLogsProtocol, false},
		{"compression", map[string]string{envCompression: "none"}, "", envCompression, false},
		{"metrics to the relay", map[string]string{envMetricsExporter: "otlp", harness.EnvOTLPEndpoint: relayEndpoint + "/" + relayToken}, "", envMetricsExporter, false},
		{"metrics to the relay by its own endpoint", map[string]string{envMetricsExporter: "otlp", envMetricsEndpoint: relayEndpoint + "/x/v1/metrics"}, "", envMetricsExporter, false},
		{"config's metrics to the relay", nil, "[telemetry]\notel_metrics_exporter = \"otlp\"\notel_endpoint = \"" + relayEndpoint + "/tok\"\n", telemetryTable + "." + keyMetricsExporter, false},
		{"prompts off", map[string]string{"OTEL_LOG_USER_PROMPTS": "0"}, "", "OTEL_LOG_USER_PROMPTS", true},
		{"tool content off", map[string]string{"OTEL_LOG_TOOL_CONTENT": "false"}, "", "OTEL_LOG_TOOL_CONTENT", true},
		{"generic endpoint", map[string]string{harness.EnvOTLPEndpoint: "http://collector:4318", harness.EnvOTLPProtocol: "grpc"}, "", "", false},
		{"the relay's own logs URL", map[string]string{envLogsEndpoint: relayLogs, envLogsProtocol: "http/protobuf", envLogsExporter: "otlp,console"}, "", "", false},
		{"metrics elsewhere", map[string]string{envMetricsExporter: "otlp", envMetricsEndpoint: "http://collector:4318/v1/metrics", harness.EnvOTLPEndpoint: relayEndpoint}, "", "", false},
		{"metrics off", map[string]string{envMetricsExporter: "none", harness.EnvOTLPEndpoint: relayEndpoint}, "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			grokDir := sandbox(t, "")
			if c.config != "" {
				writeConfig(t, grokDir, c.config, 0o600)
			}
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			got, err := Agent{ConfigDir: t.TempDir()}.Harness().ConflictsWith(harness.Exporter{Endpoint: relayEndpoint, APIKey: relayToken})
			if err != nil {
				t.Fatal(err)
			}
			if c.key == "" {
				if len(got) != 0 {
					t.Errorf("conflicts %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Key != c.key || got[0].Advisory != c.advisory {
				t.Fatalf("conflicts %+v, want %s advisory=%v", got, c.key, c.advisory)
			}
			if strings.Contains(got[0].Value+got[0].Reason, relayToken) {
				t.Errorf("a conflict shows the relay's token: %+v", got[0])
			}
		})
	}
}

// A blocking conflict stops setup before it writes anything: no config.toml, no journal,
// and the error names the variable. An advisory one does not.
func TestConfigureRelayRefusesABlockingConflictFirst(t *testing.T) {
	for key, value := range map[string]string{
		envLogsEndpoint: "http://collector:4318/v1/logs", envLogsExporter: "none", envExternalOTel: "0",
		envLogsProtocol: "grpc", envCompression: "none", envMetricsExporter: "otlp",
	} {
		t.Run(key, func(t *testing.T) {
			grokDir := sandbox(t, "1.0.92")
			t.Setenv(key, value)
			if key == envMetricsExporter {
				t.Setenv(harness.EnvOTLPEndpoint, relayEndpoint)
			}
			terma := t.TempDir()
			_, err := Agent{ConfigDir: terma}.ConfigureRelay(context.Background(), relayConfig())
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("configure: %v", err)
			}
			if _, err := os.Stat(grokDir); !os.IsNotExist(err) {
				t.Errorf("grok's home was written: %v", err)
			}
			if entries, _ := os.ReadDir(terma); len(entries) != 0 {
				t.Errorf("terma's config directory was written: %v", entries)
			}
		})
	}
	t.Run("advisory", func(t *testing.T) {
		sandbox(t, "1.0.92")
		t.Setenv("OTEL_LOG_TOOL_DETAILS", "off")
		if _, err := (Agent{ConfigDir: t.TempDir()}).ConfigureRelay(context.Background(), relayConfig()); err != nil {
			t.Fatal(err)
		}
	})
}

// A grok before 1.0.92 is not offered, and setup writes neither its export nor its hooks.
func TestAnOldGrokIsRefused(t *testing.T) {
	grokDir := sandbox(t, "1.0.91")
	a := Agent{ConfigDir: t.TempDir()}
	if a.Installed(context.Background()) {
		t.Error("an old grok reads as installed")
	}
	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err == nil || !strings.Contains(err.Error(), minimumVersion) {
		t.Errorf("configure: %v", err)
	}
	if _, err := a.PlanUserHooks(filepath.Join(grokDir, "hooks"), hookmgr.UserHookCommand("/bin/terma"), true); err == nil {
		t.Error("hooks planned for an old grok")
	}
	if plan, err := a.PlanUserHooks(filepath.Join(grokDir, "hooks"), hookmgr.UserHookCommand("/bin/terma"), false); err != nil || !plan.Empty() {
		t.Errorf("removal: %+v, %v", plan, err)
	}
	if _, err := os.Stat(grokDir); !os.IsNotExist(err) {
		t.Errorf("grok's home was written: %v", err)
	}
	if detail, _, problem := a.RelayProblem("127.0.0.1:43180"); !problem || !strings.Contains(detail, minimumVersion) {
		t.Errorf("problem %q, %v", detail, problem)
	}
}

// Pointed is the eight keys at this relay, terma's current hooks file and no blocking
// conflict. A sandbox is a warning only: the export still reaches the relay.
func TestRelayPointed(t *testing.T) {
	sandbox(t, "1.0.92")
	a := Agent{ConfigDir: t.TempDir()}
	if pointed, known := a.RelayPointed("127.0.0.1:43180"); pointed || !known {
		t.Errorf("before setup: pointed %v, known %v", pointed, known)
	}
	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
		t.Fatal(err)
	}
	if pointed, _ := a.RelayPointed("127.0.0.1:43180"); pointed {
		t.Error("pointed without terma's hooks file")
	}
	if detail, _, problem := a.RelayProblem("127.0.0.1:43180"); !problem || !strings.Contains(detail, "hooks") {
		t.Errorf("problem %q, %v", detail, problem)
	}
	installHooks(t)
	if pointed, known := a.RelayPointed("127.0.0.1:43180"); !pointed || !known {
		t.Errorf("after setup: pointed %v, known %v", pointed, known)
	}
	if _, _, problem := a.RelayProblem("127.0.0.1:43180"); problem {
		t.Error("a problem after setup")
	}
	if pointed, _ := a.RelayPointed("127.0.0.1:9999"); pointed {
		t.Error("pointed at another relay")
	}

	t.Setenv(sandboxEnv, "workspace")
	if pointed, _ := a.RelayPointed("127.0.0.1:43180"); !pointed {
		t.Error("a sandbox made the export read as not pointed")
	}
	if detail, _, problem := a.RelayProblem("127.0.0.1:43180"); !problem || !strings.Contains(detail, "workspace") {
		t.Errorf("sandbox warning %q, %v", detail, problem)
	}
	t.Setenv(sandboxEnv, "off")
	if _, _, problem := a.RelayProblem("127.0.0.1:43180"); problem {
		t.Error("the sandbox switched off still warns")
	}

	t.Setenv(sandboxEnv, "")
	path, _ := a.Harness().ConfigPath()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString("\n[sandbox]\nprofile = \"strict\"\n")
	_ = f.Close()
	if detail, _, problem := a.RelayProblem("127.0.0.1:43180"); !problem || !strings.Contains(detail, "[sandbox]") {
		t.Errorf("config sandbox warning %q, %v", detail, problem)
	}

	t.Setenv(envLogsExporter, "none")
	if pointed, _ := a.RelayPointed("127.0.0.1:43180"); pointed {
		t.Error("pointed with the log export switched off in the shell")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The hook processes grok starts carry GROK_HOOK_EVENT; the commands its tools run do not.
func TestStartedHook(t *testing.T) {
	t.Parallel()
	env := map[string]string{"GROK_SESSION_ID": "s"}
	if (Agent{}).StartedHook(func(k string) string { return env[k] }) {
		t.Error("a tool's command read as a hook grok started")
	}
	env[hookEventEnv] = "stop"
	if !(Agent{}).StartedHook(func(k string) string { return env[k] }) {
		t.Error("a hook grok started was not marked")
	}
}

func TestParseVersion(t *testing.T) {
	t.Parallel()
	for v, old := range map[string]bool{"1.0.92": false, "1.0.91": true, "1.1.0": false, "0.9.99": true, "2.0.0-beta": false, "": false, "unknown": false} {
		if _, got := tooOldVersion(v); got != old {
			t.Errorf("%q: too old %v, want %v", v, got, old)
		}
	}
}
