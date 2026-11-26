package cli

import (
	"cmp"
	"context"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

type tick struct {
	team                          routing.Team
	orgName, teamName, enrollment string
	notes                         []string
}

// prepareTick fetches t's policy on the shared user-token client. Only a personal
// organization may collect everything; non-personal global policies collect nothing.
func prepareTick(ctx context.Context, client *api.Client, authURL string, o orgRow, t teamRow) (*tick, error) {
	personal := o.Type == "personal"
	pol, err := fetchTeamPolicy(ctx, client, authURL, o.ID, t.ID)
	switch {
	case teamNotFound(err):
		return nil, fmt.Errorf("team %s is no longer in %s", cmp.Or(t.Name, t.ID), orgTitle(o))
	case orgNotAccessibleErr(err):
		return nil, fmt.Errorf("you no longer have access to %s, so %s cannot be ticked", orgTitle(o), cmp.Or(t.Name, t.ID))
	case err != nil:
		return nil, fmt.Errorf("fetch %s's policy: %w", cmp.Or(t.Name, t.ID), err)
	}
	tk := &tick{orgName: o.Name, teamName: cmp.Or(t.Name, t.ID)}
	if pol.Global() && !personal {
		pol.Mode, pol.Repositories = config.ModeRepo, nil
	}
	if !pol.Global() && len(pol.Repositories) == 0 {
		tk.notes = append(tk.notes, tk.teamName+" collects nothing until an admin chooses its repositories")
	}
	tk.team = routing.Team{OrganizationID: o.ID, AuthURL: authURL, TeamID: t.ID, Personal: personal, Policy: pol}
	return tk, nil
}
