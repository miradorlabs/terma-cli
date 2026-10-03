package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TERMA_HOOKS=0 leaves a commit unstamped; any other value leaves the hooks on.
func TestHookKillSwitch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "dev@example.com")
	git("config", "user.name", "Dev")
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_HOOKS", "")
	admitHere(t, root, "team")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()

	run := func(stdin string, args ...string) {
		t.Helper()
		c := testApp.newHookCommand()
		c.SetArgs(args)
		c.SetIn(strings.NewReader(stdin))
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		if err := c.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("terma hook %s: %v", strings.Join(args, " "), err)
		}
	}
	// No manifest, so the active-session fallback attributes the staged file below.
	run(`{"session_id":"sess-kill-switch","cwd":"`+root+`","hook_event_name":"SessionStart","source":"startup"}`, "session-start")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")

	msg := filepath.Join(root, ".git", "COMMIT_EDITMSG")
	const original = "feat: x\n"
	stamp := func() string {
		t.Helper()
		if err := os.WriteFile(msg, []byte(original), 0o644); err != nil {
			t.Fatal(err)
		}
		run("", "prepare-commit-msg", msg, "message")
		got, err := os.ReadFile(msg)
		if err != nil {
			t.Fatal(err)
		}
		return string(got)
	}

	t.Setenv("TERMA_HOOKS", "0")
	if got := stamp(); got != original {
		t.Fatalf("stamped despite TERMA_HOOKS=0:\n%s", got)
	}
	t.Setenv("TERMA_HOOKS", "1")
	if got := stamp(); !strings.Contains(got, "Agent-Session-Id: sess-kill-switch") {
		t.Fatalf("not stamped with the switch off:\n%s", got)
	}
}
