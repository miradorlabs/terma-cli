package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every doctor run whose checkout outlasted its deadline left a registration behind,
// locked "initializing" — `git worktree prune` passes over those for ever. The next
// run clears them, and only them: doctor's own naming, directory gone. A live scratch
// worktree and anyone else's stale one are not doctor's to touch.
func TestDoctorClearsOnlyItsOwnStaleScratchWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// Resolved, so paths compare with git's listing (macOS temp dirs are behind /private).
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "repo")
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "initial")

	// What a killed add leaves: a locked registration, its directory deleted.
	abandoned := func(dir string) string {
		wt := filepath.Join(base, dir, "wt")
		git("worktree", "add", "--detach", "-q", "--lock", "--reason", "initializing", wt, "HEAD")
		if err := os.RemoveAll(filepath.Dir(wt)); err != nil {
			t.Fatal(err)
		}
		return wt
	}
	stale := abandoned(scratchDirPrefix + "111")
	foreign := abandoned("someone-else")
	live := filepath.Join(base, scratchDirPrefix+"222", "wt")
	git("worktree", "add", "--detach", "-q", live, "HEAD")

	clearStaleScratchWorktrees(context.Background(), repo)

	list := git("worktree", "list", "--porcelain")
	has := func(wt string) bool { return strings.Contains(list, "worktree "+wt+"\n") }
	if has(stale) {
		t.Errorf("doctor's abandoned scratch worktree is still registered:\n%s", list)
	}
	if !has(foreign) {
		t.Errorf("a stale worktree doctor did not make was removed:\n%s", list)
	}
	if !has(live) {
		t.Errorf("a scratch worktree whose directory exists was removed:\n%s", list)
	}
}
