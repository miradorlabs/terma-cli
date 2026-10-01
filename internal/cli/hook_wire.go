package cli

import (
	"context"
	"io"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/install"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// wireCloneOnFirstUse sets core.hooksPath from the first claiming hook, since nothing in
// a commit can set a clone's git config. Later hooks pay one stat for the restoration
// record; a husky, lefthook or pre-commit line needs nothing per clone.
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
	_ = install.Wire(ctx, io.Discard, root, bound)
}
