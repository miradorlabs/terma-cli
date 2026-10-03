package omp

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// ompIn sandboxes omp's agent directory (OMP_DIR) and terma's own.
func ompIn(t *testing.T) (exporter, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(ompConfigOverride, dir)
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(dir, "terma"))
	return exporter{}, filepath.Join(dir, "agent", "hooks", "pre", "terma.ts")
}

func ompExporter(t *testing.T, h exporter, helper bool) harness.Exporter {
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

// The embedded extension has exactly one config placeholder, or every connect installs
// an inert extension.
func TestOmpTemplateHasOneConfigLineAndOneExport(t *testing.T) {
	if n := strings.Count(ompExtensionSource, ompConfigPlaceholder); n != 1 {
		t.Fatalf("placeholder appears %d times, want 1", n)
	}
	exports := 0
	for line := range strings.SplitSeq(ompExtensionSource, "\n") {
		if strings.HasPrefix(line, "export ") {
			exports++
		}
	}
	// omp's hook loader treats every export as a hook factory; a stray helper export
	// would make the hook fail to load.
	if exports != 1 {
		t.Fatalf("extension has %d exports, want exactly 1 (the hook factory)", exports)
	}
	if _, ok := readOmpExtensionConfig([]byte(ompExtensionSource)); ok {
		t.Fatal("the raw template must read as unconfigured")
	}
}

func TestOmpConnectWritesExtensionAndHelper(t *testing.T) {
	h, path := ompIn(t)
	e := ompExporter(t, h, true)
	if err := h.Connect(e, false); err != nil {
		t.Fatalf("connect: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("extension not written: %v", err)
	}
	if strings.Contains(string(data), ompConfigPlaceholder) {
		t.Fatal("placeholder left in place")
	}
	if strings.Contains(string(data), e.APIKey) {
		t.Fatal("the key landed in the extension file; it belongs in the helper")
	}
	cfg, ok := readOmpExtensionConfig(data)
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
	// No secret in the file, so it is readable like any other hook.
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Errorf("extension mode = %o, want 0644", info.Mode().Perm())
	}
	if info, err := os.Stat(e.HelperPath); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("helper: %v, mode %v", err, info)
	}
	if harness.KeyFromHelper(e.HelperPath) != e.APIKey {
		t.Fatal("helper does not hold the key")
	}
	// The user's own config was never touched.
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(path)), "config.yml")); err == nil {
		t.Fatal("connect wrote config.yml")
	}
}

func TestOmpStatusRoundTrip(t *testing.T) {
	h, path := ompIn(t)
	st, err := h.Status()
	if err != nil || st.Exists || st.Connected || st.ManagedKeys != 0 || st.ConfigPath != path {
		t.Fatalf("empty status = %+v, %v", st, err)
	}

	e := ompExporter(t, h, true)
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

// A hook file without terma's config line exists but is not managed, and disconnect
// leaves it.
func TestOmpForeignExtensionIsNotOurs(t *testing.T) {
	h, path := ompIn(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	src := []byte("// my own hook\nexport default function (pi) {}\n")
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := h.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exists || st.Connected || st.ManagedKeys != 0 {
		t.Errorf("status = %+v", st)
	}
	result, err := h.Disconnect()
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 0 {
		t.Errorf("removed %d foreign files", result.Removed)
	}
	data, _ := os.ReadFile(path)
	if string(data) != string(src) {
		t.Error("foreign extension was modified")
	}
}

func TestOmpInlineKeyIsTightened(t *testing.T) {
	h, path := ompIn(t)
	e := ompExporter(t, h, false)
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

func TestOmpDisconnectRemovesExtensionAndHelper(t *testing.T) {
	h, path := ompIn(t)
	e := ompExporter(t, h, true)
	if err := h.Connect(e, false); err != nil {
		t.Fatal(err)
	}
	result, err := h.Disconnect()
	if err != nil || result.Removed != 1 {
		t.Fatalf("disconnect = %+v, %v", result, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("extension still present")
	}
	if _, err := os.Stat(e.HelperPath); err == nil {
		t.Error("helper still present — the credential must go with the extension")
	}
	result, err = h.Disconnect()
	if err != nil || result.Removed != 0 {
		t.Fatalf("second disconnect = %+v, %v", result, err)
	}
}

// A helper outside terma's helpers directory is the user's and is left alone.
func TestOmpDisconnectLeavesForeignHelper(t *testing.T) {
	h, path := ompIn(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "my-helper")
	if err := os.WriteFile(foreign, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := ompConfig{Version: 1, Endpoint: "https://otel.terma.ai", HeadersHelper: foreign, Signals: []string{"traces"}}
	src, err := renderOmpExtension(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("foreign helper removed: %v", err)
	}
}

// The repository-scope file carries the policy and no endpoint, key or identity.
func TestOmpLocalPolicyCarriesNoCredential(t *testing.T) {
	root := t.TempDir()
	local := exporter{root: root}
	e := ompExporter(t, local, true)
	if err := local.Connect(e, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ompLocalPolicy)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{e.APIKey, e.Endpoint, "proj_123", "dev@example.com", e.HelperPath} {
		if strings.Contains(string(data), secret) {
			t.Errorf("local policy carries %q", secret)
		}
	}
	st, err := local.Status()
	if err != nil || !st.Exists || !st.HasPolicy || st.Connected {
		t.Errorf("local status = %+v, %v", st, err)
	}
	if !reflect.DeepEqual(st.Signals, []harness.Signal{harness.SignalTraces, harness.SignalLogs, harness.SignalMetrics}) {
		t.Errorf("signals = %v", st.Signals)
	}
	if _, ok := local.CurrentCredential(e.Endpoint, "proj_123"); ok {
		t.Error("a repository scope holds no credential")
	}
	if _, err := local.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("local policy still present")
	}
}

// Per-repo connect writes a keyless, projectless extension plus a per-project helper.
func TestOmpConnectPerRepo(t *testing.T) {
	h, path := ompIn(t)
	e := ompExporter(t, h, false)
	if err := h.ConnectPerRepo(e); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, ok := readOmpExtensionConfig(data)
	if !ok {
		t.Fatal("config line not readable back")
	}
	if !cfg.PerRepo || cfg.HeadersHelper != "" || len(cfg.Headers) != 0 {
		t.Errorf("per-repo config = %+v", cfg)
	}
	if cfg.Endpoint != e.Endpoint || cfg.HelpersDir == "" || cfg.HelperPrefix != "omp-otel-" || cfg.ProjectAttribute != harness.AttrProjectID {
		t.Errorf("routing fields = %+v", cfg)
	}
	if _, present := cfg.ResourceAttributes[harness.AttrProjectID]; present {
		t.Error("a per-repo extension must not pin a project id")
	}
	// A direct export, past the relay, so no content.
	if cfg.IncludePrompts || cfg.IncludeToolContent {
		t.Errorf("per-repo extension must not carry content capture: %+v", cfg)
	}
	helper, err := harness.HelperFilePath(h, e.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if harness.KeyFromHelper(helper) != e.APIKey {
		t.Error("project helper does not hold the key")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Errorf("per-repo extension mode = %o, want 0644 (no secret in it)", info.Mode().Perm())
	}
}

func TestOmpConflicts(t *testing.T) {
	e := harness.Exporter{Endpoint: "https://otel.terma.ai"}

	// A shell export pointing elsewhere defeats the extension's set.
	t.Setenv(harness.EnvOTLPEndpoint, "https://other.example.com")
	conflicts, err := (exporter{}).ConflictsWith(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || !conflicts[0].Credential || conflicts[0].Clearable {
		t.Errorf("endpoint conflict = %+v", conflicts)
	}

	t.Setenv(harness.EnvOTLPEndpoint, e.Endpoint)
	if conflicts, _ := (exporter{}).ConflictsWith(e); len(conflicts) != 0 {
		t.Errorf("same endpoint should not conflict: %+v", conflicts)
	}

	// A protocol mismatch would silently disable the export.
	t.Setenv(harness.EnvOTLPEndpoint, "")
	t.Setenv(harness.EnvOTLPProtocol, "grpc")
	conflicts, _ = (exporter{}).ConflictsWith(e)
	if len(conflicts) != 1 || conflicts[0].Key != harness.EnvOTLPProtocol {
		t.Errorf("protocol conflict = %+v", conflicts)
	}

	// The upstream content switch exported in the shell is advisory, never blocking.
	t.Setenv(harness.EnvOTLPProtocol, "")
	t.Setenv(ompCaptureContentEnv, "false")
	conflicts, _ = (exporter{}).ConflictsWith(harness.Exporter{Endpoint: e.Endpoint})
	if len(conflicts) != 1 || !conflicts[0].Advisory {
		t.Errorf("content-capture conflict should be advisory: %+v", conflicts)
	}
	t.Setenv(ompCaptureContentEnv, "true")
	if conflicts, _ := (exporter{}).ConflictsWith(harness.Exporter{Endpoint: e.Endpoint}); len(conflicts) != 0 {
		t.Errorf("a matching content export should not conflict: %+v", conflicts)
	}
}

func baseExporter() harness.Exporter {
	return harness.Exporter{Endpoint: "https://otel.terma.ai", APIKey: "ter_srv_0123456789abcdef", ProjectID: "proj_123", Signals: harness.AllSignals,
		ResourceAttributes: map[string]string{harness.AttrServiceName: "omp", harness.AttrEnduserID: "dev@example.com", harness.AttrProjectID: "proj_123"}}
}
