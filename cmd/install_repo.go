package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"

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
	current := gitx.ConfigGet(ctx, root, "core.hooksPath")
	if current == hookmgr.ShimDir {
		return nil
	}
	_, gitDir, err := gitx.Locate(ctx, root)
	if err != nil {
		return err
	}
	if err := session.RecordPreviousHooksPath(gitDir, current); err != nil {
		return err
	}
	if err := gitx.ConfigSet(ctx, root, "core.hooksPath", hookmgr.ShimDir); err != nil {
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
	if ok && previous != "" {
		return gitx.ConfigSet(ctx, root, "core.hooksPath", previous)
	}
	return gitx.ConfigUnset(ctx, root, "core.hooksPath")
}
