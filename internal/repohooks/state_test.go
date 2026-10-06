package repohooks

import (
	"path/filepath"
	"strings"
	"testing"
)

// A relative core.hooksPath is taken from the root of the working tree git runs the hooks
// in: for a linked worktree, its own checkout, not the main one.
func TestHooksPathScopeResolvesRelativePathsFromTheWorktree(t *testing.T) {
	root, gitDir := scratch(t)
	git(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(filepath.Dir(root), "wt-"+filepath.Base(root))
	git(t, root, "worktree", "add", "-q", wt)
	git(t, root, "config", "extensions.worktreeConfig", "true")
	wtGitDir := git(t, wt, "rev-parse", "--absolute-git-dir")
	if wtGitDir == gitDir {
		t.Fatal("fixture: not a linked worktree")
	}
	if got := HooksPathScope(wtGitDir); got != "" {
		t.Fatalf("no hooks path: scope %q", got)
	}
	// ".git/hooks" from the linked worktree is under its .git file: git reads no hooks there.
	git(t, wt, "config", "--worktree", "core.hooksPath", ".git/hooks")
	if got := HooksPathScope(wtGitDir); got != "worktree" {
		t.Fatalf(".git/hooks from a linked worktree: scope %q, want worktree", got)
	}
	// A relative path from the linked worktree that does reach the shared hooks directory.
	rel, err := filepath.Rel(wt, filepath.Join(gitDir, hooksDir))
	if err != nil || !strings.HasPrefix(rel, "..") {
		t.Fatalf("fixture: %q, %v", rel, err)
	}
	git(t, wt, "config", "--worktree", "core.hooksPath", rel)
	if got := HooksPathScope(wtGitDir); got != "" {
		t.Fatalf("%s reaches the shared hooks: scope %q, want none", rel, got)
	}
	// The main checkout's own ".git/hooks" is the shared directory.
	git(t, root, "config", "core.hooksPath", ".git/hooks")
	if got := HooksPathScope(gitDir); got != "" {
		t.Fatalf(".git/hooks in the main checkout: scope %q", got)
	}
	git(t, root, "config", "core.hooksPath", filepath.Join(gitDir, hooksDir)+"/")
	if got := HooksPathScope(gitDir); got != "" {
		t.Fatalf("the absolute hooks directory with a trailing slash: scope %q", got)
	}
}
