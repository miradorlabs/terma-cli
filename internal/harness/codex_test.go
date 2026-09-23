package harness

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// codexIn points Codex's config at a temp dir, so every test here works against a
// throwaway file rather than the developer's real ~/.codex/config.toml.
func codexIn(t *testing.T, contents string) (Codex, string) {
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
	return Codex{}, path
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

func codexExporter() Exporter {
	e := fullExporter()
	e.ResourceAttributes[AttrServiceName] = codexServiceName
	return e
}

// A realistic config.toml: comments, a trailing comment, nested MCP tables, an env
// table whose values look like secrets, and a profile. None of it may change.
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
	env := Codex{}.render(codexExporter())

	want := map[string]string{
		"trace_exporter":   `{ otlp-http = { endpoint = "https://otel.terma.ai/v1/traces", headers = { Authorization = "Bearer mir_srv_0123456789abcdef" }, protocol = "binary" } }`,
		"exporter":         `{ otlp-http = { endpoint = "https://otel.terma.ai/v1/logs", headers = { Authorization = "Bearer mir_srv_0123456789abcdef" }, protocol = "binary" } }`,
		"metrics_exporter": `{ otlp-http = { endpoint = "https://otel.terma.ai/v1/metrics", headers = { Authorization = "Bearer mir_srv_0123456789abcdef" }, protocol = "binary" } }`,
		"log_user_prompt":  "false",
		"tool_result":      "{ max_bytes = 0 }",
		// service.name is Codex's own; only the attribution keys go on spans, and each
		// is its own entry so the table's other attributes are never Terma's.
		"span_attributes/enduser.id":         `"dev@example.com"`,
		"span_attributes/mirador.project.id": `"proj_123"`,
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("Render() =\n%#v\nwant\n%#v", env, want)
	}
}

// Codex's exporters are self-contained, so an unselected signal is left alone rather
// than written as "none": for metrics that would switch off OpenAI's own route, which
// is not Terma's to do.
func TestCodexRenderLeavesUnselectedSignalsAlone(t *testing.T) {
	env := Codex{}.render(Exporter{Endpoint: termaEndpoint, Signals: []Signal{SignalLogs}})
	if _, ok := env["exporter"]; !ok {
		t.Error("the log exporter was not written")
	}
	for _, key := range []string{"trace_exporter", "metrics_exporter"} {
		if v, ok := env[key]; ok {
			t.Errorf("%s = %q, want it left unwritten", key, v)
		}
	}
}

func TestCodexRenderContentSwitches(t *testing.T) {
	on := Codex{}.render(Exporter{Signals: AllSignals, IncludePrompts: true, IncludeToolContent: true})
	if on["log_user_prompt"] != "true" {
		t.Errorf("log_user_prompt = %q, want true", on["log_user_prompt"])
	}
	// With content on, Codex's own cap — or one the user chose — stays in force.
	if v, ok := on["tool_result"]; ok {
		t.Errorf("tool_result = %q, want it unwritten when tool content is on", v)
	}

	off := Codex{}.render(Exporter{Signals: AllSignals})
	if off["log_user_prompt"] != "false" || off["tool_result"] != "{ max_bytes = 0 }" {
		t.Errorf("exclusions rendered as %v", off)
	}
}

// The file is hand-written and full of things this CLI knows nothing about. A connect
// appends one table and changes no other byte.
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
	// And the rest still parses to exactly what it was.
	doc := mustParse(t, got)
	delete(doc, "otel")
	if !reflect.DeepEqual(doc, mustParse(t, codexSeed)) {
		t.Fatalf("settings outside otel changed:\n%v", doc)
	}
}

// An existing table is rewritten in place. Keys in it that Terma does not own — the
// user's environment — survive; the next table's comment stays with the next table.
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

// span_attributes is the user's table as much as Terma's. An attribute already in it
// survives the connect alongside Terma's two, and the disconnect gives back exactly
// the original text.
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

// An attribute added after the connect is the user's and stays; Terma's own entries
// are still recognized and removed, since ownership stops at the entry rather than at
// the table.
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

// Terma overwrites an attribute of its own name that the user had set, and puts
// the user's value back on disconnect.
func TestCodexDisconnectRestoresOverwrittenSpanAttribute(t *testing.T) {
	const seed = "[otel]\nspan_attributes = { \"enduser.id\" = \"someone@example.com\" }\n"
	c, path := codexIn(t, seed)
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := codexSpanAttribute(otelOf(t, path), AttrEnduserID); got != "dev@example.com" {
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

// config.toml now holds a live server key, and Codex has no other place to put it.
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

// Connect then disconnect leaves the file exactly as it was — not a parsed-and-reprinted
// version of it.
func TestCodexDisconnectRestoresOriginalTextExactly(t *testing.T) {
	c, path := codexIn(t, codexSeed)
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if result.Removed != len(Codex{}.render(codexExporter())) {
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

// A key edited since the connect is the user's, and disconnect names it rather than
// deleting it.
func TestCodexDisconnectLeavesEditedKeysAlone(t *testing.T) {
	c, path := codexIn(t, "")
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	edited := strings.Replace(readText(t, path), "log_user_prompt = false", "log_user_prompt = true", 1)
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
	if otelOf(t, path)["log_user_prompt"] != true {
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

	e := codexExporter()
	e.IncludePrompts = true
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
	if after.Endpoint != termaEndpoint {
		t.Errorf("endpoint = %q, want the base URL with the signal path stripped", after.Endpoint)
	}
	if !reflect.DeepEqual(after.Signals, AllSignals) {
		t.Errorf("signals = %v, want %v", after.Signals, AllSignals)
	}
	if !after.IncludePrompts || !after.IncludeToolContent {
		t.Errorf("prompts=%v tool content=%v, want both on", after.IncludePrompts, after.IncludeToolContent)
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
	const key = "mir_srv_0123456789abcdef0123456789abcdef"
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
	if st.KeyPrefix == "" || strings.Contains(st.KeyPrefix, key) || !strings.HasPrefix(st.KeyPrefix, "mir_srv_") {
		t.Fatalf("key prefix = %q", st.KeyPrefix)
	}
}

// A metrics exporter is set but analytics are off: Codex installs no metrics exporter at
// all. Reporting metrics as on would send someone hunting for data never sent.
func TestCodexStatusDropsMetricsWhenAnalyticsDisabled(t *testing.T) {
	c, _ := codexIn(t, "[analytics]\nenabled = false\n")
	if err := c.Connect(codexExporter(), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !reflect.DeepEqual(st.Signals, []Signal{SignalTraces, SignalLogs}) {
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
		conflicts, err := c.ConflictsWith(Exporter{Endpoint: termaEndpoint, Signals: []Signal{SignalTraces}})
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

// `[analytics] enabled = false` is the user's opt-out from OpenAI's analytics as well as
// the metrics switch. Terma refuses the metrics signal rather than flipping it.
func TestCodexConflictsReportAnalyticsOptOut(t *testing.T) {
	c, _ := codexIn(t, "[analytics]\nenabled = false\n")
	conflicts, err := c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].Key != "analytics.enabled" || conflicts[0].Clearable {
		t.Fatalf("got %+v, want analytics.enabled reported as unclearable", conflicts)
	}
	// The key this replaced never existed in Codex; a config using it opts out of
	// nothing, and must not be treated as if it did.
	c, _ = codexIn(t, "analytics_enabled = false\n")
	conflicts, err = c.ConflictsWith(termaExporter())
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("got %+v, want none for a key Codex does not read", conflicts)
	}
	conflicts, err = c.ConflictsWith(Exporter{Endpoint: termaEndpoint, Signals: []Signal{SignalTraces, SignalLogs}})
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("got %+v, want none without the metrics signal", conflicts)
	}
}

// Codex refuses `otel` from project-local config — repository contents do not get to
// choose where credentials go — so an exporter in .codex/config.toml is inert and must
// not block a connect.
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

// A profile file beside config.toml is applied over it only while that profile is
// selected, and can carry its own exporters. Terma cannot know which profile a
// session will use, so every one is reported — as advisory, naming the file — rather
// than allowed to block the connect that the plain configuration asked for.
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
	if len(conflicts) != 1 || conflicts[0].Scope != ScopeProfile || conflicts[0].Clearable || !conflicts[0].Advisory {
		t.Fatalf("got %+v, want the profile exporter reported as advisory", conflicts)
	}
	if !strings.Contains(conflicts[0].Key, "work.config.toml") || !strings.Contains(conflicts[0].Reason, "--profile work") {
		t.Errorf("conflict %+v does not name the profile", conflicts[0])
	}
	// A profile that sets no exporter for a selected signal is no conflict at all.
	if err := os.WriteFile(profile, []byte("model = \"gpt-5\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if conflicts, _ := c.ConflictsWith(termaExporter()); len(conflicts) != 0 {
		t.Fatalf("got %+v, want none", conflicts)
	}
}

// Managed layers are read the same way; the file itself cannot be written in a test,
// so the parser is exercised directly. An explicit "none" counts: it decides the
// signal's destination just as surely as an endpoint does.
func TestCodexConflictsInManagedLayer(t *testing.T) {
	layer := codexLayer{source: "/etc/codex/managed_config.toml", scope: ScopeManaged, where: "managed"}
	data := []byte("[otel]\nexporter = \"none\"\ntrace_exporter = { otlp-grpc = { endpoint = \"https://collector.example.com:4317\" } }\n")
	conflicts := codexConflictsInLayer(data, layer, termaExporter())
	if len(conflicts) != 2 {
		t.Fatalf("got %+v, want both managed exporters reported", conflicts)
	}
	for _, c := range conflicts {
		if c.Clearable || c.Advisory || c.Scope != ScopeManaged || !strings.HasPrefix(c.Key, "/etc/codex/managed_config.toml:otel.") {
			t.Errorf("conflict %+v is not reported as a blocking managed setting", c)
		}
	}
	if conflicts := codexConflictsInLayer(data, layer, Exporter{Endpoint: termaEndpoint, Signals: []Signal{SignalMetrics}}); len(conflicts) != 0 {
		t.Fatalf("got %+v, want none for a signal Terma is not exporting", conflicts)
	}
	// The same destination named in a higher layer changes nothing.
	same := []byte("[otel]\nexporter = { otlp-http = { endpoint = \"" + termaEndpoint + "/v1/logs\", protocol = \"binary\" } }\n")
	if conflicts := codexConflictsInLayer(same, layer, termaExporter()); len(conflicts) != 0 {
		t.Fatalf("got %+v, want none for Terma's own endpoint", conflicts)
	}
}

// The privacy posture the plan promises can be overturned by a higher layer without
// any exporter changing hands: the content switches, the attribution, and the analytics
// opt-out all merge over the user config key by key. Each is checked against what
// Terma is about to write, and reported only when it contradicts it.
func TestCodexConflictsInLayerCoverPrivacySettings(t *testing.T) {
	layer := codexLayer{source: "work.config.toml", scope: ScopeProfile, where: "profile", advisory: true}
	data := []byte(`[analytics]
enabled = false

[otel]
log_user_prompt = true
tool_result = { max_bytes = 2048 }
span_attributes = { "mirador.project.id" = "proj_other", team = "payments" }
`)

	redacted := Exporter{
		Endpoint: termaEndpoint, Signals: AllSignals,
		ResourceAttributes: map[string]string{AttrProjectID: "proj_123"},
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

	// The same layer agrees with a connect that captures everything for that project
	// and does not export metrics: nothing to report.
	agreeing := Exporter{
		Endpoint: termaEndpoint, Signals: []Signal{SignalTraces, SignalLogs},
		IncludePrompts: true, IncludeToolContent: true,
		ResourceAttributes: map[string]string{AttrProjectID: "proj_other"},
	}
	if conflicts := codexConflictsInLayer(data, layer, agreeing); len(conflicts) != 0 {
		t.Fatalf("got %+v, want none when the layer agrees with the connect", conflicts)
	}
}

// No released build ever wrote a Codex config without a journal, so a config without
// one is somebody else's — here, a company collector with its own bearer token.
// Nothing in it is Terma's to count, name, or remove.
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

// A reconnect asking for less must take away what the earlier connect added: the
// metrics exporter goes back to what it was, whether that was an explicit statsig or
// nothing at all.
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
			fewer.Signals = []Signal{SignalTraces, SignalLogs}
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

			// And the journal agrees: a disconnect now has nothing to say about metrics.
			// A file that only ever held Terma's table goes away with it.
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

// The tool-result cap is only written to exclude content. Turning content back on must
// remove that zero — or put back the cap the user had chosen.
func TestCodexReconnectWithToolContentOnLiftsTheCap(t *testing.T) {
	c, path := codexIn(t, "[otel]\ntool_result = { max_bytes = 8192 }\n")

	excluded := codexExporter()
	if err := c.Connect(excluded, false); err != nil {
		t.Fatalf("connect excluding tool content: %v", err)
	}
	if codexToolContentOn(otelOf(t, path)) {
		t.Fatal("tool output was not excluded")
	}

	included := codexExporter()
	included.IncludeToolContent = true
	if err := c.Connect(included, false); err != nil {
		t.Fatalf("reconnect including tool content: %v", err)
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
	if key, ok := c.CurrentCredential(termaEndpoint, "proj_123"); !ok || key != "mir_srv_0123456789abcdef" {
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

// A dotfiles setup: config.toml is a link into a repo. The write must land on the
// target and leave the link in place.
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
	path, err := Codex{}.ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if path != filepath.Join("/tmp/elsewhere", "config.toml") {
		t.Fatalf("ConfigPath = %q, want it under CODEX_HOME", path)
	}
}

func TestCodexDetectDoesNotFailWhenAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if d := (Codex{}).Detect(context.Background()); d.Found {
		t.Fatalf("Detect reported a harness found on an empty PATH: %+v", d)
	}
}

func TestCodexConnectNotes(t *testing.T) {
	notes := Codex{}.ConnectNotes(codexExporter())
	if len(notes) != 2 {
		t.Fatalf("notes = %v, want the metrics route and the tool-arguments limit", notes)
	}
	quiet := codexExporter()
	quiet.Signals = []Signal{SignalTraces}
	quiet.IncludeToolContent = true
	if notes := (Codex{}).ConnectNotes(quiet); len(notes) != 0 {
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
