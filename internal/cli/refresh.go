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
	return refresh.Refresher{StateDir: app.stateDir, Agents: app.agents, Version: app.version, RelayService: app.refreshRelayService}
}

// refreshRelayService rewrites a relay service an earlier terma (or another binary) wrote,
// in the environment install recorded, never the caller's: an update can run from any shell.
// A relay of an earlier release is then asked to make way, so the upgrade takes it over now
// rather than at the next agent hook.
func (app *App) refreshRelayService(ctx context.Context) (path string, changed bool, err error) {
	if realTerma() && service.Supported() {
		path, changed, err = daemon.RefreshService(ctx, app.stateDir)
	}
	daemon.Supersede(app.stateDir, app.version)
	return path, changed, err
}

// runRefresh is `terma update --refresh`.
func (app *App) runRefresh(ctx context.Context, out io.Writer) error {
	out = style.Highlight(out)
	changed, err := app.refresher().Run(ctx)
	if len(changed) == 0 {
		fmt.Fprintf(out, "Nothing to refresh: what terma installed already matches %s.\n", app.version)
	} else {
		fmt.Fprintf(out, "Refreshed for terma %s:\n", app.version)
		for _, p := range changed {
			fmt.Fprintf(out, "  updated %s\n", p)
		}
	}
	return err
}

// refreshAfterUpgrade says what the first command under a newer release refreshed.
func (app *App) refreshAfterUpgrade(ctx context.Context, out io.Writer) {
	out = style.Highlight(out)
	up, err := app.refresher().AfterUpgrade(ctx)
	if !up.Due {
		return
	}
	if len(up.Changed) > 0 {
		fmt.Fprintf(out, "terma %s refreshed %d file(s) an earlier version installed.\n", app.version, len(up.Changed))
	}
	if err != nil {
		fmt.Fprintf(out, "terma %s could not refresh what an earlier version installed (%v). Run `terma update` to retry.\n", app.version, err)
	}
}
