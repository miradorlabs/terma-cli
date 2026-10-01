package claude

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// helperExporter is fullExporter delivered through the headers helper.
func helperExporter(t *testing.T) harness.Exporter {
	t.Helper()
	e := fullExporter()
	path, err := harness.HelperFilePath(exporter{}, "proj_123")
	if err != nil {
		t.Fatalf("HelperFilePath: %v", err)
	}
	e.HelperPath = path
	return e
}

// After a helper-mode connect the settings file holds a path, and the key lives in a 0700 script.
func TestHelperConnectKeepsKeyOutOfSettings(t *testing.T) {
	c, path := claudeIn(t, `{"model":"opus"}`)
	e := helperExporter(t)

	if err := c.Connect(e, false); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if strings.Contains(string(raw), e.APIKey) {
		t.Fatal("the key reached the settings file; the helper exists to prevent exactly that")
	}
	if _, ok := envOf(t, path)["OTEL_EXPORTER_OTLP_HEADERS"]; ok {
		t.Error("an inline Authorization header was written alongside the helper")
	}
	if got := readJSON(t, path)["otelHeadersHelper"]; got != e.HelperPath {
		t.Errorf("otelHeadersHelper = %v, want %q", got, e.HelperPath)
	}

	data, err := os.ReadFile(e.HelperPath)
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	if !strings.Contains(string(data), e.APIKey) {
		t.Fatalf("the helper script does not carry the key:\n%s", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(e.HelperPath)
		if err != nil {
			t.Fatalf("stat helper: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o700 {
			t.Fatalf("helper mode = %#o, want 0700 — it holds a credential and must be executable only by the user", mode)
		}
	}
}

// A helper-mode connect keeps the user's own settings mode, a dotfiles 0644 included.
func TestHelperConnectPreservesSettingsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	c, path := claudeIn(t, `{"model":"opus"}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if err := c.Connect(helperExporter(t), false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o644 {
		t.Fatalf("settings mode = %#o, want the user's 0644 kept — the file holds no secret", mode)
	}
}

// Terma's own helper is not a conflict, or every reconnect would fight the previous install.
func TestOwnHelperIsNotAConflict(t *testing.T) {
	c, _ := claudeIn(t, "")
	t.Chdir(t.TempDir())
	e := helperExporter(t)

	if err := c.Connect(e, false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	conflicts, err := c.ConflictsWith(e)
	if err != nil {
		t.Fatalf("ConflictsWith: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("got %+v, want none — the configured helper is Terma's own", conflicts)
	}

	// Status reads the key's prefix out of the helper.
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Connected {
		t.Fatal("helper-mode config did not report as connected")
	}
	if !strings.HasPrefix(st.KeyPrefix, "ter_srv_") {
		t.Errorf("key prefix = %q, want it read from the helper script", st.KeyPrefix)
	}
}

// Disconnect removes its helper setting and deletes the script, which holds a live key.
func TestDisconnectRemovesOwnHelper(t *testing.T) {
	c, path := claudeIn(t, `{"model":"opus"}`)
	e := helperExporter(t)

	if err := c.Connect(e, false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	if _, ok := readJSON(t, path)["otelHeadersHelper"]; ok {
		t.Error("otelHeadersHelper survived disconnect")
	}
	if _, err := os.Stat(e.HelperPath); !os.IsNotExist(err) {
		t.Fatalf("the helper script survived disconnect (stat err %v); it holds a live key", err)
	}
	if readJSON(t, path)["model"] != "opus" {
		t.Error("an unrelated setting was lost")
	}
}

// A helper the user repointed is their edit: left alone and reported.
func TestDisconnectSkipsRepointedHelper(t *testing.T) {
	c, path := claudeIn(t, "")
	e := helperExporter(t)
	if err := c.Connect(e, false); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	s, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	s.root["otelHeadersHelper"] = []byte(`"/usr/local/bin/my-own-helper.sh"`)
	if err := s.save(false); err != nil {
		t.Fatalf("save: %v", err)
	}

	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if got := readJSON(t, path)["otelHeadersHelper"]; got != "/usr/local/bin/my-own-helper.sh" {
		t.Fatalf("the user's repointed helper was touched: %v", got)
	}
	found := false
	for _, k := range result.Skipped {
		if k == "otelHeadersHelper" {
			found = true
		}
	}
	if !found {
		t.Errorf("skipped = %v, want the repointed helper reported", result.Skipped)
	}
}

// CurrentCredential returns the installed key only for the same endpoint and project.
func TestCurrentCredential(t *testing.T) {
	t.Run("helper mode", func(t *testing.T) {
		c, _ := claudeIn(t, "")
		e := helperExporter(t)
		if err := c.Connect(e, false); err != nil {
			t.Fatalf("Connect: %v", err)
		}

		key, ok := c.CurrentCredential(e.Endpoint, "proj_123")
		if !ok || key != e.APIKey {
			t.Fatalf("CurrentCredential = (%q, %v), want the installed key", key, ok)
		}
		if _, ok := c.CurrentCredential(e.Endpoint, "some-other-project"); ok {
			t.Fatal("a key was offered for reuse across projects")
		}
		if _, ok := c.CurrentCredential("https://otel-dev.terma.ai", "proj_123"); ok {
			t.Fatal("a key was offered for reuse across endpoints — that is a disclosure, not a reuse")
		}
	})

	t.Run("inline mode", func(t *testing.T) {
		c, _ := claudeIn(t, "")
		e := fullExporter()
		if err := c.Connect(e, false); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		key, ok := c.CurrentCredential(e.Endpoint, "proj_123")
		if !ok || key != e.APIKey {
			t.Fatalf("CurrentCredential = (%q, %v), want the inline key", key, ok)
		}
	})
}

// A connect sweeps journals for configs that no longer exist and keeps the live one.
func TestConnectPrunesStaleJournals(t *testing.T) {
	termaHome := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", termaHome)
	c := exporter{}

	sandbox, err := os.MkdirTemp("", "prune-sandbox-*")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", sandbox)
	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("connect sandbox: %v", err)
	}
	if err := os.RemoveAll(sandbox); err != nil {
		t.Fatalf("remove sandbox: %v", err)
	}

	live := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", live)
	if err := c.Connect(fullExporter(), false); err != nil {
		t.Fatalf("connect live: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(termaHome, "telemetry"))
	if err != nil {
		t.Fatalf("read journal dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("journal dir holds %v, want exactly the live config's record", names)
	}
}

// A reconnect must not record terma's own helper path as the previous value, or disconnect would
// restore a path to the script it deleted.
func TestHelperSurvivesReconnectThenDisconnect(t *testing.T) {
	c, path := claudeIn(t, `{"model":"opus"}`)
	e := helperExporter(t)
	if err := c.Connect(e, false); err != nil {
		t.Fatalf("connect: %v", err)
	}
	e.IncludePrompts = !e.IncludePrompts
	if err := c.Connect(e, false); err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	result, err := c.Disconnect()
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if got := readJSON(t, path); got["otelHeadersHelper"] != nil {
		t.Fatalf("otelHeadersHelper = %v after disconnect, want it removed — it points at a deleted script", got["otelHeadersHelper"])
	}
	if _, err := os.Stat(e.HelperPath); !os.IsNotExist(err) {
		t.Fatal("the helper script survived disconnect")
	}
	if len(result.Skipped) != 0 {
		t.Errorf("skipped = %v, want nothing", result.Skipped)
	}
	if readJSON(t, path)["model"] != "opus" {
		t.Error("an unrelated setting was lost")
	}
}

// A ter_srv_ key round-trips through the helper, or a connect would report as keyless.
func TestCurrentCredentialAcceptsTermaPrefix(t *testing.T) {
	c, _ := claudeIn(t, "")
	e := helperExporter(t)
	e.APIKey = "ter_srv_00112233445566778899aabbccddeeff"
	if err := c.Connect(e, false); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	key, ok := c.CurrentCredential(e.Endpoint, "proj_123")
	if !ok || key != e.APIKey {
		t.Fatalf("CurrentCredential = (%q, %v), want the ter_srv_ key", key, ok)
	}
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.HasPrefix(st.KeyPrefix, "ter_srv_") {
		t.Errorf("key prefix = %q, want it read from the helper script", st.KeyPrefix)
	}
}
