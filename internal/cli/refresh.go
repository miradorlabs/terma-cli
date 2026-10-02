package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/miradorlabs/terma-cli/internal/refresh"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/relay/service"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

func (app *App) refresher() refresh.Refresher {
	return refresh.Refresher{Agents: app.agents, Version: app.version, RelayService: refreshRelayService}
}

// refreshRelayService rewrites a relay service an earlier terma (or another binary) wrote,
// in the environment install recorded, never the caller's: an update can run from any shell.
func refreshRelayService(ctx context.Context) (string, bool, error) {
	if !realTerma() || !service.Supported() {
		return "", false, nil
	}
	return daemon.RefreshService(ctx)
}

// workspaceRoot is the workspace around the working directory, "" outside one.
func workspaceRoot(ctx context.Context) (root, gitDir string) {
	root, gitDir, err := workspaceHere(ctx)
	if err != nil {
		return "", ""
	}
	return root, gitDir
}

// runRefresh is `terma update --refresh`.
func (app *App) runRefresh(ctx context.Context, out io.Writer) error {
	out = style.Highlight(out)
	root, gitDir := workspaceRoot(ctx)
	res, err := app.refresher().Run(ctx, root, gitDir)
	for _, name := range res.Migrated {
		fmt.Fprintf(out, "Migrated saved state: %s.\n", name)
	}
	switch {
	case len(res.Machine) == 0 && len(res.RepoChanged) == 0:
		fmt.Fprintf(out, "Nothing to refresh: what terma installed already matches %s.\n", app.version)
	default:
		fmt.Fprintf(out, "Refreshed for terma %s:\n", app.version)
		for _, p := range append(res.Machine, res.RepoChanged...) {
			fmt.Fprintf(out, "  updated %s\n", p)
		}
	}
	if res.Repo != nil {
		for _, n := range res.Repo.Notes {
			fmt.Fprintf(out, "\n%s\n", n)
		}
	}
	if len(res.RepoChanged) > 0 {
		fmt.Fprintln(out)
		printCommitList(out, "These are committed files. Commit them so every clone runs the same hooks:", res.RepoChanged)
		for _, note := range res.Retrust {
			fmt.Fprintf(out, "\n%s\n", note)
		}
	}
	if res.Repo == nil {
		fmt.Fprintln(out, "\nRun `terma update` inside each repository terma is installed in to update its committed hooks too.")
	}
	return err
}

// refreshAfterUpgrade says what the first command under a newer release refreshed.
func (app *App) refreshAfterUpgrade(ctx context.Context, dir string, out io.Writer) {
	out = style.Highlight(out)
	root, gitDir := workspaceRoot(ctx)
	up, err := app.refresher().AfterUpgrade(ctx, dir, root, gitDir)
	if !up.Due {
		return
	}
	if len(up.Changed) > 0 {
		fmt.Fprintf(out, "terma %s refreshed %d file(s) an earlier version installed.\n", app.version, len(up.Changed))
	}
	if err != nil {
		fmt.Fprintf(out, "terma %s could not refresh what an earlier version installed (%v). Run `terma update` to retry.\n", app.version, err)
	}
	if up.RepoStale {
		fmt.Fprintln(out, "This repository's hooks were written by an earlier terma. Run `terma update` here to update them.")
	}
}
