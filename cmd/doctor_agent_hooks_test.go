package cmd

import (
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
	c := agentHooksCheck(root, []string{"claude"})
	if c.Status != doctor.Warn || c.Ready != 0 || c.Of != 1 {
		t.Fatalf("malformed settings: %+v, want warning and 0/1 ready", c)
	}
	if !strings.Contains(c.Detail, "could not be read") || strings.Contains(c.Detail, "hooks present") || !strings.Contains(c.Fix, ".claude/settings.json") {
		t.Fatalf("missing actionable parse error: %+v", c)
	}
}

// The agent-hooks check reads what is wired from the repository's hooks files, and
// calls an agent missing only when this developer uses it and the repository does not
// wire it — never an agent nobody named.
func TestAgentHooksCheckReadsTheRepository(t *testing.T) {
	root := t.TempDir()
	if c := agentHooksCheck(root, nil); c.Status != doctor.Skip {
		t.Fatalf("nothing wired, no agents named: status = %v (%s), want skip", c.Status, c.Detail)
	}

	wireAdapters(t, root, "claude")
	c := agentHooksCheck(root, nil)
	if c.Status != doctor.Pass || c.Detail != "Claude Code hooks present" {
		t.Fatalf("claude wired, no agents named: %v %q", c.Status, c.Detail)
	}

	// A developer who uses Cursor in a repository wired only for Claude Code.
	c = agentHooksCheck(root, []string{"cursor"})
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
