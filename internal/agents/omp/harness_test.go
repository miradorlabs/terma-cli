package omp

import (
	"cmp"
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

const relayToken = "relay-token-0123456789abcdef"

// ompIn sandboxes omp's agent directory and terma's own, with an OMP 19.1.0 on PATH, and
// returns the agent and the .env.
func ompIn(t *testing.T) (Agent, string) {
	t.Helper()
	ompOnPath(t, "19.1.0")
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
		harness.EnvOTLPEndpoint:                      "http://127.0.0.1:43180/" + relayToken,
		harness.EnvOTLPProtocol:                      harness.ProtocolHTTPProtobuf,
		signalKey(harness.SignalTraces, "ENDPOINT"):  "http://127.0.0.1:43180/" + relayToken + "/v1/traces",
		signalKey(harness.SignalLogs, "ENDPOINT"):    "http://127.0.0.1:43180/" + relayToken + "/v1/logs",
		signalKey(harness.SignalMetrics, "ENDPOINT"): "http://127.0.0.1:43180/" + relayToken + "/v1/metrics",
		signalKey(harness.SignalTraces, "HEADERS"):   "x-team=blue",
		signalExporterKey(harness.SignalMetrics):     "otlp",
		"NOTE":                                       "two\nlines",
		"ANTHROPIC_BASE_URL":                         "https://proxy.example",
	} {
		if values[key] != want {
			t.Errorf("%s = %q, want %q", key, values[key], want)
		}
	}
	// Content capture in this file would reach an endpoint that outranks it: the extension asks
	// for it in the running OMP instead.
	if _, ok := values[ompCaptureContentEnv]; ok {
		t.Errorf("setup wrote %s into the .env", ompCaptureContentEnv)
	}
	// A repository's .env that sets a signal's endpoint outranks this file, and omp would
	// send it this file's headers: the token rides only in terma's own endpoints' path.
	if strings.Contains(strings.ToLower(after), "authorization") || strings.Count(after, relayToken) != 4 {
		t.Errorf("the token is somewhere other than the four endpoints:\n%s", after)
	}
	if n := strings.Count(after, harness.EnvOTLPEndpoint+"="); n != 1 {
		t.Errorf("the common endpoint is in the file %d times, want once:\n%s", n, after)
	}
	if !strings.HasPrefix(after, "# my omp settings\nANTHROPIC_BASE_URL=https://proxy.example\nOTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:43180/"+relayToken+"\n") {
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

// A .env that was there before setup is there after teardown, as it was: an empty or
// blank one is kept, not removed, and its mode comes back from the 0600 setup wrote it at,
// after two setups too.
func TestRelayEnvTeardownRestoresTheFile(t *testing.T) {
	for name, before := range map[string]string{"empty": "", "blank": "\n  \n", "the developer's": "# mine\nANTHROPIC_BASE_URL=https://proxy.example\n"} {
		t.Run(name, func(t *testing.T) {
			a, env := ompIn(t)
			write(t, env, before, 0o644)
			for range 2 {
				if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
					t.Fatal(err)
				}
			}
			if info, _ := os.Stat(env); info.Mode().Perm() != 0o600 {
				t.Errorf("after setup .env is %v: it holds the relay's token", info.Mode().Perm())
			}
			if _, err := a.UnconfigureRelay(context.Background(), relayToken); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(env)
			if err != nil {
				t.Fatalf("teardown removed the developer's .env: %v", err)
			}
			if info.Mode().Perm() != 0o644 {
				t.Errorf("after teardown .env is %v, want 0644 as it was", info.Mode().Perm())
			}
			if got := read(t, env); got != before {
				t.Errorf("after teardown .env is %q, want %q", got, before)
			}
		})
	}
}

// A key the developer changed after setup is theirs: teardown leaves it, says so, and
// doctor no longer reads omp as sending to the relay.
func TestRelayEnvEditedAfterSetup(t *testing.T) {
	a, env := ompIn(t)
	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(read(t, env), "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=http://127.0.0.1:43180/"+relayToken+"/v1/logs", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=https://mine.example/v1/logs", 1)
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

// omp switches a signal off whose exporter list names no otlp, as it does one naming none:
// such a signal is not read as exporting, setup turns it on, and teardown puts the list back.
func TestRelayEnvExporterListWithoutOTLP(t *testing.T) {
	a, env := ompIn(t)
	write(t, env, "OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:43180/"+relayToken+"\nOTEL_TRACES_EXPORTER=console\n", 0o600)
	st, err := a.Harness().Status()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(st.Signals, harness.SignalTraces) {
		t.Errorf("traces read as exported though their exporter is console: %v", st.Signals)
	}
	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, env), "OTEL_TRACES_EXPORTER=otlp\n") {
		t.Errorf("setup left traces off:\n%s", read(t, env))
	}
	if _, err := a.UnconfigureRelay(context.Background(), relayToken); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, env), "OTEL_TRACES_EXPORTER=console\n") {
		t.Errorf("teardown did not restore the list:\n%s", read(t, env))
	}
}

// OMP fills a key its agent .env leaves unset from the dotenv files it reads after it, where a
// switch can turn a signal or the whole SDK off: setup sets every such key in the agent .env
// whether or not it was there, and teardown takes away the ones it added.
func TestRelayEnvShadowsSwitchesItDidNotFind(t *testing.T) {
	a, env := ompIn(t)
	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
		t.Fatal(err)
	}
	f, err := loadEnvFile(env)
	if err != nil {
		t.Fatal(err)
	}
	values := f.values()
	want := map[string]string{envSDKDisabled: "false"}
	for _, s := range harness.AllSignals {
		want[signalExporterKey(s)] = exporterOTLP
		want[signalKey(s, "PROTOCOL")] = harness.ProtocolHTTPProtobuf
	}
	for key, v := range want {
		if values[key] != v {
			t.Errorf("%s = %q, want %q", key, values[key], v)
		}
	}
	if _, err := a.UnconfigureRelay(context.Background(), relayToken); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(env); err == nil && strings.Contains(string(got), "OTEL_") {
		t.Errorf("teardown left:\n%s", got)
	}
}

// An exporter list that already names otlp exports to the relay as it is: setup keeps it.
func TestRelayEnvKeepsAnExporterListNamingOTLP(t *testing.T) {
	a, env := ompIn(t)
	write(t, env, "OTEL_TRACES_EXPORTER=console,otlp\n", 0o600)
	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
		t.Fatal(err)
	}
	if got := read(t, env); !strings.Contains(got, "OTEL_TRACES_EXPORTER=console,otlp\n") {
		t.Errorf("setup replaced a list that names otlp:\n%s", got)
	}
	if pointed, _ := a.RelayPointed("127.0.0.1:43180"); !pointed {
		t.Error("the .env does not read as pointed at the relay")
	}
}

// An earlier setup sent the token in an Authorization header and wrote content capture; the
// next one takes each back to what the developer had, and teardown leaves that.
func TestRelayEnvRetiresTheTokenHeader(t *testing.T) {
	a, env := ompIn(t)
	write(t, env, "OTEL_EXPORTER_OTLP_TRACES_HEADERS=x-team=blue\n", 0o600)
	auth := "authorization=Bearer%20" + relayToken
	headers := []string{harness.EnvOTLPHeaders, signalKey(harness.SignalTraces, "HEADERS")}
	blue := "x-team=blue"
	earlier := harness.NewJournal(name, env, map[string]string{headers[1]: blue}, map[string]string{headers[0]: auth, headers[1]: auth, ompCaptureContentEnv: "full"}, nil, nil, nil)
	if err := earlier.Save(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	write(t, env, headers[1]+"="+auth+"\n"+headers[0]+"="+auth+"\n"+ompCaptureContentEnv+"=full\n", 0o600)

	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
		t.Fatal(err)
	}
	after := read(t, env)
	if strings.Contains(strings.ToLower(after), "authorization") || strings.Contains(after, ompCaptureContentEnv) || !strings.Contains(after, headers[1]+"="+blue+"\n") {
		t.Fatalf("setup kept a token header or content capture, or lost the developer's header:\n%s", after)
	}
	if pointed, _ := a.RelayPointed("127.0.0.1:43180"); !pointed {
		t.Error("the .env does not read as pointed at the relay")
	}
	if _, err := a.UnconfigureRelay(context.Background(), relayToken); err != nil {
		t.Fatal(err)
	}
	if got := read(t, env); got != headers[1]+"="+blue+"\n" {
		t.Errorf("teardown left:\n%s", got)
	}
}

// Traces alone name OMP's session, so a .env that switches them off after setup is not
// pointed at the relay, though logs and metrics still export there.
func TestRelayEnvTracesOffIsNotPointed(t *testing.T) {
	for _, line := range []string{"OTEL_TRACES_EXPORTER=none", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=grpc"} {
		a, env := ompIn(t)
		if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
			t.Fatal(err)
		}
		write(t, env, read(t, env)+line+"\n", 0o600)
		if pointed, _ := a.RelayPointed("127.0.0.1:43180"); pointed {
			t.Errorf("with %s the .env still reads as pointed at the relay", line)
		}
	}
}

// A reconnect whose .env write fails leaves the earlier journal as it was, header records
// included, so teardown can still take back the token header the .env still holds.
func TestRelayEnvFailedReconnectKeepsTheJournal(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	a, env := ompIn(t)
	auth := "authorization=Bearer%20" + relayToken
	earlier := harness.NewJournal(name, env, map[string]string{}, map[string]string{harness.EnvOTLPHeaders: auth}, nil, nil, nil)
	if err := earlier.Save(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	write(t, env, harness.EnvOTLPHeaders+"="+auth+"\n", 0o600)
	if err := os.Chmod(filepath.Dir(env), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(env), 0o700) })

	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err == nil {
		t.Fatal("setup wrote into a read-only directory")
	}
	j, err := harness.LoadJournal(a.ConfigDir, name, env)
	if err != nil || j == nil {
		t.Fatalf("journal = %v, %v", j, err)
	}
	if j.Installed[harness.EnvOTLPHeaders] != auth {
		t.Errorf("the restored journal lost the header it installed: %v", j.Installed)
	}
}

// A .env that links to a file kept elsewhere is written through, and teardown leaves the
// link resolving to an emptied file, which a later setup can write again.
func TestRelayEnvSymlinkedTeardown(t *testing.T) {
	a, env := ompIn(t)
	target := filepath.Join(t.TempDir(), "dotfiles", "omp.env")
	write(t, target, "", 0o600)
	if err := os.MkdirAll(filepath.Dir(env), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, env); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(read(t, target), relayToken) {
			t.Fatal("setup did not write through the link")
		}
		if _, err := a.UnconfigureRelay(context.Background(), relayToken); err != nil {
			t.Fatal(err)
		}
		if got := read(t, target); got != "" {
			t.Fatalf("after teardown the linked file holds:\n%s", got)
		}
	}
	if info, err := os.Lstat(env); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link is gone: %v, %v", info, err)
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
	for _, want := range []string{`"hookCommand":["/usr/local/bin/terma","hook"]`} {
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

// A setup that cannot write the extension leaves omp's exporter where it was: pointed at
// the relay with nothing to claim its sessions, its spans would be held and dropped.
func TestConfigureRelayWithoutTheExtensionLeavesTheEnv(t *testing.T) {
	a, env := ompIn(t)
	write(t, env, "OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example\n", 0o600)
	write(t, filepath.Join(filepath.Dir(env), "extensions"), "not a directory\n", 0o600)
	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err == nil {
		t.Fatal("setup succeeded without its extension")
	}
	if got := read(t, env); got != "OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example\n" {
		t.Fatalf(".env = %q", got)
	}
}

// A shell export outranks the .env key by key: a signal's endpoint elsewhere is a credential
// conflict, the relay's own is none, and the common endpoint or protocol is none, since omp
// reads each signal's own first and the .env sets every one.
func TestShellExportsConflict(t *testing.T) {
	_, _ = ompIn(t)
	e := harness.Exporter{Endpoint: "http://127.0.0.1:43180"}
	t.Setenv(signalKey(harness.SignalLogs, "ENDPOINT"), "http://127.0.0.1:43180/v1/logs")
	t.Setenv(harness.EnvOTLPEndpoint, "https://collector.example")
	t.Setenv(harness.EnvOTLPProtocol, "grpc")
	if c := ompConflicts(e); len(c) != 0 {
		t.Fatalf("the relay's own or the common endpoint or protocol is a conflict: %+v", c)
	}
	t.Setenv(signalKey(harness.SignalTraces, "ENDPOINT"), "https://collector.example/v1/traces")
	t.Setenv(ompCaptureContentEnv, "summary")
	c := ompConflicts(e)
	if len(c) != 2 || c[0].Key != signalKey(harness.SignalTraces, "ENDPOINT") || !c[0].Credential || c[1].Key != ompCaptureContentEnv || !c[1].Advisory {
		t.Fatalf("conflicts = %+v", c)
	}
}

// A shell export that switches omp's export off outranks the .env too: traces off is a
// conflict, since only they name the session; logs or metrics off is advisory.
func TestShellExportsSwitchingSignalsOff(t *testing.T) {
	_, _ = ompIn(t)
	e := harness.Exporter{Endpoint: "http://127.0.0.1:43180"}
	t.Setenv(signalExporterKey(harness.SignalTraces), "otlp,console")
	if c := ompConflicts(e); len(c) != 0 {
		t.Fatalf("an exporter list naming otlp is a conflict: %+v", c)
	}
	for _, c := range []struct {
		key, value string
		advisory   bool
	}{
		{envSDKDisabled, "true", false},
		{signalExporterKey(harness.SignalTraces), "none", false},
		{signalKey(harness.SignalTraces, "PROTOCOL"), "grpc", false},
		{signalExporterKey(harness.SignalMetrics), "console", true},
		{signalKey(harness.SignalLogs, "PROTOCOL"), "grpc", true},
	} {
		t.Run(c.key, func(t *testing.T) {
			t.Setenv(c.key, c.value)
			got := ompConflicts(e)
			if len(got) != 1 || got[0].Key != c.key || got[0].Advisory != c.advisory || got[0].Scope != harness.ScopeEnvironment {
				t.Fatalf("conflicts = %+v", got)
			}
		})
	}
}

// OMP's spans are attributed only by the claims terma's extension makes, so an .env pointed
// at the relay is not enough: the extension must be in place and be this terma's hooks-only
// file for this relay, whatever hook command it names. Doctor says which is wrong.
func TestRelayPointedNeedsTheCurrentExtension(t *testing.T) {
	a, _ := ompIn(t)
	if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
		t.Fatal(err)
	}
	dir, err := ompAgentDir()
	if err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(dir, extensionPath)
	if pointed, _ := a.RelayPointed("127.0.0.1:43180"); !pointed {
		t.Fatal("a fresh setup does not read as pointed")
	}
	if _, _, problem := a.RelayProblem("127.0.0.1:43180"); problem {
		t.Fatal("a fresh setup reports a problem")
	}
	other, err := renderExtension([]string{"/opt/terma/bin/terma", "hook"}, "http://127.0.0.1:43180")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, text string
		pointed    bool
	}{
		{"another hook command", other, true},
		{"an older extension", strings.Replace(other, "omp-session-start", "omp-session-begin", 1), false},
		{"the bare template", extensionTemplate, false},
		{"an empty hook command", strings.Replace(other, `"hookCommand":["/opt/terma/bin/terma","hook"]`, `"hookCommand":[]`, 1), false},
		{"an empty hook executable", strings.Replace(other, `"hookCommand":["/opt/terma/bin/terma","hook"]`, `"hookCommand":[""]`, 1), false},
		{"another relay's address", strings.Replace(other, `"relay":"http://127.0.0.1:43180"`, `"relay":"http://127.0.0.1:43181"`, 1), false},
		{"the developer's own file", "export default function () {}\n", false},
		{"missing", "", false},
	} {
		if c.text == "" {
			if err := os.Remove(ext); err != nil {
				t.Fatal(err)
			}
		} else {
			write(t, ext, c.text, 0o600)
		}
		if pointed, known := a.RelayPointed("127.0.0.1:43180"); !known || pointed != c.pointed {
			t.Errorf("%s: pointed %v, want %v", c.name, pointed, c.pointed)
		}
		detail, fix, problem := a.RelayProblem("127.0.0.1:43180")
		if problem == c.pointed || !c.pointed && (!strings.Contains(detail, extensionPath) || fix != "terma setup") {
			t.Errorf("%s: problem %v %q %q", c.name, problem, detail, fix)
		}
	}
}

// Teardown puts back the developer's own entries as they wrote them, whatever quoting, prefix,
// comment or line ending they had: an unquoted value holding every quote character reads back
// whole, which no re-quoting of it would. A key written twice keeps both entries, the later
// one moved up to the first, where the file holds the key while terma's.
func TestRelayEnvTeardownRestoresTheEntryVerbatim(t *testing.T) {
	cases := map[string]struct{ before, after string }{
		"every quote, unquoted": {"OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example/a'b\"c`d\n", ""},
		"export and comment":    {"export OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example:4318   # company\n", ""},
		"crlf":                  {"OTEL_EXPORTER_OTLP_ENDPOINT='https://collector.example'\r\nA=1\r\n", ""},
		"multiline":             {"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=\"https://collector.example\nv1\"\nA=1\n", ""},
		"written twice": {
			"A=1\nOTEL_EXPORTER_OTLP_ENDPOINT=https://first.example # first\nB=2\nOTEL_EXPORTER_OTLP_ENDPOINT=https://second.example\n",
			"A=1\nOTEL_EXPORTER_OTLP_ENDPOINT=https://first.example # first\nOTEL_EXPORTER_OTLP_ENDPOINT=https://second.example\nB=2\n",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			before, after := c.before, cmp.Or(c.after, c.before)
			a, env := ompIn(t)
			write(t, env, before, 0o600)
			want, err := loadEnvFile(env)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
				t.Fatal(err)
			}
			if _, err := a.UnconfigureRelay(context.Background(), relayToken); err != nil {
				t.Fatal(err)
			}
			if got := read(t, env); got != after {
				t.Errorf("teardown left:\n%q\nwant:\n%q", got, after)
			}
			got, err := loadEnvFile(env)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(got.values(), want.values()) {
				t.Errorf("teardown reads as %q, want %q", got.values(), want.values())
			}
		})
	}
}

// A line inside another entry's quoted value is part of that value as OMP reads it, an escaped
// quote not closing it, so setup leaves it alone: rewritten, it would put the relay's token in
// an ordinary variable that OMP hands to the tools it runs.
func TestRelayEnvLeavesALineInsideAQuotedValue(t *testing.T) {
	for _, q := range []string{`"`, `'`, "`"} {
		a, env := ompIn(t)
		note := "NOTE=" + q + `one\` + q + "\nOTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example\ntwo" + q + "\n"
		write(t, env, note, 0o600)
		if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
			t.Fatal(err)
		}
		after := read(t, env)
		if !strings.HasPrefix(after, note) {
			t.Errorf("%s: setup rewrote the inside of NOTE's value:\n%s", q, after)
		}
		f, err := loadEnvFile(env)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.values()["NOTE"]; strings.Contains(got, relayToken) {
			t.Errorf("%s: NOTE holds the relay's token: %q", q, got)
		}
		if got := f.values()[harness.EnvOTLPEndpoint]; got != "http://127.0.0.1:43180/"+relayToken {
			t.Errorf("%s: the common endpoint is %q", q, got)
		}
	}
}

// A file whose lines terma cannot map to OMP's entries, here a bare carriage return that OMP
// reads as a line break, is refused rather than edited.
func TestRelayEnvRefusesAFileItCannotReadWithCertainty(t *testing.T) {
	a, env := ompIn(t)
	const before = "A=1\rOTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example\n"
	write(t, env, before, 0o600)
	_, err := a.ConfigureRelay(context.Background(), relayConfig())
	if err == nil || !strings.Contains(err.Error(), env) {
		t.Fatalf("setup edited a .env it cannot read with certainty: %v", err)
	}
	if got := read(t, env); got != before {
		t.Errorf("the refused .env changed:\n%q", got)
	}
}

// Every signal must reach the relay as OMP resolves it, its own endpoint ahead of the common
// one, as the extension requires before it asks OMP for content: one sent elsewhere is not
// pointed, whichever it is.
func TestRelayEnvASignalSentElsewhereIsNotPointed(t *testing.T) {
	const relay = "http://127.0.0.1:43180/" + relayToken
	for name, extra := range map[string]string{
		"logs elsewhere":    "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=https://collector.example/ingest/v1/logs\n",
		"metrics elsewhere": "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=https://collector.example/ingest/v1/metrics\n",
		"only traces on the relay": "OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example/ingest\n" +
			"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=" + relay + "/v1/traces\n" +
			"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=\nOTEL_EXPORTER_OTLP_METRICS_ENDPOINT=\n",
	} {
		a, env := ompIn(t)
		if _, err := a.ConfigureRelay(context.Background(), relayConfig()); err != nil {
			t.Fatal(err)
		}
		write(t, env, read(t, env)+extra, 0o600)
		if pointed, _ := a.RelayPointed("127.0.0.1:43180"); pointed {
			t.Errorf("%s: the .env still reads as pointed at the relay", name)
		}
	}
}
