package harness

import (
	"os"
	"strings"
	"testing"
)

// A wrap an earlier version installed is brought up to this build's command, falling
// back to the renderer it recorded, and every other option survives.
func TestRefreshStatusLineUpgradesAnOlderWrap(t *testing.T) {
	c, path := claudeIn(t, `{"statusLine": {"type": "command", "command": "my-renderer --fancy", "padding": 2}}`)
	if _, err := c.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	// What an earlier build wrote: the bare invocation, without the fallback guard.
	s, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	s.root[claudeStatusLineKey] = []byte(`{"type":"command","command":"exec terma hook statusline","padding":2}`)
	if err := s.save(false); err != nil {
		t.Fatal(err)
	}

	got, changed, err := c.RefreshStatusLine()
	if err != nil || !changed || got != path {
		t.Fatalf("refresh: path=%q changed=%v err=%v", got, changed, err)
	}
	sl := statusLineOf(t, path)
	if sl["command"] != StatusLineCommand("my-renderer --fancy") || sl["padding"] != 2.0 {
		t.Fatalf("refreshed entry %v", sl)
	}
	if r, err := StatusLineRenderer(); err != nil || r != "my-renderer --fancy" {
		t.Fatalf("renderer %q err %v", r, err)
	}
	if _, changed, err := c.RefreshStatusLine(); err != nil || changed {
		t.Fatalf("second refresh: changed=%v err=%v", changed, err)
	}
	// The record still restores what the developer had.
	if _, err := c.RemoveStatusLine(); err != nil {
		t.Fatal(err)
	}
	if sl := statusLineOf(t, path); sl["command"] != "my-renderer --fancy" {
		t.Fatalf("restored %v", sl)
	}
}

// A refresh never installs: no status line, the developer's own, or a copied terma
// command with no record of what it replaced all stay exactly as they are.
func TestRefreshStatusLineNeverInstalls(t *testing.T) {
	for name, settings := range map[string]string{
		"absent":    `{"model": "opus"}`,
		"their own": `{"statusLine": {"type": "command", "command": "my-renderer"}}`,
		"no record": `{"statusLine": {"type": "command", "command": "terma hook statusline"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, path := claudeIn(t, settings)
			before, _ := os.ReadFile(path)
			if _, changed, err := c.RefreshStatusLine(); err != nil || changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Fatalf("file changed:\n%s", after)
			}
		})
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
