package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/doctor"
)

func TestAgentHooksCheckRejectsMalformedSettings(t *testing.T) {
	root := t.TempDir()
	wireAdapters(t, root, "claude")
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"), []byte(`{"hooks":`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := doctor.AgentHooksCheck(testApp.agents, root, []string{"claude"})
	if c.Status != doctor.Warn || c.Ready != 0 || c.Of != 1 {
		t.Fatalf("malformed settings: %+v, want warning and 0/1 ready", c)
	}
	if !strings.Contains(c.Detail, "could not be read") || strings.Contains(c.Detail, "hooks present") || !strings.Contains(c.Fix, ".claude/settings.json") {
		t.Fatalf("missing actionable parse error: %+v", c)
	}
}

// An agent is missing only when this developer uses it and the repository does not wire
// it, never an agent nobody named.
func TestAgentHooksCheckReadsTheRepository(t *testing.T) {
	root := t.TempDir()
	if c := doctor.AgentHooksCheck(testApp.agents, root, nil); c.Status != doctor.Skip {
		t.Fatalf("nothing wired, no agents named: status = %v (%s), want skip", c.Status, c.Detail)
	}

	wireAdapters(t, root, "claude")
	c := doctor.AgentHooksCheck(testApp.agents, root, nil)
	if c.Status != doctor.Pass || c.Detail != "Claude Code hooks present" {
		t.Fatalf("claude wired, no agents named: %v %q", c.Status, c.Detail)
	}

	// A developer who uses one agent in a repository wired only for another.
	c = doctor.AgentHooksCheck(testApp.agents, root, []string{"cursor"})
	if c.Status != doctor.Warn || !strings.Contains(c.Detail, "Cursor hooks missing") || c.Fix != "terma install" {
		t.Fatalf("cursor named but not wired: %v %q fix %q", c.Status, c.Detail, c.Fix)
	}
	if !strings.Contains(c.Detail, "Claude Code hooks present") {
		t.Fatalf("a colleague's wired agent should still be reported: %q", c.Detail)
	}
	if c.Ready != 0 || c.Of != 1 {
		t.Fatalf("readiness counts only the developer's own agents: %d/%d, want 0/1", c.Ready, c.Of)
	}
}

// Hooks an earlier terma wrote are out of date, and the fix is the refresh, not a re-install.
func TestAgentHooksCheckSendsStaleHooksToRefresh(t *testing.T) {
	root := t.TempDir()
	wireAdapters(t, root, "claude")
	path := filepath.Join(root, ".claude", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	delete(settings["hooks"].(map[string]any), "Stop")
	if data, err = json.Marshal(settings); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	c := doctor.AgentHooksCheck(testApp.agents, root, []string{"claude"})
	if c.Status != doctor.Warn || !strings.Contains(c.Detail, "Claude Code hooks out of date") || c.Fix != "terma update --refresh" {
		t.Fatalf("stale hooks: %v %q fix %q", c.Status, c.Detail, c.Fix)
	}
}
