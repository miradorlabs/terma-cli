package omp

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

const relayToken = "relay-token-0123456789abcdef"

// ompIn sandboxes omp's agent directory and terma's own, and returns the agent and the .env.
func ompIn(t *testing.T) (Agent, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(envAgentDir, filepath.Join(dir, "agent"))
	for _, k := range []string{harness.EnvOTLPEndpoint, harness.EnvOTLPProtocol, ompCaptureContentEnv,
		signalKey(harness.SignalTraces, "ENDPOINT"), signalKey(harness.SignalLogs, "ENDPOINT"), signalKey(harness.SignalMetrics, "ENDPOINT")} {
		t.Setenv(k, "")
	}
	return Agent{ConfigDir: filepath.Join(dir, "terma")}, filepath.Join(dir, "agent", ".env")
}

func relayConfig() agents.RelayConfig {
	return agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: relayToken, HookCommand: []string{"/usr/local/bin/terma", "hook"}}
}

func write(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The developer's .env keeps every line terma does not manage; terma's keys win over theirs,
// once each; a per-signal endpoint, header or switch already there is pointed at the relay
// too; the file is private; and a teardown puts back exactly what was there.
func TestRelayEnvSpliceAndTeardown(t *testing.T) {
	a, env := ompIn(t)
	const before = `# my omp settings
ANTHROPIC_BASE_URL=https://proxy.example
export OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example:4318 # company collector
OTEL_EXPORTER_OTLP_TRACES_ENDPOINT="https://traces.example/v1/traces"
OTEL_EXPORTER_OTLP_TRACES_HEADERS=x-team=blue
OTEL_METRICS_EXPORTER=none
NOTE="two
lines"
OTEL_EXPORTER_OTLP_ENDPOINT=https://later.example
`
	write(t, env, before, 0o644)

	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
		t.Fatal(err)
	}
	after := read(t, env)
	for _, keep := range []string{"# my omp settings", "ANTHROPIC_BASE_URL=https://proxy.example", "NOTE=\"two\nlines\""} {
		if !strings.Contains(after, keep) {
			t.Errorf("the developer's line %q is gone:\n%s", keep, after)
		}
	}
	f, err := loadEnvFile(env)
	if err != nil {
		t.Fatal(err)
	}
	values := f.values()
	for key, want := range map[string]string{
		harness.EnvOTLPEndpoint:                      "http://127.0.0.1:43180",
		harness.EnvOTLPProtocol:                      harness.ProtocolHTTPProtobuf,
		harness.EnvOTLPHeaders:                       "authorization=Bearer%20" + relayToken,
		ompCaptureContentEnv:                         "full",
		signalKey(harness.SignalTraces, "ENDPOINT"):  "http://127.0.0.1:43180/v1/traces",
		signalKey(harness.SignalLogs, "ENDPOINT"):    "http://127.0.0.1:43180/v1/logs",
		signalKey(harness.SignalMetrics, "ENDPOINT"): "http://127.0.0.1:43180/v1/metrics",
		signalKey(harness.SignalTraces, "HEADERS"):   "authorization=Bearer%20" + relayToken,
		signalExporterKey(harness.SignalMetrics):     "otlp",
		"NOTE":                                       "two\nlines",
		"ANTHROPIC_BASE_URL":                         "https://proxy.example",
	} {
		if values[key] != want {
			t.Errorf("%s = %q, want %q", key, values[key], want)
		}
	}
	if n := strings.Count(after, harness.EnvOTLPEndpoint+"="); n != 1 {
		t.Errorf("the common endpoint is in the file %d times, want once:\n%s", n, after)
	}
	if !strings.HasPrefix(after, "# my omp settings\nANTHROPIC_BASE_URL=https://proxy.example\nOTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:43180\n") {
		t.Errorf("terma's endpoint did not take the developer's line's place:\n%s", after)
	}
	if info, _ := os.Stat(env); info.Mode().Perm() != 0o600 {
		t.Errorf(".env is %v: it holds the relay's token", info.Mode().Perm())
	}
	if pointed, known := a.RelayPointed("127.0.0.1:43180"); !known || !pointed {
		t.Error("the .env it just wrote does not read as pointed at the relay")
	}

	if _, err := a.UnconfigureRelay(context.Background(), relayToken); err != nil {
		t.Fatal(err)
	}
	restored, err := loadEnvFile(env)
	if err != nil {
		t.Fatal(err)
	}
	got := restored.values()
	for key, want := range map[string]string{
		harness.EnvOTLPEndpoint:                     "https://later.example",
		signalKey(harness.SignalTraces, "ENDPOINT"): "https://traces.example/v1/traces",
		signalKey(harness.SignalTraces, "HEADERS"):  "x-team=blue",
		signalExporterKey(harness.SignalMetrics):    "none",
		"NOTE":                                      "two\nlines",
	} {
		if got[key] != want {
			t.Errorf("after teardown %s = %q, want %q", key, got[key], want)
		}
	}
	for _, gone := range []string{harness.EnvOTLPProtocol, harness.EnvOTLPHeaders, ompCaptureContentEnv, signalKey(harness.SignalLogs, "ENDPOINT")} {
		if _, ok := got[gone]; ok {
			t.Errorf("teardown left %s, which the developer never set", gone)
		}
	}
	if strings.Contains(read(t, env), relayToken) {
		t.Error("teardown left the relay's token in the .env")
	}
	if j, err := harness.LoadJournal(a.ConfigDir, name, env); err != nil || j != nil {
		t.Errorf("the journal outlived the teardown: %+v, %v", j, err)
	}
}

// A .env setup created goes again at teardown, and a reconnect keeps the developer's
// original values to restore, not terma's own.
func TestRelayEnvReconnectAndRemoval(t *testing.T) {
	a, env := ompIn(t)
	write(t, env, "OTEL_EXPORTER_OTLP_PROTOCOL=grpc\n", 0o600)
	for range 2 {
		if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.UnconfigureRelay(context.Background(), relayToken); err != nil {
		t.Fatal(err)
	}
	if got := read(t, env); got != "OTEL_EXPORTER_OTLP_PROTOCOL=grpc\n" {
		t.Fatalf("after two setups and a teardown the .env is:\n%s", got)
	}

	if err := os.Remove(env); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UnconfigureRelay(context.Background(), relayToken); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(env); !os.IsNotExist(err) {
		t.Fatalf("the .env setup created outlived the teardown: %v", err)
	}
}

// A key the developer changed after setup is theirs: teardown leaves it, says so, and
// doctor no longer reads omp as sending to the relay.
func TestRelayEnvEditedAfterSetup(t *testing.T) {
	a, env := ompIn(t)
	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(read(t, env), "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=http://127.0.0.1:43180/v1/logs", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=https://mine.example/v1/logs", 1)
	write(t, env, text, 0o600)
	if pointed, _ := a.RelayPointed("127.0.0.1:43180"); pointed {
		t.Error("a per-signal endpoint elsewhere still reads as pointed at the relay")
	}
	res, err := a.UnconfigureRelay(context.Background(), relayToken)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") }) {
		t.Errorf("teardown did not name the key it left: %v", res.Notes)
	}
	if got := read(t, env); got != "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=https://mine.example/v1/logs\n" {
		t.Errorf("teardown left:\n%s", got)
	}
}

// omp's agent directory is PI_CODING_AGENT_DIR, else ~/$PI_CONFIG_DIR/agent, else ~/.omp/agent.
func TestAgentDirFollowsOmp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, tc := range []struct{ agentDir, configDir, want string }{
		{"", "", filepath.Join(home, ".omp", "agent", ".env")},
		{"", ".omp-work", filepath.Join(home, ".omp-work", "agent", ".env")},
		{filepath.Join(home, "custom"), ".omp-work", filepath.Join(home, "custom", ".env")},
	} {
		t.Setenv(envAgentDir, tc.agentDir)
		t.Setenv(envConfigDir, tc.configDir)
		if got, err := (exporter{}).ConfigPath(); err != nil || got != tc.want {
			t.Errorf("PI_CODING_AGENT_DIR=%q PI_CONFIG_DIR=%q: %s, %v; want %s", tc.agentDir, tc.configDir, got, err, tc.want)
		}
	}
}

// Setup replaces the span-exporting extension and the exporter hook an earlier terma wrote,
// leaves a developer's own file of either name, and writes an extension that holds no
// token: it only hands sessions and edits to terma's hooks.
func TestConfigureRelayReplacesLegacyFiles(t *testing.T) {
	a, env := ompIn(t)
	dir := filepath.Dir(env)
	legacy := filepath.Join(dir, "extensions", "terma-relay.ts")
	hook := filepath.Join(dir, "hooks", "pre", "terma.ts")
	write(t, legacy, "// Terma extension for Pi (@earendil-works/pi-coding-agent) and omp\nconst CONFIG = {}\n", 0o600)
	write(t, hook, "// my own pre hook\n", 0o644)

	res, err := a.ConfigureRelay(context.Background(), relayConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("the legacy extension is still there: %v", err)
	}
	if read(t, hook) != "// my own pre hook\n" {
		t.Error("setup removed a hook terma did not write")
	}
	ext := filepath.Join(dir, "extensions", "terma-omp.ts")
	body := read(t, ext)
	for _, want := range []string{`"agent":"omp"`, `"lifecycle":true`, `"endpoint":""`, `"hookCommand":["/usr/local/bin/terma","hook"]`} {
		if !strings.Contains(body, want) {
			t.Errorf("extension lacks %s", want)
		}
	}
	if strings.Contains(body, relayToken) {
		t.Error("the extension holds the relay's token; only the .env should")
	}
	for _, want := range []string{env, ext, legacy} {
		if !slices.Contains(res.Paths, want) {
			t.Errorf("result paths %v lack %s", res.Paths, want)
		}
	}

	if _, err := a.UnconfigureRelay(context.Background(), relayToken); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ext); !os.IsNotExist(err) {
		t.Errorf("teardown left the extension: %v", err)
	}
}

// A shell export outranks the .env: an endpoint elsewhere is a credential conflict, the
// relay's own is none.
func TestShellExportsConflict(t *testing.T) {
	_, _ = ompIn(t)
	e := harness.Exporter{Endpoint: "http://127.0.0.1:43180"}
	t.Setenv(signalKey(harness.SignalTraces, "ENDPOINT"), "http://127.0.0.1:43180/v1/traces")
	if c := ompConflicts(e); len(c) != 0 {
		t.Fatalf("the relay's own endpoint is a conflict: %+v", c)
	}
	t.Setenv(harness.EnvOTLPEndpoint, "https://collector.example")
	t.Setenv(ompCaptureContentEnv, "summary")
	c := ompConflicts(e)
	if len(c) != 2 || c[0].Key != harness.EnvOTLPEndpoint || !c[0].Credential || c[1].Key != ompCaptureContentEnv || !c[1].Advisory {
		t.Fatalf("conflicts = %+v", c)
	}
}
