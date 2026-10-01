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

// ScratchCommit proves the installed hook chain works: a detached temporary
// worktree gets a seeded session manifest and one file, is committed through the
// real hooks, and the resulting message is checked for the trailer. The worktree
// and its unreferenced commit are removed afterwards; nothing touches the user's
// branch.
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
	// Seed the binding into the worktree so the post-commit hook attributes the scratch
	// commit to this project — the round-trip needs a routable terma.commit event. The
	// worktree is a checkout of HEAD, so a repository that commits .terma/settings.json
	// already has it; one that gitignores its own binding (like terma-cli) does not. This
	// build's hooks would find the main checkout's through the worktree link
	// (project.Resolve), but the hooks run whatever terma is on PATH, and a build from
	// before that would spool a commit event with no project id, dropped as unroutable.
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
	// A new worktree has its own config.worktree and checks out HEAD. Immediately
	// after install, neither the per-worktree core.hooksPath nor the uncommitted
	// shim files exist there. Point the scratch commit at the hooks this checkout
	// actually runs, including files the developer has yet to commit.
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
	// scratchGitTimeout bounds the scratch worktree's checkout, commit and removal.
	// They ran under gitx.Timeout, a hook's 2-second budget: a checkout of 9,270
	// files and 2.7 GB takes 11 s, so doctor failed that repository on every run
	// with "git worktree: signal: killed". The commit runs the repository's own
	// hooks too, which are not terma's to budget.
	scratchGitTimeout = 2 * time.Minute
	// scratchDirPrefix and scratchWorktreeName name the scratch worktree
	// (<tmp>/terma-doctor-*/wt), so a later run can recognise one an earlier run
	// could not clean up.
	scratchDirPrefix    = "terma-doctor-"
	scratchWorktreeName = "wt"
)

// removeScratchWorktree unregisters a scratch worktree. --force twice, because an
// add killed mid-checkout leaves its registration locked ("initializing"), and
// `git worktree prune` passes over a locked one for ever. It runs even when doctor
// is interrupted, since that is when a half-made worktree is most likely.
func removeScratchWorktree(ctx context.Context, root, wt string) {
	_, _ = gitx.GitWithin(context.WithoutCancel(ctx), scratchGitTimeout, root, "worktree", "remove", "--force", "--force", wt)
}

// clearStaleScratchWorktrees removes what earlier runs could not: registrations of
// a scratch worktree whose directory is gone. Before scratchGitTimeout, every run
// against a large repository left one behind, locked. Only doctor's own naming is
// touched, and only once the directory no longer exists.
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
