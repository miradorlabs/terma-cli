package cli

import (
	"errors"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

func committedPrimary(st routing.State, fallback routing.Team) routing.Team {
	for _, t := range st.Teams {
		if t.TeamID == fallback.TeamID {
			return t
		}
	}
	return fallback
}

func (app *App) persistCommittedPolicies(st routing.State) error {
	for _, t := range st.Teams {
		if t.Policy.FetchedAt.IsZero() {
			continue
		}
		if err := routing.StoreTeamPolicy(app.stateDir, t); err != nil {
			return err
		}
	}
	return nil
}

func (app *App) recordSetupTeam(cfg *config.Config, account, orgID, orgName, teamID, teamName string) error {
	var rejected error
	err := config.UpdateProfile(app.dir, cfg.ProfileName, func(p *config.Profile) {
		if account != sendas.NoPersonLane && p.Account != account {
			rejected = errors.New("signed in as someone else while setting up teams")
			return
		}
		if orgID != "" {
			if p.OrganizationID != orgID {
				p.OrganizationName = ""
			}
			p.OrganizationID = orgID
			if orgName != "" {
				p.OrganizationName = orgName
			}
		}
		if p.Team != teamID {
			p.TeamName = ""
		}
		p.Team = teamID
		if teamName != "" && teamName != teamID {
			p.TeamName = teamName
		}
	})
	if err != nil {
		return err
	}
	return rejected
}
