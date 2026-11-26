package history

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// uploadWithReach uploads one session in cwd whose second turn follows a reach of paths, and
// returns what teamID received.
func uploadWithReach(t *testing.T, cwd, teamID string, personal bool, paths []string) string {
	t.Helper()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-4 * time.Hour)
	before := makeNativeSegment("claude", "s-reach", cwd, "BEFORE_REACH_OK", t0)
	after := makeNativeSegment("claude", "s-reach", cwd, "AFTER_REACH", t0.Add(2*time.Minute))
	after.ToolReaches = []agents.HistoryToolReach{{At: t0.Add(time.Minute), Cwd: cwd, Paths: paths}}
	src := &stubHistorySource{
		source:   "claude",
		files:    []agents.HistoryFile{{SourceSystem: "claude", SessionID: "s-reach", ModTime: now.Add(-3 * time.Hour)}},
		segments: map[string][]agents.HistorySegment{"s-reach": {before, after}},
	}
	srv, got := captureServer(t)
	if _, err := Upload(context.Background(), uploadOpts(t.TempDir(), twoOrgState(now, personal), srv, now, src)); err != nil {
		t.Fatal(err)
	}
	sent := got()[teamID]
	if !strings.Contains(sent, "BEFORE_REACH_OK") {
		t.Fatalf("%s received %q; want the turn before the reach", teamID, sent)
	}
	return sent
}

// A file in a stale linked worktree, whose main repository moved or was deleted, may be an
// organization's: a Personal session that reads it is cut.
func TestReachIntoStaleLinkedWorktreeIsUnknown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd, wt := filepath.Join(home, "notes"), filepath.Join(home, "worktrees", "app-1")
	for _, dir := range []string{cwd, wt} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gone := filepath.Join(home, "gone", "app", ".git", "worktrees", "app-1")
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gone+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(wt, "main.go"), filepath.Join(cwd, "todo.txt")} {
		if err := os.WriteFile(f, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if sent := uploadWithReach(t, cwd, "team-p", true, hookrun.Alone(filepath.Join(wt, "main.go"))); strings.Contains(sent, "AFTER_REACH") {
		t.Errorf("a read in a stale linked worktree did not cut the Personal session")
	}
	if sent := uploadWithReach(t, cwd, "team-p", true, hookrun.Alone(filepath.Join(cwd, "todo.txt"))); !strings.Contains(sent, "AFTER_REACH") {
		t.Errorf("a read in the session's own folder cut it")
	}
}

// A missing path that LocateFS places in the session's own checkout by name, but that passes
// through a symlink to somewhere since deleted, is not known to be the checkout's.
func TestReachThroughDanglingSymlinkInCheckoutIsUnknown(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	repoA := initTestGitRepo(t, "https://github.com/org-a/app.git")
	if err := os.Symlink(filepath.Join(t.TempDir(), "org-b-deleted"), filepath.Join(repoA, "partner")); err != nil {
		t.Fatal(err)
	}
	if sent := uploadWithReach(t, repoA, "team-a", false, hookrun.Alone(filepath.Join(repoA, "partner", "secret.go"))); strings.Contains(sent, "AFTER_REACH") {
		t.Errorf("a read through a dangling symlink did not cut the session")
	}
	if sent := uploadWithReach(t, repoA, "team-a", false, hookrun.Alone(filepath.Join(repoA, "deleted.go"))); !strings.Contains(sent, "AFTER_REACH") {
		t.Errorf("a missing file in the session's own checkout cut it")
	}
}
