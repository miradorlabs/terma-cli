package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/e2e"
)

// A desktop app's snapshot is its release's: its capture notes, how it launched its agent, the
// agent it runs and its bundle; the capability checks of the agent build it runs join it, and
// make no release of their own.
func TestDesktopAppSnapshot(t *testing.T) {
	dir := t.TempDir()
	p := claudeProfile("4.16336.0", "Be brief.", []string{"Bash"}, "", "")
	p.Harness = "claude-desktop"
	p.Notes = []string{"Ran whole on a hosted runner."}
	p.Probes = map[string]json.RawMessage{
		"launch":     json.RawMessage(`{"argv":["claude","--output-format","stream-json"],"env":["CLAUDE_CODE_ENTRYPOINT"],"env_values":{"CLAUDE_CODE_ENTRYPOINT":"claude-desktop-3p"}}`),
		"app-server": json.RawMessage(`{"ClientRequest":["thread/start"]}`),
	}
	p.Binary = json.RawMessage(`{"form":"executable","size_bytes":1,"sha256":"00","env_vars":["CLAUDE_CODE_ENTRYPOINT"],
		"app":{"name":"Claude.app","version":"4.16336.0"},"agent":{"harness":"claude","version":"2.1.317","source":"downloaded"}}`)
	n := night{now: time.Now(), profiles: []e2e.Profile{p}, labels: map[string]string{},
		history: []HistoryEntry{
			{Harness: "claude-desktop", Version: "2.1.317", Platform: "darwin/arm64", Capability: "relay.desktop", Result: "pass", LastRun: time.Now()},
			{Harness: "claude-desktop", Version: "2.1.318", Platform: "darwin/arm64", Capability: "relay.desktop", Result: "pass", LastRun: time.Now()},
		}}
	if got := n.builds(); !slices.Equal(got, []buildID{{"claude-desktop", "4.16336.0"}}) {
		t.Errorf("builds = %v: an agent build's checks make no release of the app", got)
	}
	if _, err := publish(dir, n, options{}); err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	if err := readJSON(filepath.Join(dir, "claude-desktop", "versions", "4.16336.0.json"), &s); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.Capture.Notes, "Ran whole on a hosted runner.") {
		t.Errorf("notes = %q", s.Capture.Notes)
	}
	launch, _ := s.CLI["launch"].(map[string]any)
	if v, _ := launch["env_values"].(map[string]any); v["CLAUDE_CODE_ENTRYPOINT"] != "claude-desktop-3p" {
		t.Errorf("cli.launch = %v", s.CLI["launch"])
	}
	if s.CLI["app_server"] == nil {
		t.Errorf("cli.app_server = %v", s.CLI)
	}
	if agent, _ := s.Binary["agent"].(map[string]any); agent["version"] != "2.1.317" {
		t.Errorf("binary.agent = %v", s.Binary["agent"])
	}
	if s.Config == nil || s.Config["env_vars"] == nil {
		t.Errorf("config = %v", s.Config)
	}
	if s.Support == nil || len(s.Support.Capabilities) != 1 {
		t.Errorf("support = %v: the checks of the agent build the release runs, alone", s.Support)
	}
}
