package cli

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// authorizeLabel names the action that authorizes the n organizations this machine has no
// sign-in for, at most auth.MaxCodes per approval.
func authorizeLabel(n int) string {
	switch {
	case n > auth.MaxCodes:
		return fmt.Sprintf("Authorize %d of %d more organizations", auth.MaxCodes, n)
	case n == 1:
		return "Authorize 1 more organization"
	}
	return fmt.Sprintf("Authorize %d more organizations", n)
}

// authorizeBatch is what one approval authorizes of orgs: the first auth.MaxCodes, in
// listing order. The action repeats until none is left.
func authorizeBatch(orgs []organization) []organization {
	return orgs[:min(len(orgs), auth.MaxCodes)]
}

// authorizeNames names an approval's organizations, for the action's detail.
func authorizeNames(orgs []organization) string {
	names := make([]string, 0, len(orgs))
	for _, o := range authorizeBatch(orgs) {
		names = append(names, cmp.Or(o.Name, o.ID))
	}
	return strings.Join(names, ", ")
}

// authorizeMore signs in to the first auth.MaxCodes of orgs in one approval, the page
// pre-checking exactly those (contract C7). The organization active before stays active.
func (app *App) authorizeMore(cmd *cobra.Command, cfg *config.Config, orgs []organization, noBrowser bool) error {
	batch := authorizeBatch(orgs)
	ids := make([]string, 0, len(batch))
	for _, o := range batch {
		ids = append(ids, o.ID)
	}
	_, err := app.signIn(cmd, cfg, signInOptions{orgs: ids, noBrowser: noBrowser, pauseBeforeBrowser: canPrompt()})
	return err
}

// unauthorizedOrgs are the organizations the developer belongs to that this machine has no
// sign-in for, in listing order.
func (app *App) unauthorizedOrgs(ctx context.Context, cfg *config.Config) ([]organization, error) {
	orgs, err := app.listOrganizations(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var out []organization
	for _, o := range orgs {
		if _, ok := app.orgSignIn(cfg, o.ID); !ok {
			out = append(out, o)
		}
	}
	return out, nil
}

// runAuthorize is `terma teams --authorize`: the checklist's "Authorize N more
// organizations" without the checklist, for the desktop and scripts. With every
// organization signed in already it changes nothing, and says so.
func (app *App) runAuthorize(cmd *cobra.Command, cfg *config.Config, noBrowser bool) error {
	orgs, err := app.unauthorizedOrgs(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	if len(orgs) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "Every organization you belong to already has a sign-in on this machine.")
		return nil
	}
	return app.authorizeMore(cmd, cfg, orgs, noBrowser)
}
