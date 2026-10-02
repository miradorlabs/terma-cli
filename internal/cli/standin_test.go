package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// Claude Code reads .claude/settings.json only where it started, so a session started in
// a bound repository's subdirectory runs none of the committed hooks. Outside global mode
// the machine-wide hooks then stand in for them, and only then.
func TestMachineWideHooksStandInForASubdirectorySession(t *testing.T) {
	root := hookruntest.InitRepo(t)
	sub := filepath.Join(root, "src")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	claude, ok := testApp.agents.Lookup("claude")
	if !ok {
		t.Fatal("no claude agent")
	}
	plan, err := claude.Plan(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(root, plan); err != nil {
		t.Fatal(err)
	}
	repo := config.DefaultPolicy()
	gm := testApp.globalMode()
	yields := func(cwd, launch string) bool {
		t.Setenv("CLAUDE_PROJECT_DIR", launch)
		return gm.Yields(true, repo, "claude-code", cwd)
	}
	if !yields(sub, sub) {
		t.Fatal("a machine-wide hook acted in a repository bound to no project")
	}
	if err := termaproject.Save(root, &termaproject.File{Project: termaproject.Project{ID: "proj-sub"}}); err != nil {
		t.Fatal(err)
	}
	if yields(sub, sub) {
		t.Fatal("a session started in a bound repository's subdirectory ran no hook at all")
	}
	if yields(sub, "") {
		t.Fatal("without CLAUDE_PROJECT_DIR the hook's own directory is where the session started")
	}
	// Started at the root, the committed hooks run: the machine-wide one must not fire too.
	if !yields(root, root) || !yields(sub, root) {
		t.Fatal("a machine-wide hook fired beside the repository's own")
	}
	// Global mode is unchanged: there the machine-wide hooks always act.
	if gm.Yields(true, config.Policy{Mode: config.ModeGlobal, DefaultProjectID: "p"}, "claude-code", sub) {
		t.Fatal("a machine-wide hook stood down in global mode")
	}
	// An agent that finds committed hooks from any subdirectory never needs a stand-in.
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if !gm.Yields(true, repo, "codex", sub) {
		t.Fatal("Codex's machine-wide hook acted where its repository hooks already run")
	}
}
