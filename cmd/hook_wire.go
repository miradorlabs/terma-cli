package cmd

import (
	"context"
	"io"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// wireCloneOnFirstUse does the per-clone half of `terma install` from the first agent
// hook that claims a session in the clone: git is pointed at the committed commit-hook
// shims (core.hooksPath, wireRepo), so commits made there carry their session's
// trailers. A repository the platform connected commits its binding and hooks; nothing
// in a commit can set a clone's git config, and without this its commits were never
// stamped unless someone ran `terma install`.
//
// It costs one stat on every later hook: a clone wired before (by this or by install)
// has the hook-restoration record under its git directory, and is left alone — also
// when its developer pointed core.hooksPath somewhere else since. Only a binding whose
// commit hooks are terma's own shims is wired: a husky, lefthook or pre-commit line is
// committed and needs nothing per clone.
func wireCloneOnFirstUse(ctx context.Context, cwd string) {
	root, gitDir, ok := gitx.LocateFS(cwd)
	if !ok {
		return
	}
	if _, recorded := session.PreviousHooksPath(gitDir); recorded {
		return
	}
	bound, _, err := termaproject.Resolve(root, gitDir)
	if err != nil || bound == nil || bound.Install.HookManager != string(hookmgr.GitShim) {
		return
	}
	_ = wireRepo(ctx, io.Discard, root, bound)
}
