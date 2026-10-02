package codex

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// codexIn points Codex's config at a temp dir, never the developer's real one.
func codexIn(t *testing.T, contents string) (exporter, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(dir, "terma"))

	path := filepath.Join(dir, "config.toml")
	if contents != "" {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("seed config: %v", err)
		}
	}
	return exporter{}, path
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func otelOf(t *testing.T, path string) map[string]any {
	t.Helper()
	doc := mustParse(t, readText(t, path))
	otel, _ := doc["otel"].(map[string]any)
	return otel
}

func codexExporter() harness.Exporter {
	e := fullExporter()
	e.ResourceAttributes[harness.AttrServiceName] = codexServiceName
	return e
}

// A realistic config.toml, none of which a connect may change.
const codexSeed = `# Codex configuration
model = "gpt-5"          # my model
model_reasoning_effort = "high"

[mcp_servers.storybloq]
command = "storybloq"
args = ["serve", "--stdio"]

[mcp_servers.storybloq.env]
STORYBLOQ_CLIENT = "cli"

[profiles.fast]
model = "gpt-5-mini"
`

func TestCodexRenderWritesOneExporterPerSignal(t *testing.T) {
	env := exporter{}.render(codexExporter())

	want := map[string]string{
		"trace_exporter":   `{ otlp-http = { endpoint = "https://otel.terma.ai/v1/traces", headers = { Authorization = "Bearer ter_srv_0123456789abcdef" }, protocol = "binary" } }`,
		"exporter":         `{ otlp-http = { endpoint = "https://otel.terma.ai/v1/logs", headers = { Authorization = "Bearer ter_srv_0123456789abcdef" }, protocol = "binary" } }`,
		"metrics_exporter": `{ otlp-http = { endpoint = "https://otel.terma.ai/v1/metrics", headers = { Authorization = "Bearer ter_srv_0123456789abcdef" }, protocol = "binary" } }`,
		"log_user_prompt":  "true",
		// Each attribution key is its own span_attributes entry; service.name is Codex's own.
		"span_attributes/enduser.id":         `"dev@example.com"`,
		"span_attributes/mirador.project.id": `"proj_123"`,
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("Render() =\n%#v\nwant\n%#v", env, want)
	}
}

// An unselected signal is left alone, not written as "none": for metrics that would
// switch off OpenAI's own route.
func TestCodexRenderLeavesUnselectedSignalsAlone(t *testing.T) {
	env := exporter{}.render(harness.Exporter{Endpoint: termaEndpoint, Signals: []harness.Signal{harness.SignalLogs}})
	if _, ok := env["exporter"]; !ok {
		t.Error("the log exporter was not written")
	}
	for _, key := range []string{"trace_exporter", "metrics_exporter"} {
		if v, ok := env[key]; ok {
			t.Errorf("%s = %q, want it left unwritten", key, v)
		}
	}
}

// Content always goes to the relay, which withholds it per the team's policy: prompts are
// logged and Codex's or the user's own tool-output cap stays in force.
func TestCodexRenderAlwaysCapturesContent(t *testing.T) {
	on := exporter{}.render(harness.Exporter{Signals: harness.AllSignals})
	if on["log_user_prompt"] != "true" {
		t.Errorf("log_user_prompt = %q, want true", on["log_user_prompt"])
	}
	if v, ok := on["tool_result"]; ok {
		t.Errorf("tool_result = %q, want it unwritten", v)
	}
}

// A connect appends one table and changes no other byte of a hand-written file.
func TestCodexConnectPreservesFileByteForByte(t *testing.T) {
	c, path := codexIn(t, codexSeed)

	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	got := readText(t, path)
	if !strings.HasPrefix(got, codexSeed+"\n[otel]\n") {
		t.Fatalf("the original text was not preserved verbatim:\n%s", got)
	}
	otel := otelOf(t, path)
	for _, key := range []string{"exporter", "trace_exporter", "metrics_exporter", "log_user_prompt", "span_attributes"} {
		if _, ok := otel[key]; !ok {
			t.Errorf("%s missing from the written table", key)
		}
	}
	doc := mustParse(t, got)
	delete(doc, "otel")
	if !reflect.DeepEqual(doc, mustParse(t, codexSeed)) {
		t.Fatalf("settings outside otel changed:\n%v", doc)
	}
}

// An existing table is rewritten in place: the user's keys survive, and the next table's
// comment stays with the next table.
func TestCodexConnectRewritesExistingTableInPlace(t *testing.T) {
	const seed = `model = "gpt-5"

[otel]
environment = "staging"
exporter = "none"

# MCP servers
[mcp_servers.foo]
command = "foo"
`
	c, path := codexIn(t, seed)
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	got := readText(t, path)
	if strings.Count(got, "[otel]") != 1 {
		t.Fatalf("want exactly one [otel] table:\n%s", got)
	}
	if !strings.HasPrefix(got, "model = \"gpt-5\"\n\n[otel]\n") || !strings.HasSuffix(got, "\n# MCP servers\n[mcp_servers.foo]\ncommand = \"foo\"\n") {
		t.Fatalf("the text around the table changed:\n%s", got)
	}
	otel := otelOf(t, path)
	if otel["environment"] != "staging" {
		t.Errorf("environment = %v, want the user's value preserved", otel["environment"])
	}
	if shape := codexExporterOf(otel["exporter"]); shape.Kind != "otlp-http" {
		t.Errorf("exporter = %v, want it replaced", otel["exporter"])
	}
}

// A user attribute in span_attributes survives the connect, and disconnect gives back the
// original text.
func TestCodexConnectPreservesCustomSpanAttributes(t *testing.T) {
	const seed = "[otel]\nspan_attributes = { team = \"payments\" }\n"
	c, path := codexIn(t, seed)
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	attrs, _ := otelOf(t, path)["span_attributes"].(map[string]any)
	want := map[string]any{"team": "payments", "enduser.id": "dev@example.com", "mirador.project.id": "proj_123"}
	if !reflect.DeepEqual(attrs, want) {
		t.Fatalf("span_attributes = %v, want %v", attrs, want)
	}
	if _, err := c.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if got := readText(t, path); got != seed {
		t.Fatalf("after disconnect:\n%s\nwant:\n%s", got, seed)
	}
}

// An attribute added after the connect stays; Terma's own entries are still removed.
func TestCodexDisconnectRemovesOnlyItsOwnSpanAttributes(t *testing.T) {
	c, path := codexIn(t, "")
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	edited := strings.Replace(readText(t, path), "span_attributes = { ", "span_attributes = { team = \"payments\", ", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatalf("edit: %v", err)
	}

	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if len(result.Skipped) != 0 {
		t.Errorf("skipped = %v, want nothing — the user's addition is not an edit of Terma's entries", result.Skipped)
	}
	attrs, _ := otelOf(t, path)["span_attributes"].(map[string]any)
	if !reflect.DeepEqual(attrs, map[string]any{"team": "payments"}) {
		t.Fatalf("span_attributes = %v, want only the user's attribute left", attrs)
	}
	for _, key := range []string{"exporter", "log_user_prompt"} {
		if _, ok := otelOf(t, path)[key]; ok {
			t.Errorf("%s survived disconnect", key)
		}
	}
}

// Terma overwrites a same-named user attribute and puts the user's value back on disconnect.
func TestCodexDisconnectRestoresOverwrittenSpanAttribute(t *testing.T) {
	const seed = "[otel]\nspan_attributes = { \"enduser.id\" = \"someone@example.com\" }\n"
	c, path := codexIn(t, seed)
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := codexSpanAttribute(otelOf(t, path), harness.AttrEnduserID); got != "dev@example.com" {
		t.Fatalf("enduser.id = %q after connect", got)
	}
	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if result.Restored != 1 {
		t.Errorf("restored = %d, want the user's enduser.id put back", result.Restored)
	}
	if got := readText(t, path); got != seed {
		t.Fatalf("after disconnect:\n%s\nwant:\n%s", got, seed)
	}
}

func TestCodexConnectCreatesFileWhenAbsent(t *testing.T) {
	c, path := codexIn(t, "")
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if shape := codexExporterOf(otelOf(t, path)["exporter"]); shape.Endpoint != termaEndpoint+"/v1/logs" {
		t.Fatalf("exporter endpoint = %q", shape.Endpoint)
	}
}

// A connect that writes a live server key leaves config.toml owner-only.
func TestCodexConnectTightensFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	c, path := codexIn(t, codexSeed)
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("config mode = %#o, want no group/other access — the file holds a server key", mode)
	}
}

func TestCodexConnectRefusesMalformedConfig(t *testing.T) {
	for name, original := range map[string]string{
		"syntax":    "model = \"gpt-5\"\n[otel\n",
		"not table": "otel = \"none\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, path := codexIn(t, original)
			if err := c.Connect(codexExporter(), false); err == nil {
				t.Fatal("Connect succeeded against a config it cannot safely rewrite")
			}
			if readText(t, path) != original {
				t.Fatal("the file was modified; it must be left exactly as found")
			}
		})
	}
}

// Connect then disconnect leaves the file byte for byte as it was.
func TestCodexDisconnectRestoresOriginalTextExactly(t *testing.T) {
	c, path := codexIn(t, codexSeed)
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if result.Removed != len(exporter{}.render(codexExporter())) {
		t.Errorf("removed %d keys, want every key the connect wrote", result.Removed)
	}
	if got := readText(t, path); got != codexSeed {
		t.Fatalf("after disconnect:\n%s\nwant the original:\n%s", got, codexSeed)
	}
}

func TestCodexDisconnectKeepsTheUsersOtelKeys(t *testing.T) {
	const seed = "[otel]\nenvironment = \"staging\"\nmetrics_exporter = \"statsig\"\n"
	c, path := codexIn(t, seed)
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if result.Restored != 1 {
		t.Errorf("restored %d, want the explicit statsig metrics exporter put back", result.Restored)
	}
	if got := readText(t, path); got != seed {
		t.Fatalf("after disconnect:\n%s\nwant:\n%s", got, seed)
	}
}

func TestCodexDisconnectOnCleanFileIsANoop(t *testing.T) {
	c, path := codexIn(t, codexSeed)
	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if result.Removed != 0 || result.Restored != 0 || readText(t, path) != codexSeed {
		t.Errorf("disconnect changed a file that was never connected")
	}
}

// A key edited since the connect is the user's: disconnect names it rather than deleting it.
func TestCodexDisconnectLeavesEditedKeysAlone(t *testing.T) {
	c, path := codexIn(t, "")
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	edited := strings.Replace(readText(t, path), "log_user_prompt = true", "log_user_prompt = false", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatalf("edit: %v", err)
	}

	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if !reflect.DeepEqual(result.Skipped, []string{"log_user_prompt"}) {
		t.Errorf("skipped = %v, want the edited key", result.Skipped)
	}
	if otelOf(t, path)["log_user_prompt"] != false {
		t.Error("the user's edit was thrown away")
	}
	if _, ok := otelOf(t, path)["exporter"]; ok {
		t.Error("an unedited Terma key survived")
	}
}

func TestCodexStatusRoundTrip(t *testing.T) {
	c, _ := codexIn(t, "")

	before, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if before.Connected || before.Exists {
		t.Errorf("a missing config reported connected=%v exists=%v", before.Connected, before.Exists)
	}

	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	after, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !after.Connected {
		t.Error("status did not report a freshly connected harness")
	}
	if after.Endpoint != termaEndpoint {
		t.Errorf("endpoint = %q, want the base URL with the signal path stripped", after.Endpoint)
	}
	if !reflect.DeepEqual(after.Signals, harness.AllSignals) {
		t.Errorf("signals = %v, want %v", after.Signals, harness.AllSignals)
	}
	if after.ProjectID != "proj_123" {
		t.Errorf("project = %q, want it read back from span_attributes", after.ProjectID)
	}
	if after.ManagedKeys == 0 {
		t.Error("no managed keys counted after a connect")
	}
	if len(after.Conflicts) != 0 {
		t.Errorf("conflicts = %+v, want none in a config Terma just wrote", after.Conflicts)
	}
}

func TestCodexStatusNeverReturnsTheWholeKey(t *testing.T) {
	const key = "ter_srv_0123456789abcdef0123456789abcdef"
	c, _ := codexIn(t, "")
	e := codexExporter()
	e.APIKey = key
	if err := c.Connect(e, false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.KeyPrefix == "" || strings.Contains(st.KeyPrefix, key) || !strings.HasPrefix(st.KeyPrefix, "ter_srv_") {
		t.Fatalf("key prefix = %q", st.KeyPrefix)
	}
}

// A metrics exporter with analytics off is not reported as metrics on.
func TestCodexStatusDropsMetricsWhenAnalyticsDisabled(t *testing.T) {
	c, _ := codexIn(t, "[analytics]\nenabled = false\n")
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !reflect.DeepEqual(st.Signals, []harness.Signal{harness.SignalTraces, harness.SignalLogs}) {
		t.Errorf("signals = %v, want metrics dropped", st.Signals)
	}
	if len(st.Conflicts) != 1 || st.Conflicts[0].Key != "analytics.enabled" {
		t.Errorf("conflicts = %+v, want analytics.enabled named", st.Conflicts)
	}
}

func TestCodexConflictsReportForeignExporter(t *testing.T) {
	t.Run("other collector", func(t *testing.T) {
		c, _ := codexIn(t, "[otel]\nexporter = { otlp-http = { endpoint = \"https://other.example.com/v1/logs\", protocol = \"binary\" } }\n")
		conflicts, err := c.ConflictsWith(termaExporter())
		if err != nil {
			t.Fatalf("ConflictsWith: %v", err)
		}
		if len(conflicts) != 1 || conflicts[0].Key != "otel.exporter" || !conflicts[0].Clearable {
			t.Fatalf("got %+v, want the log exporter reported as replaceable", conflicts)
		}
		if conflicts[0].Credential {
			t.Error("a Codex exporter carries its own headers; replacing it discloses nothing")
		}
	})
	t.Run("not exported by terma", func(t *testing.T) {
		c, _ := codexIn(t, "[otel]\nexporter = { otlp-http = { endpoint = \"https://other.example.com/v1/logs\", protocol = \"binary\" } }\n")
		conflicts, err := c.ConflictsWith(harness.Exporter{Endpoint: termaEndpoint, Signals: []harness.Signal{harness.SignalTraces}})
		if err != nil {
			t.Fatalf("ConflictsWith: %v", err)
		}
		if len(conflicts) != 0 {
			t.Fatalf("got %+v, want none — logs are not being exported", conflicts)
		}
	})
	t.Run("reconnect", func(t *testing.T) {
		c, _ := codexIn(t, "")
		if err := c.Connect(codexExporter(), false); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		conflicts, err := c.ConflictsWith(termaExporter())
		if err != nil {
			t.Fatalf("ConflictsWith: %v", err)
		}
		if len(conflicts) != 0 {
			t.Fatalf("got %+v, want none on a reconnect", conflicts)
		}
	})
	t.Run("base url is the wrong path", func(t *testing.T) {
		c, _ := codexIn(t, "[otel]\ntrace_exporter = { otlp-http = { endpoint = \""+termaEndpoint+"\", protocol = \"binary\" } }\n")
		conflicts, err := c.ConflictsWith(termaExporter())
		if err != nil {
			t.Fatalf("ConflictsWith: %v", err)
		}
		if len(conflicts) != 1 || !strings.Contains(conflicts[0].Reason, "wrong path") {
			t.Fatalf("got %+v, want the bare base URL reported", conflicts)
		}
	})
	t.Run("defaults are not conflicts", func(t *testing.T) {
		c, _ := codexIn(t, "[otel]\nexporter = \"none\"\nmetrics_exporter = \"statsig\"\n")
		conflicts, err := c.ConflictsWith(termaExporter())
		if err != nil {
			t.Fatalf("ConflictsWith: %v", err)
		}
		if len(conflicts) != 0 {
			t.Fatalf("got %+v, want none for Codex's own defaults", conflicts)
		}
	})
}

// Terma refuses the metrics signal rather than flipping the user's analytics opt-out.
func TestCodexConflictsReportAnalyticsOptOut(t *testing.T) {
	c, _ := codexIn(t, "[analytics]\nenabled = false\n")
	conflicts, err := c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].Key != "analytics.enabled" || conflicts[0].Clearable {
		t.Fatalf("got %+v, want analytics.enabled reported as unclearable", conflicts)
	}
	// The key this replaced never existed in Codex, so it opts out of nothing.
	c, _ = codexIn(t, "analytics_enabled = false\n")
	conflicts, err = c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("got %+v, want none for a key Codex does not read", conflicts)
	}
	conflicts, err = c.ConflictsWith(harness.Exporter{Endpoint: termaEndpoint, Signals: []harness.Signal{harness.SignalTraces, harness.SignalLogs}})
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("got %+v, want none without the metrics signal", conflicts)
	}
}

// An exporter in a project's .codex/config.toml is inert, so it does not block a connect.
func TestCodexIgnoresProjectConfig(t *testing.T) {
	c, _ := codexIn(t, "")
	repo := t.TempDir()
	for _, dir := range []string{".git", ".codex"} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, ".codex", "config.toml"),
		[]byte("[otel]\nexporter = { otlp-http = { endpoint = \"https://other.example.com/v1/logs\", protocol = \"binary\" } }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	conflicts, err := c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("got %+v, want none — Codex does not read otel from project config", conflicts)
	}
}

// Every profile file's exporters are reported as advisory, naming the file, never
// blocking.
func TestCodexConflictsReportProfileFiles(t *testing.T) {
	c, path := codexIn(t, "")
	profile := filepath.Join(filepath.Dir(path), "work.config.toml")
	if err := os.WriteFile(profile, []byte("model = \"gpt-5\"\n\n[otel]\nexporter = \"none\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	conflicts, err := c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].Scope != harness.ScopeProfile || conflicts[0].Clearable || !conflicts[0].Advisory {
		t.Fatalf("got %+v, want the profile exporter reported as advisory", conflicts)
	}
	if !strings.Contains(conflicts[0].Key, "work.config.toml") || !strings.Contains(conflicts[0].Reason, "--profile work") {
		t.Errorf("conflict %+v does not name the profile", conflicts[0])
	}
	if err := os.WriteFile(profile, []byte("model = \"gpt-5\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if conflicts, _ := c.ConflictsWith(termaExporter()); len(conflicts) != 0 {
		t.Fatalf("got %+v, want none", conflicts)
	}
}

// Managed layers parse the same way, and an explicit "none" counts as a destination.
func TestCodexConflictsInManagedLayer(t *testing.T) {
	layer := codexLayer{source: "/etc/codex/managed_config.toml", scope: harness.ScopeManaged, where: "managed"}
	data := []byte("[otel]\nexporter = \"none\"\ntrace_exporter = { otlp-grpc = { endpoint = \"https://collector.example.com:4317\" } }\n")
	conflicts := codexConflictsInLayer(data, layer, termaExporter())
	if len(conflicts) != 2 {
		t.Fatalf("got %+v, want both managed exporters reported", conflicts)
	}
	for _, c := range conflicts {
		if c.Clearable || c.Advisory || c.Scope != harness.ScopeManaged || !strings.HasPrefix(c.Key, "/etc/codex/managed_config.toml:otel.") {
			t.Errorf("conflict %+v is not reported as a blocking managed setting", c)
		}
	}
	if conflicts := codexConflictsInLayer(data, layer, harness.Exporter{Endpoint: termaEndpoint, Signals: []harness.Signal{harness.SignalMetrics}}); len(conflicts) != 0 {
		t.Fatalf("got %+v, want none for a signal Terma is not exporting", conflicts)
	}
	// The same destination named in a higher layer changes nothing.
	same := []byte("[otel]\nexporter = { otlp-http = { endpoint = \"" + termaEndpoint + "/v1/logs\", protocol = \"binary\" } }\n")
	if conflicts := codexConflictsInLayer(same, layer, termaExporter()); len(conflicts) != 0 {
		t.Fatalf("got %+v, want none for Terma's own endpoint", conflicts)
	}
}

// A higher layer's content switches, attribution and analytics opt-out are reported only
// when they contradict what Terma is about to write; content it always asks for.
func TestCodexConflictsInLayerCoverPrivacySettings(t *testing.T) {
	layer := codexLayer{source: "work.config.toml", scope: harness.ScopeProfile, where: "profile", advisory: true}
	data := []byte(`[analytics]
enabled = false

[otel]
log_user_prompt = false
tool_result = { max_bytes = 0 }
span_attributes = { "mirador.project.id" = "proj_other", team = "payments" }
`)

	redacted := harness.Exporter{
		Endpoint: termaEndpoint, Signals: harness.AllSignals,
		ResourceAttributes: map[string]string{harness.AttrProjectID: "proj_123"},
	}
	conflicts := codexConflictsInLayer(data, layer, redacted)
	var keys []string
	for _, c := range conflicts {
		keys = append(keys, c.Key)
		if !c.Advisory || c.Clearable {
			t.Errorf("profile conflict %+v must be advisory and unclearable", c)
		}
	}
	want := []string{
		"work.config.toml:otel.log_user_prompt",
		"work.config.toml:otel.tool_result.max_bytes",
		"work.config.toml:otel.span_attributes.mirador.project.id",
		"work.config.toml:analytics.enabled",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("conflict keys = %v, want %v", keys, want)
	}

	agreeing := harness.Exporter{
		Endpoint: termaEndpoint, Signals: []harness.Signal{harness.SignalTraces, harness.SignalLogs},
		ResourceAttributes: map[string]string{harness.AttrProjectID: "proj_other"},
	}
	data = []byte(`[otel]
log_user_prompt = true
tool_result = { max_bytes = 2048 }
span_attributes = { "mirador.project.id" = "proj_other", team = "payments" }
`)
	if conflicts := codexConflictsInLayer(data, layer, agreeing); len(conflicts) != 0 {
		t.Fatalf("got %+v, want none when the layer agrees with the connect", conflicts)
	}
}

// A config without a journal is somebody else's, here a company collector: nothing in it
// is Terma's to count, name or remove.
func TestCodexWithoutJournalTouchesNothing(t *testing.T) {
	const seed = `[otel]
environment = "prod"
exporter = { otlp-http = { endpoint = "https://collector.example.com/v1/logs", protocol = "binary", headers = { Authorization = "Bearer company-secret-token" } } }
log_user_prompt = true
tool_result = { max_bytes = 4096 }
span_attributes = { team = "payments" }
`
	c, path := codexIn(t, seed)

	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ManagedKeys != 0 {
		t.Errorf("managed keys = %d, want none counted in a config Terma never wrote", st.ManagedKeys)
	}
	if st.KeyPrefix != "" {
		t.Errorf("key prefix = %q, want a foreign credential left unmentioned", st.KeyPrefix)
	}
	if !st.Connected || st.Endpoint != "https://collector.example.com" {
		t.Errorf("connected=%v endpoint=%q, want the foreign export still described", st.Connected, st.Endpoint)
	}

	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if result.Removed != 0 || result.Restored != 0 {
		t.Errorf("disconnect changed %d keys in a config Terma never wrote", result.Removed+result.Restored)
	}
	if got := readText(t, path); got != seed {
		t.Fatalf("the foreign config was modified:\n%s", got)
	}
}

// A reconnect asking for less puts the metrics exporter back to what it was.
func TestCodexReconnectWithFewerSignalsRestoresTheMetricsExporter(t *testing.T) {
	for name, seed := range map[string]string{"explicit statsig": "[otel]\nmetrics_exporter = \"statsig\"\n", "absent": ""} {
		t.Run(name, func(t *testing.T) {
			c, path := codexIn(t, seed)
			if err := c.Connect(codexExporter(), false); err != nil {
				t.Fatalf("first connect: %v", err)
			}
			if shape := codexExporterOf(otelOf(t, path)["metrics_exporter"]); shape.Kind != "otlp-http" {
				t.Fatalf("metrics exporter after full connect = %v", otelOf(t, path)["metrics_exporter"])
			}

			fewer := codexExporter()
			fewer.Signals = []harness.Signal{harness.SignalTraces, harness.SignalLogs}
			if err := c.Connect(fewer, false); err != nil {
				t.Fatalf("reconnect: %v", err)
			}
			got, present := otelOf(t, path)["metrics_exporter"]
			if seed == "" && present {
				t.Fatalf("metrics_exporter = %v, want it removed", got)
			}
			if seed != "" && got != "statsig" {
				t.Fatalf("metrics_exporter = %v, want the user's statsig back", got)
			}

			// The journal agrees, and a file that held only Terma's table goes away.
			if _, err := c.Disconnect(); err != nil {
				t.Fatalf("Disconnect: %v", err)
			}
			if seed == "" {
				if fileExists(path) {
					t.Fatalf("a file Terma created was left behind:\n%s", readText(t, path))
				}
			} else if got := readText(t, path); got != seed {
				t.Fatalf("after disconnect:\n%s\nwant:\n%s", got, seed)
			}
		})
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// An earlier terma's zero tool-output cap goes at the next connect, which restores the
// user's own: content always goes to the relay now.
func TestCodexReconnectLiftsAnEarlierTermasZeroCap(t *testing.T) {
	c, path := codexIn(t, "[otel]\ntool_result = { max_bytes = 8192 }\n")
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("connect: %v", err)
	}
	// What an earlier terma's connect with tool content off left: the cap, and a journal
	// that owns it.
	j, err := harness.LoadJournal(c.Name(), path)
	if err != nil || j == nil {
		t.Fatalf("journal: %v, %v", j, err)
	}
	zero, user := mustRenderTOML(map[string]any{"max_bytes": int64(0)}), mustRenderTOML(map[string]any{"max_bytes": int64(8192)})
	j.Installed["tool_result"], j.Previous["tool_result"] = zero, &user
	if err := j.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(readText(t, path), "max_bytes = 8192", "max_bytes = 0", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	table, _ := otelOf(t, path)["tool_result"].(map[string]any)
	if table["max_bytes"] != int64(8192) {
		t.Fatalf("tool_result = %v, want the user's 8192 cap restored", otelOf(t, path)["tool_result"])
	}
}

func TestCodexConnectWithForceReplacesForeignExporterAndDisconnectRestoresIt(t *testing.T) {
	const seed = "[otel]\nexporter = { otlp-http = { endpoint = \"https://other.example.com/v1/logs\", protocol = \"binary\" } }\n"
	c, path := codexIn(t, seed)
	if err := c.Connect(codexExporter(), true); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if strings.Contains(readText(t, path), "other.example.com") {
		t.Fatal("--force left the foreign exporter in place")
	}
	if _, err := c.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if got := readText(t, path); got != seed {
		t.Fatalf("after disconnect:\n%s\nwant the foreign exporter restored:\n%s", got, seed)
	}
}

func TestCodexCurrentCredential(t *testing.T) {
	c, _ := codexIn(t, "")
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if key, ok := c.CurrentCredential(termaEndpoint, "proj_123"); !ok || key != "ter_srv_0123456789abcdef" {
		t.Errorf("CurrentCredential = (%q, %v), want the installed key", key, ok)
	}
	if _, ok := c.CurrentCredential(termaEndpoint, "proj_other"); ok {
		t.Error("a key for another project was offered for reuse")
	}
	if _, ok := c.CurrentCredential("https://otel.example.com", "proj_123"); ok {
		t.Error("a key for another endpoint was offered for reuse")
	}
}

func TestCodexBackupCopiesTheOriginal(t *testing.T) {
	c, _ := codexIn(t, codexSeed)
	backup, err := c.Backup(termaEndpoint)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if backup == "" || readText(t, backup) != codexSeed {
		t.Fatalf("backup %q is not a verbatim copy", backup)
	}
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if readText(t, backup) != codexSeed {
		t.Fatal("the backup was overwritten by the connect it exists to protect against")
	}
}

// A symlinked config.toml is written through, leaving the link in place.
func TestCodexConnectWritesThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	c, path := codexIn(t, "")
	target := filepath.Join(t.TempDir(), "codex-config.toml")
	if err := os.WriteFile(target, []byte(codexSeed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a regular file")
	}
	if !strings.Contains(readText(t, target), "[otel]") {
		t.Fatal("the link target was not written")
	}
}

func TestCodexConfigPathHonoursCodexHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "/tmp/elsewhere")
	path, err := exporter{}.ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if path != filepath.Join("/tmp/elsewhere", "config.toml") {
		t.Fatalf("ConfigPath = %q, want it under CODEX_HOME", path)
	}
}

func TestCodexDetectDoesNotFailWhenAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if d := (exporter{}).Detect(context.Background()); d.Found {
		t.Fatalf("Detect reported a harness found on an empty PATH: %+v", d)
	}
}

func TestCodexConnectNotes(t *testing.T) {
	notes := exporter{}.ConnectNotes(codexExporter())
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want the metrics route", notes)
	}
	quiet := codexExporter()
	quiet.Signals = []harness.Signal{harness.SignalTraces}
	if notes := (exporter{}).ConnectNotes(quiet); len(notes) != 0 {
		t.Fatalf("notes = %v, want none", notes)
	}
}

func TestCodexCurrentCredentialAcceptsTermaPrefix(t *testing.T) {
	c, _ := codexIn(t, "")
	e := codexExporter()
	e.APIKey = "ter_srv_00112233445566778899aabbccddeeff"
	if err := c.Connect(e, false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if key, ok := c.CurrentCredential(termaEndpoint, "proj_123"); !ok || key != e.APIKey {
		t.Errorf("CurrentCredential = (%q, %v), want the ter_srv_ key", key, ok)
	}
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.HasPrefix(st.KeyPrefix, "ter_srv_") {
		t.Errorf("key prefix = %q, want the installed key's head", st.KeyPrefix)
	}
}
