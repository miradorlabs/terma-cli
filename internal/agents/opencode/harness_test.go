package opencode

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// opencodeIn sandboxes OpenCode's config directory (XDG_CONFIG_HOME) and terma's own.
func opencodeIn(t *testing.T) (exporter, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(dir, "terma"))
	return exporter{}, filepath.Join(dir, "opencode", "plugins", "terma.js")
}

func opencodeExporter(t *testing.T, h exporter, helper bool) harness.Exporter {
	t.Helper()
	e := baseExporter()
	if helper {
		path, err := harness.HelperFilePath(h, "proj_123")
		if err != nil {
			t.Fatal(err)
		}
		e.HelperPath = path
	}
	return e
}

// The embedded plugin has exactly one config placeholder, or every connect installs an
// inert plugin.
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
	if !reflect.DeepEqual(cfg.Signals, []string{"traces", "logs", "metrics"}) || !cfg.IncludePrompts || !cfg.IncludeToolContent {
		t.Errorf("policy = %v / %v / %v, want every signal and all content", cfg.Signals, cfg.IncludePrompts, cfg.IncludeToolContent)
	}
	if cfg.ResourceAttributes[harness.AttrProjectID] != "proj_123" || cfg.ResourceAttributes[harness.AttrEnduserID] != "dev@example.com" {
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
	if harness.KeyFromHelper(e.HelperPath) != e.APIKey {
		t.Fatal("helper does not hold the key")
	}
	// The user's own config was never touched.
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
	e.Signals = []harness.Signal{harness.SignalTraces, harness.SignalLogs}
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
	if !reflect.DeepEqual(st.Signals, []harness.Signal{harness.SignalTraces, harness.SignalLogs}) {
		t.Errorf("signals = %v", st.Signals)
	}
	if st.KeyPrefix != harness.MaskKey(e.APIKey) || strings.Contains(st.KeyPrefix, e.APIKey[len(e.APIKey)-6:]) {
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
	if !st.Connected || st.KeyPrefix != harness.MaskKey(e.APIKey) {
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
	h, ok := exporter{}.Local(repo)
	if !ok {
		t.Fatal("Local must bind to the repository scope")
	}
	e := opencodeExporter(t, exporter{}, false)
	e.Signals = []harness.Signal{harness.SignalTraces}
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
	if st.Connected || st.ManagedKeys != 1 || !reflect.DeepEqual(st.Signals, []harness.Signal{harness.SignalTraces}) || st.StaleContent {
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
	if global, _ := (exporter{}).ConfigPath(); exists(global) {
		t.Error("a local connect wrote the global plugin")
	}
}

// OpenCode's shell-driven native export is reported but never blocks: it cannot disclose
// terma's key.
func TestOpenCodeConflictsAreAdvisoryOnly(t *testing.T) {
	h, _ := opencodeIn(t)
	e := opencodeExporter(t, h, true)
	if got, _ := h.ConflictsWith(e); len(got) != 0 {
		t.Fatalf("no env, yet conflicts: %+v", got)
	}
	t.Setenv(harness.EnvOTLPEndpoint, e.Endpoint)
	if got, _ := h.ConflictsWith(e); len(got) != 0 {
		t.Fatalf("same endpoint reported: %+v", got)
	}
	t.Setenv(harness.EnvOTLPEndpoint, "https://other.example.com")
	got, _ := h.ConflictsWith(e)
	if len(got) != 1 || !got[0].Advisory || got[0].Clearable || got[0].Credential || got[0].Scope != harness.ScopeEnvironment {
		t.Fatalf("conflicts = %+v", got)
	}
}

func TestOpenCodeServiceNameAndRegistry(t *testing.T) {
	if harness.ServiceName(exporter{}) != "opencode" {
		t.Error("service name")
	}
	if _, ok := (exporter{}).Local(t.TempDir()); !ok {
		t.Error("OpenCode has a repository policy file and must be Scoped")
	}
}

func TestOpenCodeDetectDoesNotFailWhenAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if d := (exporter{}).Detect(t.Context()); d.Found {
		t.Fatalf("found %+v on an empty PATH", d)
	}
}

// An installed plugin gets this build's source around its own configuration, and keeps
// its file mode; an absent or inert one is left alone.
func TestRefreshPluginKeepsItsConfiguration(t *testing.T) {
	h, path := opencodeIn(t)
	if _, changed, err := h.RefreshPlugin(); err != nil || changed {
		t.Fatalf("absent plugin: changed=%v err=%v", changed, err)
	}
	if err := h.Connect(opencodeExporter(t, h, false), false); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(path)
	cfg, ok := readPluginConfig(want)
	if !ok {
		t.Fatal("connect wrote no configuration")
	}
	info, _ := os.Stat(path)
	stale := strings.Replace(string(want), "export", "// an earlier build\nexport", 1)
	if err := os.WriteFile(path, []byte(stale), info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}

	if _, changed, err := h.RefreshPlugin(); err != nil || !changed {
		t.Fatalf("refresh: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(want) {
		t.Fatalf("refreshed plugin differs from this build's:\n%s", got)
	}
	if again, ok := readPluginConfig(got); !ok || again.Endpoint != cfg.Endpoint || len(again.Headers) != len(cfg.Headers) {
		t.Fatalf("configuration not kept: %+v", again)
	}
	if after, _ := os.Stat(path); after.Mode().Perm() != info.Mode().Perm() {
		t.Fatalf("mode %v, want %v", after.Mode().Perm(), info.Mode().Perm())
	}
	if _, changed, err := h.RefreshPlugin(); err != nil || changed {
		t.Fatalf("second refresh: changed=%v err=%v", changed, err)
	}

	inert := []byte(opencodePluginSource + "\n// inert\n")
	if err := os.WriteFile(path, inert, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := h.RefreshPlugin(); err != nil || changed {
		t.Fatalf("inert plugin: changed=%v err=%v", changed, err)
	}
}

func baseExporter() harness.Exporter {
	return harness.Exporter{Endpoint: "https://otel.terma.ai", APIKey: "ter_srv_0123456789abcdef", ProjectID: "proj_123", Signals: harness.AllSignals,
		ResourceAttributes: map[string]string{harness.AttrServiceName: "opencode", harness.AttrEnduserID: "dev@example.com", harness.AttrProjectID: "proj_123"}}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
