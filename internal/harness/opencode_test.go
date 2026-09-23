package harness

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// opencodeIn sandboxes OpenCode's config directory (XDG_CONFIG_HOME) and Terma's own,
// so a connect here writes a throwaway plugins directory and helper.
func opencodeIn(t *testing.T) (OpenCode, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(dir, "terma"))
	return OpenCode{}, filepath.Join(dir, "opencode", "plugins", "terma.js")
}

func opencodeExporter(t *testing.T, h OpenCode, helper bool) Exporter {
	t.Helper()
	e := fullExporter()
	e.IncludePrompts, e.IncludeToolContent = true, false
	if helper {
		path, err := HelperFilePath(h, "proj_123")
		if err != nil {
			t.Fatal(err)
		}
		e.HelperPath = path
	}
	return e
}

// The embedded plugin and the Go side share one line: the config placeholder. If
// either drifts, every connect would install an inert plugin.
func TestOpenCodeTemplateHasOneConfigLineAndOneExport(t *testing.T) {
	if n := strings.Count(opencodePluginSource, opencodeConfigPlaceholder); n != 1 {
		t.Fatalf("placeholder appears %d times, want 1", n)
	}
	exports := 0
	for line := range strings.SplitSeq(opencodePluginSource, "\n") {
		if strings.HasPrefix(line, "export ") {
			exports++
		}
	}
	// OpenCode's loader treats every export as a plugin function; a stray helper export
	// would make the plugin fail to load.
	if exports != 1 {
		t.Fatalf("plugin has %d exports, want exactly 1 (the plugin function)", exports)
	}
	if _, ok := readPluginConfig([]byte(opencodePluginSource)); ok {
		t.Fatal("the raw template must read as unconfigured")
	}
}

func TestOpenCodeConnectWritesPluginAndHelper(t *testing.T) {
	h, path := opencodeIn(t)
	e := opencodeExporter(t, h, true)
	if err := h.Connect(e, false); err != nil {
		t.Fatalf("connect: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("plugin not written: %v", err)
	}
	if strings.Contains(string(data), opencodeConfigPlaceholder) {
		t.Fatal("placeholder left in place")
	}
	if strings.Contains(string(data), e.APIKey) {
		t.Fatal("the key landed in the plugin file; it belongs in the helper")
	}
	cfg, ok := readPluginConfig(data)
	if !ok {
		t.Fatal("config line not readable back")
	}
	if cfg.Endpoint != e.Endpoint || cfg.HeadersHelper != e.HelperPath || len(cfg.Headers) != 0 {
		t.Errorf("config = %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.Signals, []string{"traces", "logs", "metrics"}) || !cfg.IncludePrompts || cfg.IncludeToolContent {
		t.Errorf("policy = %v / %v / %v", cfg.Signals, cfg.IncludePrompts, cfg.IncludeToolContent)
	}
	if cfg.ResourceAttributes[AttrProjectID] != "proj_123" || cfg.ResourceAttributes[AttrEnduserID] != "dev@example.com" {
		t.Errorf("resource attributes = %v", cfg.ResourceAttributes)
	}
	if !reflect.DeepEqual(cfg.HookCommand, []string{"terma", "hook"}) {
		t.Errorf("hook command = %v", cfg.HookCommand)
	}
	// No secret in the file, so it is readable like any other plugin.
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Errorf("plugin mode = %o, want 0644", info.Mode().Perm())
	}
	if info, err := os.Stat(e.HelperPath); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("helper: %v, mode %v", err, info)
	}
	if keyFromHelper(e.HelperPath) != e.APIKey {
		t.Fatal("helper does not hold the key")
	}
	// The user's own config was never touched, or created.
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(path)), "opencode.json")); err == nil {
		t.Fatal("connect wrote opencode.json")
	}
}

func TestOpenCodeStatusRoundTrip(t *testing.T) {
	h, path := opencodeIn(t)
	st, err := h.Status()
	if err != nil || st.Exists || st.Connected || st.ManagedKeys != 0 || st.ConfigPath != path {
		t.Fatalf("empty status = %+v, %v", st, err)
	}

	e := opencodeExporter(t, h, true)
	e.Signals = []Signal{SignalTraces, SignalLogs}
	if err := h.Connect(e, false); err != nil {
		t.Fatal(err)
	}
	st, err = h.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exists || !st.Connected || st.Endpoint != e.Endpoint || st.ManagedKeys != 1 {
		t.Errorf("status = %+v", st)
	}
	if !reflect.DeepEqual(st.Signals, []Signal{SignalTraces, SignalLogs}) || !st.IncludePrompts || st.IncludeToolContent {
		t.Errorf("policy = %v / %v / %v", st.Signals, st.IncludePrompts, st.IncludeToolContent)
	}
	if st.KeyPrefix != MaskKey(e.APIKey) || strings.Contains(st.KeyPrefix, e.APIKey[len(e.APIKey)-6:]) {
		t.Errorf("key prefix = %q", st.KeyPrefix)
	}
	if st.ProjectID != "proj_123" {
		t.Errorf("project = %q", st.ProjectID)
	}

	key, ok := h.CurrentCredential(e.Endpoint, "proj_123")
	if !ok || key != e.APIKey {
		t.Errorf("CurrentCredential = %q, %v", key, ok)
	}
	if _, ok := h.CurrentCredential(e.Endpoint, "other"); ok {
		t.Error("a key for another project must not be reused")
	}
	if _, ok := h.CurrentCredential("https://elsewhere.example.com", "proj_123"); ok {
		t.Error("a key for another endpoint must not be reused")
	}
}

func TestOpenCodeInlineKeyIsTightened(t *testing.T) {
	h, path := opencodeIn(t)
	e := opencodeExporter(t, h, false)
	if err := h.Connect(e, false); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600 with the key inline", info.Mode().Perm())
	}
	st, _ := h.Status()
	if !st.Connected || st.KeyPrefix != MaskKey(e.APIKey) {
		t.Errorf("status = %+v", st)
	}
	if key, ok := h.CurrentCredential(e.Endpoint, "proj_123"); !ok || key != e.APIKey {
		t.Errorf("CurrentCredential = %q, %v", key, ok)
	}
}

func TestOpenCodeDisconnectRemovesPluginAndHelper(t *testing.T) {
	h, path := opencodeIn(t)
	e := opencodeExporter(t, h, true)
	if err := h.Connect(e, false); err != nil {
		t.Fatal(err)
	}
	result, err := h.Disconnect()
	if err != nil || result.Removed != 1 {
		t.Fatalf("disconnect = %+v, %v", result, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("plugin file survived")
	}
	if _, err := os.Stat(e.HelperPath); err == nil {
		t.Error("helper survived — it holds a live key")
	}
	result, err = h.Disconnect()
	if err != nil || result.Removed != 0 {
		t.Fatalf("second disconnect = %+v, %v", result, err)
	}
}

func TestOpenCodeLocalPolicyCarriesNoDestination(t *testing.T) {
	_, _ = opencodeIn(t)
	repo := t.TempDir()
	h := OpenCode{}.Local(repo)
	if ScopeOf(h) != ScopeLocal {
		t.Fatal("Local must bind to the repository scope")
	}
	e := opencodeExporter(t, OpenCode{}, false)
	e.Signals = []Signal{SignalTraces}
	if err := h.Connect(e, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, ".opencode", "terma.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"endpoint", "headers", "Bearer", e.APIKey, "resourceAttributes"} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("policy file carries %q:\n%s", forbidden, data)
		}
	}
	st, err := h.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Connected || st.ManagedKeys != 1 || !reflect.DeepEqual(st.Signals, []Signal{SignalTraces}) || !st.IncludePrompts {
		t.Errorf("local status = %+v", st)
	}
	if _, ok := h.(interface {
		CurrentCredential(string, string) (string, bool)
	}).CurrentCredential(e.Endpoint, "proj_123"); ok {
		t.Error("a policy file holds no credential to reuse")
	}
	if result, err := h.Disconnect(); err != nil || result.Removed != 1 {
		t.Fatalf("disconnect = %+v, %v", result, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("policy file survived")
	}
	// The global plugin was never written by a local connect.
	if global, _ := (OpenCode{}).ConfigPath(); fileExists(global) {
		t.Error("a local connect wrote the global plugin")
	}
}

// OpenCode's native export is a separate stream driven by the shell; it is reported so a
// second copy of the data is not a mystery, and it never blocks — nothing about it can
// disclose Terma's key.
func TestOpenCodeConflictsAreAdvisoryOnly(t *testing.T) {
	h, _ := opencodeIn(t)
	e := opencodeExporter(t, h, true)
	if got, _ := h.ConflictsWith(e); len(got) != 0 {
		t.Fatalf("no env, yet conflicts: %+v", got)
	}
	t.Setenv(otelEndpoint, e.Endpoint)
	if got, _ := h.ConflictsWith(e); len(got) != 0 {
		t.Fatalf("same endpoint reported: %+v", got)
	}
	t.Setenv(otelEndpoint, "https://other.example.com")
	got, _ := h.ConflictsWith(e)
	if len(got) != 1 || !got[0].Advisory || got[0].Clearable || got[0].Credential || got[0].Scope != ScopeEnvironment {
		t.Fatalf("conflicts = %+v", got)
	}
}

func TestOpenCodeServiceNameAndRegistry(t *testing.T) {
	if ServiceName(OpenCode{}) != "opencode" {
		t.Error("service name")
	}
	h, err := Lookup("opencode")
	if err != nil || h.Name() != "opencode" {
		t.Fatalf("Lookup: %v", err)
	}
	if _, ok := h.(Scoped); !ok {
		t.Error("OpenCode has a repository policy file and must be Scoped")
	}
}

func TestOpenCodeDetectDoesNotFailWhenAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if d := (OpenCode{}).Detect(t.Context()); d.Found {
		t.Fatalf("found %+v on an empty PATH", d)
	}
}

// ConnectPerRepo installs one global plugin in per-repo mode plus this project's helper.
// The plugin file names no fixed project and holds no key — the project is resolved from
// each session's repository at runtime, and the key lives in the per-project helper.
func TestOpenCodeConnectPerRepo(t *testing.T) {
	h, pluginPath := opencodeIn(t)
	e := Exporter{
		Endpoint:           "https://otel.example.com",
		APIKey:             "ter_srv_perrepo0123456789",
		ProjectID:          "proj_123",
		Signals:            AllSignals,
		ResourceAttributes: map[string]string{AttrServiceName: "opencode", AttrProjectID: "proj_123", AttrEnduserID: "dev@example.com"},
		IncludePrompts:     true,
		IncludeToolContent: true,
	}
	if err := h.ConnectPerRepo(e); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("plugin not written: %v", err)
	}
	cfg, ok := readPluginConfig(data)
	if !ok {
		t.Fatalf("plugin config not readable:\n%s", data)
	}
	if !cfg.PerRepo {
		t.Fatal("plugin is not in per-repo mode")
	}
	if cfg.Endpoint != e.Endpoint {
		t.Fatalf("endpoint = %q, want %q", cfg.Endpoint, e.Endpoint)
	}
	if cfg.HelperPrefix != "opencode-otel-" || cfg.ProjectAttribute != AttrProjectID {
		t.Fatalf("per-repo fields wrong: %+v", cfg)
	}
	// The shared plugin must never carry one repo's content-capture choice, even though
	// this exporter asked for both: otherwise installing this project would flip prompt
	// and tool-content capture on for every other project with no policy of its own.
	// Content capture is opt-in per repository via its committed .opencode/terma.json.
	if cfg.IncludePrompts || cfg.IncludeToolContent {
		t.Fatalf("shared per-repo plugin leaked content capture: prompts=%v toolContent=%v", cfg.IncludePrompts, cfg.IncludeToolContent)
	}
	// The project id must not be baked into a plugin shared across projects.
	if _, ok := cfg.ResourceAttributes[AttrProjectID]; ok {
		t.Fatalf("project id must not be in the shared plugin: %+v", cfg.ResourceAttributes)
	}
	// The key never sits in the plugin file.
	if strings.Contains(string(data), e.APIKey) {
		t.Fatal("the key reached the plugin file")
	}
	// The project's helper carries the key.
	helper, err := HelperFilePath(h, "proj_123")
	if err != nil {
		t.Fatal(err)
	}
	hdata, err := os.ReadFile(helper)
	if err != nil {
		t.Fatalf("per-project helper not written: %v", err)
	}
	if !strings.Contains(string(hdata), e.APIKey) {
		t.Fatalf("helper does not carry the key:\n%s", hdata)
	}
}
