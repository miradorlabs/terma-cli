package claude

import (
	"os"
	"testing"
)

// A wrap from an earlier build is upgraded, keeping its recorded renderer and every option.
func TestRefreshStatusLineUpgradesAnOlderWrap(t *testing.T) {
	c, path := claudeIn(t, `{"statusLine": {"type": "command", "command": "my-renderer --fancy", "padding": 2}}`)
	if _, err := c.InstallStatusLine(); err != nil {
		t.Fatal(err)
	}
	// The bare invocation, without the fallback guard.
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
	if sl["command"] != statusLineCommand("my-renderer --fancy") || sl["padding"] != 2.0 {
		t.Fatalf("refreshed entry %v", sl)
	}
	if r, err := statusLineRenderer(); err != nil || r != "my-renderer --fancy" {
		t.Fatalf("renderer %q err %v", r, err)
	}
	if _, changed, err := c.RefreshStatusLine(); err != nil || changed {
		t.Fatalf("second refresh: changed=%v err=%v", changed, err)
	}
	if _, err := c.RemoveStatusLine(); err != nil {
		t.Fatal(err)
	}
	if sl := statusLineOf(t, path); sl["command"] != "my-renderer --fancy" {
		t.Fatalf("restored %v", sl)
	}
}

// A refresh never installs, and leaves an unrecorded copy of terma's command alone.
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
