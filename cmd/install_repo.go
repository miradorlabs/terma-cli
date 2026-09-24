package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/session"
)

func chooseProject(cmd *cobra.Command, cfg *config.Config) (*project, error) {
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	projects, err := availableProjects(cmd.Context(), client)
	if errors.Is(err, errNoProjects) {
		return nil, fmt.Errorf("%w, then run `terma install` again", err)
	}
	if err != nil {
		return nil, err
	}
	return soleOrPick(cmd, projects)
}

// wireRepo does the per-clone half of an install: point git at the committed shims
// when the repo uses them, remembering the previous hooksPath for uninstall.
func wireRepo(ctx context.Context, out io.Writer, root string, bound *termaproject.File) error {
	if bound.Install.HookManager != string(hookmgr.GitShim) {
		return nil
	}
	_, gitDir, err := gitx.Locate(ctx, root)
	if err != nil {
		return err
	}
	// Older installs wrote shared --local config. Restore that owned setting
	// before recording the new worktree override, or we'd remember our own shim
	// and lose the user's original hook path. Never migrate shared config from
	// a linked checkout: the owning main checkout must do that first.
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

// unwireRepo restores core.hooksPath to what it was before terma set it.
func unwireRepo(ctx context.Context, root, gitDir string) error {
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

// Linked worktrees share --local config. Use Git's per-worktree scope so changing
// one checkout never redirects or disables hooks in another checkout. Enable it
// for the first install too, before another worktree might be added.
func hooksConfigScope(ctx context.Context, root, gitDir string) (string, error) {
	common := gitx.CommonDirFS(gitDir)

	if gitx.ConfigGet(ctx, root, "extensions.worktreeConfig") != "true" {
		// Git requires these main-worktree-only settings to move out of the
		// shared config when enabling worktreeConfig (notably separate git dirs).
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
