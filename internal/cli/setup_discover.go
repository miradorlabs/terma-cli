package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

type discoveredTeam struct {
	team     routing.Team
	orgName  string
	teamName string
}

type setupDiscovery struct {
	orgs             []organization
	teams            []discoveredTeam
	names            teamNames
	inaccessibleOrgs []string
	warnings         []string
	scoped           bool
	stub             bool
	baseline         setupBaseline
}

// discoverSetup fetches the signed-in developer's organizations and associated teams (and
// their policies) under one continuous Terma-branded loading spinner. When --org or --team
// is passed, discovery scopes to the named organization or team. Under TERMA_POLICY_STUB,
// it still lists organizations and records the target organization, leaving stub policy
// loading to setupTeams.
func (app *App) discoverSetup(ctx context.Context, cmd *cobra.Command, cfg *config.Config, cred *auth.Credential, wantOrg orgRef, wantTeam string) (*setupDiscovery, error) {
	baseline := app.loadSetupBaseline(cfg.ProfileName, cfg.Account)
	client, err := api.New(cfg, api.Options{Version: app.version, Credential: cred})
	if err != nil {
		return nil, err
	}

	sp := app.startSpinner(cmd.ErrOrStderr())
	defer sp.Stop()
	sp.Start("Fetching organizations…")

	orgs, err := fetchOrganizations(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("list your organizations: %w", err)
	}
	if len(orgs) == 0 {
		return nil, errNoOrganization
	}
	slices.SortStableFunc(orgs, func(a, b organization) int {
		return cmp.Compare(organizationRank(a), organizationRank(b))
	})

	scoped := !wantOrg.empty() || wantTeam != ""
	targetOrgs := orgs
	if !wantOrg.empty() {
		org, err := matchOrganization(orgs, wantOrg.String())
		if errors.As(err, new(noMatchError)) {
			return nil, fmt.Errorf("%w: %w", errNotYourOrganization, err)
		}
		if err != nil {
			return nil, err
		}
		targetOrgs = []organization{*org}
	}

	names := loadTeamNames(app.stateDir, cfg.ProfileName)
	for _, o := range orgs {
		names.org(o.ID, o.Name)
	}

	if config.PolicyStub() != "" {
		names.save(app.stateDir, cfg.ProfileName)
		chosen := pickStubOrganization(targetOrgs, cfg.OrganizationID, !wantOrg.empty())
		if err := recordOrganization(app.dir, cfg.ProfileName, cred, &chosen); err != nil {
			return nil, err
		}
		return &setupDiscovery{
			orgs:     []organization{chosen},
			names:    names,
			scoped:   scoped,
			stub:     true,
			baseline: baseline,
		}, nil
	}

	// Keep the same spinner running seamlessly while loading teams and their policies.
	sp.Start("Fetching teams…")
	listed := app.fetchTeams(ctx, client, cfg.AuthURL, targetOrgs)

	disc := &setupDiscovery{
		orgs:     targetOrgs,
		names:    names,
		scoped:   scoped,
		baseline: baseline,
	}
	inaccessible := map[string]bool{}
	markInaccessible := func(o organization) {
		if inaccessible[o.ID] {
			return
		}
		inaccessible[o.ID] = true
		disc.inaccessibleOrgs = append(disc.inaccessibleOrgs, o.ID)
		disc.warnings = append(disc.warnings, fmt.Sprintf("%s: %s", cmp.Or(o.Name, o.ID), orgInaccessibleNote))
	}

	for i, lo := range listed {
		o := targetOrgs[i]
		orgLabel := cmp.Or(o.Name, o.ID)
		switch {
		case orgNotAccessibleErr(lo.err):
			markInaccessible(o)
			continue
		case lo.err != nil:
			disc.warnings = append(disc.warnings, fmt.Sprintf("%s: teams could not be listed (%v)", orgLabel, lo.err))
			continue
		}
		personal := strings.EqualFold(o.Type, "personal")
		for _, p := range lo.teams {
			disc.names.team(p.ID, p.Name)
			if teamNotFound(p.err) {
				continue
			}
			switch {
			case orgNotAccessibleErr(p.err):
				markInaccessible(o)
				continue
			case p.err != nil:
				disc.warnings = append(disc.warnings, fmt.Sprintf("%s: policy could not be read (%v)", orgTeamLabel(o.Name, cmp.Or(p.Name, p.ID)), p.err))
				continue
			}
			pol := p.policy
			if !personal && pol.Global() {
				pol.Mode, pol.Repositories = config.ModeRepo, nil
			}
			disc.teams = append(disc.teams, discoveredTeam{
				team: routing.Team{
					OrganizationID: o.ID,
					AuthURL:        cfg.AuthURL,
					TeamID:         p.ID,
					Personal:       personal,
					Policy:         pol,
				},
				orgName:  o.Name,
				teamName: cmp.Or(p.Name, p.ID),
			})
		}
	}
	disc.names.save(app.stateDir, cfg.ProfileName)

	if len(disc.teams) == 0 {
		if len(disc.inaccessibleOrgs) > 0 && baseline.sameAccount {
			app.commitInaccessibleOrgs(ctx, cfg, cfg.Account, baseline, disc.inaccessibleOrgs)
		}
		if len(disc.warnings) > 0 {
			return nil, fmt.Errorf("could not load teams: %s", strings.Join(disc.warnings, "; "))
		}
		if len(targetOrgs) == 1 {
			return nil, errNoProjects
		}
		return nil, errors.New("no teams in your organizations yet — create one in the Terma app")
	}

	if wantTeam != "" {
		projects := make([]project, 0, len(disc.teams))
		for _, dt := range disc.teams {
			projects = append(projects, project{
				ID:             dt.team.TeamID,
				Name:           dt.teamName,
				OrganizationID: dt.team.OrganizationID,
			})
		}
		matched, err := matchProject(projects, wantTeam)
		if err != nil {
			return nil, err
		}
		idx := slices.IndexFunc(disc.teams, func(dt discoveredTeam) bool {
			return dt.team.TeamID == matched.ID && dt.team.OrganizationID == matched.OrganizationID
		})
		disc.teams = []discoveredTeam{disc.teams[idx]}
		if oi := slices.IndexFunc(disc.orgs, func(o organization) bool { return o.ID == matched.OrganizationID }); oi >= 0 {
			disc.orgs = []organization{disc.orgs[oi]}
		}
	}

	return disc, nil
}

func organizationRank(o organization) int {
	if strings.EqualFold(o.Type, "personal") {
		return 1
	}
	return 0
}

func pickStubOrganization(orgs []organization, currentID string, forced bool) organization {
	if !forced && currentID != "" {
		if i := slices.IndexFunc(orgs, func(o organization) bool { return o.ID == currentID }); i >= 0 {
			return orgs[i]
		}
	}
	return orgs[0]
}
