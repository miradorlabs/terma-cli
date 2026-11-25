package cli

import (
	"errors"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

type bindingChoice struct {
	key      string
	org      string
	refuse   bool
	override bool
}

// autoSetupBindings computes non-prompting bindings for a single team (used by server-key
// setup and server-key routing migration).
func autoSetupBindings(target routing.Team, existing map[string]routing.Binding) []bindingChoice {
	choices, _ := planSetupBindings([]discoveredTeam{{team: target}}, nil, existing, teamNames{})
	return choices
}

func applySetupBindingsAll(s *routing.State, choices []bindingChoice) error {
	for _, c := range choices {
		if _, decided := s.Bindings[c.key]; decided && !c.override {
			continue
		}
		if config.IsRepositoryKey(c.key) && !c.override {
			if _, ownerDecided := s.Bindings[routing.Owner(c.key)]; ownerDecided {
				continue
			}
		}
		if c.refuse {
			if err := s.Refuse(c.key); err != nil {
				return err
			}
		} else if err := s.Bind(c.key, c.org, c.override); err != nil {
			return err
		}
	}
	return nil
}

func alreadyDecided(bindings map[string]routing.Binding, e routing.Entry) bool {
	if len(bindings) == 0 {
		return false
	}
	if _, ok := bindings[e.Key()]; ok {
		return true
	}
	if !e.Wildcard() {
		_, ok := bindings[routing.Owner(e.Key())]
		return ok
	}
	return false
}

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
