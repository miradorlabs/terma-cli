package install

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Wire does the per-clone half of an install: point git at the committed shims
// when the repo uses them, remembering the previous hooksPath for uninstall.
func Wire(ctx context.Context, out io.Writer, root string, bound *termaproject.File) error {
	if bound.Install.HookManager != string(hookmgr.GitShim) {
		return nil
	}
	_, gitDir, err := gitx.Locate(ctx, root)
	if err != nil {
		return err
	}
	// Restore a shared --local setting first, or the new override would record our own
	// shim as the previous path; only the main checkout may migrate it.
	if session.PreviousHooksScope(gitDir) == "--local" {
		local, localErr := gitx.Git(ctx, root, "config", "--local", "--get", "core.hooksPath")
		if localErr == nil && local == hookmgr.ShimDir && filepath.Clean(gitx.CommonDirFS(gitDir)) != filepath.Clean(gitDir) {
			return fmt.Errorf("legacy shared Git hooks must be migrated from the main worktree with `terma install` first")
		}
		if previous, recorded := session.PreviousHooksPath(gitDir); recorded && localErr == nil && local == hookmgr.ShimDir {
			if session.HooksPathWasLocal(gitDir) {
				err = gitx.ConfigSet(ctx, root, "core.hooksPath", previous)
			} else {
				err = gitx.ConfigUnset(ctx, root, "core.hooksPath")
			}
			if err != nil {
				return err
			}
		}
	}
	scope, err := hooksConfigScope(ctx, root, gitDir)
	if err != nil {
		return err
	}
	current := gitx.ConfigGet(ctx, root, "core.hooksPath")
	if strings.ContainsAny(current, "\r\n") {
		return fmt.Errorf("cannot chain a core.hooksPath containing a newline")
	}
	scopedCurrent, localErr := gitx.Git(ctx, root, "config", scope, "--get", "core.hooksPath")
	if localErr == nil && scopedCurrent == hookmgr.ShimDir {
		return nil
	}
	chainPath := current
	if current != "" {
		chainPath, err = gitx.Git(ctx, root, "config", "--path", "--get", "core.hooksPath")
		if err != nil {
			return err
		}
	}
	if err := session.RecordPreviousHooksPathAtScope(gitDir, current, scope, localErr == nil, chainPath); err != nil {
		return err
	}
	if _, err := gitx.Git(ctx, root, "config", scope, "core.hooksPath", hookmgr.ShimDir); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nPointed git at the committed hook shims (core.hooksPath = %s).\n", hookmgr.ShimDir)
	if current != "" {
		fmt.Fprintf(out, "The shims chain your previous hooks (%s) when they exist; `terma uninstall` restores the setting.\n", current)
	}
	return nil
}

// Unwire restores core.hooksPath to what it was before terma set it.
func Unwire(ctx context.Context, root, gitDir string) error {
	if gitx.ConfigGet(ctx, root, "core.hooksPath") != hookmgr.ShimDir {
		return nil
	}
	previous, ok := session.PreviousHooksPath(gitDir)
	if !ok {
		return nil
	}
	scope := session.PreviousHooksScope(gitDir)
	var err error
	if session.HooksPathWasLocal(gitDir) {
		_, err = gitx.Git(ctx, root, "config", scope, "core.hooksPath", previous)
		return err
	}
	_, err = gitx.Git(ctx, root, "config", scope, "--unset", "core.hooksPath")
	return err
}

// hooksConfigScope enables Git's per-worktree scope, since linked worktrees share
// --local config and one checkout must never redirect another's hooks.
func hooksConfigScope(ctx context.Context, root, gitDir string) (string, error) {
	common := gitx.CommonDirFS(gitDir)

	if gitx.ConfigGet(ctx, root, "extensions.worktreeConfig") != "true" {
		// Git requires these main-worktree-only settings out of the shared config first.
		for _, key := range []string{"core.worktree", "core.bare"} {
			value, err := gitx.Git(ctx, root, "config", "--local", "--get", key)
			if err != nil || (key == "core.bare" && strings.ToLower(value) != "true") {
				continue
			}
			if _, err := gitx.Git(ctx, root, "config", "--file", filepath.Join(common, "config.worktree"), key, value); err != nil {
				return "", err
			}
			if err := gitx.ConfigUnset(ctx, root, key); err != nil {
				return "", err
			}
		}
		if err := gitx.ConfigSet(ctx, root, "extensions.worktreeConfig", "true"); err != nil {
			return "", err
		}
	}
	return "--worktree", nil
}
