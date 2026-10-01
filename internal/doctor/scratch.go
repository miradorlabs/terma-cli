package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

// ScratchCommit proves the hook chain works by committing a seeded session's file in a
// temporary detached worktree and checking the message for the trailer.
func ScratchCommit(ctx context.Context, root string, bound *termaproject.File) (string, Check) {
	if gitx.HeadSHA(ctx, root) == "" {
		return "", Check{Status: Skip, Detail: "repository has no commits yet"}
	}
	clearStaleScratchWorktrees(ctx, root)
	tmp, err := os.MkdirTemp("", scratchDirPrefix+"*")
	if err != nil {
		return "", Check{Status: Fail, Detail: err.Error()}
	}
	wt := filepath.Join(tmp, scratchWorktreeName)
	cleanup := func() {
		removeScratchWorktree(ctx, root, wt)
		_ = os.RemoveAll(tmp)
		_, _ = gitx.Git(context.WithoutCancel(ctx), root, "worktree", "prune")
	}
	if _, err := gitx.GitWithin(ctx, scratchGitTimeout, root, "worktree", "add", "--detach", "-q", wt, "HEAD"); err != nil {
		cleanup()
		return "", Check{Status: Fail, Detail: "could not create a temporary worktree: " + err.Error(), Fix: "`terma doctor --skip-commit` runs every other check"}
	}
	defer cleanup()
	// Seed the binding, which may be gitignored, since the hooks run whatever terma is on
	// PATH and an older build cannot find the main checkout's.
	if bound != nil {
		_ = termaproject.Save(wt, bound)
	}

	_, wtGitDir, err := gitx.Locate(ctx, wt)
	if err != nil {
		return "", Check{Status: Fail, Detail: err.Error()}
	}
	sessionID := fmt.Sprintf("doctor-%d", time.Now().UnixNano())
	file := ".terma-doctor"
	if err := os.WriteFile(filepath.Join(wt, file), []byte("terma doctor scratch file\n"), 0o644); err != nil {
		return "", Check{Status: Fail, Detail: err.Error()}
	}
	store := session.Open(wtGitDir)
	if err := store.Touch(session.Session{ID: sessionID, Tool: "terma-doctor"}, []string{file}, time.Now()); err != nil {
		return "", Check{Status: Fail, Detail: "could not seed a session manifest: " + err.Error()}
	}
	if _, err := gitx.Git(ctx, wt, "add", file); err != nil {
		return "", Check{Status: Fail, Detail: err.Error()}
	}
	commitArgs := []string{"-c", "commit.gpgsign=false"}
	// Right after install the new worktree has neither the per-worktree core.hooksPath nor
	// the uncommitted shims, so point it at the hooks this checkout runs.
	if gitx.ConfigGet(ctx, root, "core.hooksPath") != "" {
		hooksPath, err := gitx.Git(ctx, root, "config", "--path", "--get", "core.hooksPath")
		if err != nil {
			return "", Check{Status: Fail, Detail: "could not resolve core.hooksPath: " + err.Error()}
		}
		if !filepath.IsAbs(hooksPath) {
			hooksPath = filepath.Join(root, hooksPath)
		}
		commitArgs = append(commitArgs, "-c", "core.hooksPath="+hooksPath)
	}
	commitArgs = append(commitArgs, "commit", "-q", "-m", "terma doctor scratch commit")
	if _, err := gitx.GitWithin(ctx, scratchGitTimeout, wt, commitArgs...); err != nil {
		return "", Check{Status: Fail, Detail: "scratch commit failed: " + err.Error(), Fix: "a hook is failing the commit — run it with TERMA_DEBUG=1 to see why"}
	}
	sha := gitx.HeadSHA(ctx, wt)
	message, err := gitx.CommitMessage(ctx, wt, "HEAD")
	if err != nil {
		return sha, Check{Status: Fail, Detail: err.Error()}
	}
	for _, t := range trailer.Parse(message, gitx.CommentChar(ctx, wt)) {
		if t.SessionID == sessionID {
			return sha, Check{Status: Pass, Detail: "prepare-commit-msg stamped Agent-Session-Id on " + sha[:7]}
		}
	}
	return sha, Check{Status: Fail, Detail: "the commit went through but carried no Agent-Session-Id trailer", Fix: "the hook did not run: re-run `terma install`, then the hook manager's install step (see its notes)"}
}

const (
	// scratchGitTimeout is not a hook's 2-second gitx.Timeout: a 2.7 GB checkout takes 11 s.
	scratchGitTimeout = 2 * time.Minute
	// scratchDirPrefix and scratchWorktreeName let a later run recognise an abandoned one.
	scratchDirPrefix    = "terma-doctor-"
	scratchWorktreeName = "wt"
)

// removeScratchWorktree forces twice: a killed add leaves its registration locked, which
// prune skips; it runs even when doctor is interrupted.
func removeScratchWorktree(ctx context.Context, root, wt string) {
	_, _ = gitx.GitWithin(context.WithoutCancel(ctx), scratchGitTimeout, root, "worktree", "remove", "--force", "--force", wt)
}

// clearStaleScratchWorktrees removes registrations of doctor's own scratch worktrees
// whose directory is gone.
func clearStaleScratchWorktrees(ctx context.Context, root string) {
	out, err := gitx.Git(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(out, "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok || filepath.Base(path) != scratchWorktreeName || !strings.HasPrefix(filepath.Base(filepath.Dir(path)), scratchDirPrefix) {
			continue
		}
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			removeScratchWorktree(ctx, root, path)
		}
	}
}
