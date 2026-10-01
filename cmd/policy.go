package cmd

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

const policyRefreshInterval = time.Minute

// saveCollectionPolicy serializes refreshes with login changes. Each team has a
// separate revision; refreshing another team must not change machine-wide coverage.
func saveCollectionPolicy(cfg *config.Config, pol *config.Policy) error {
	var rejected error
	err := config.UpdateProfile(cfg.ProfileName, func(p *config.Profile) {
		if p.OrganizationID == "" && cfg.OrganizationID != "" {
			p.SelectOrganization(cfg.OrganizationID, "")
		}
		if p.OrganizationID != cfg.OrganizationID {
			rejected = errors.New("organization changed while fetching collection policy")
			return
		}
		if pol.TeamID != "" {
			if err := routing.SavePolicy(*pol); err != nil {
				rejected = err
				return
			}
		}
		if p.Policy == nil || p.Policy.TeamID == pol.TeamID || cfg.Policy.TeamID == pol.TeamID {
			p.Policy = pol
		}
	})
	if err != nil {
		return err
	}
	return rejected
}

// refreshCollectionPolicy uses the developer login, never a telemetry key. A
// rejected request cannot replace the last validated team's capture policy.
func (app *App) refreshCollectionPolicy(ctx context.Context, cfg *config.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pol, err := app.fetchPolicy(ctx, cfg)
	if err != nil {
		return err
	}
	if err := saveCollectionPolicy(cfg, &pol); err != nil {
		return err
	}
	cfg.Policy = pol
	file, err := config.LoadFile()
	if err != nil {
		return err
	}
	if p := file.Profiles[cfg.ProfileName]; p != nil && p.Policy != nil && p.Policy.TeamID == pol.TeamID {
		return app.applyGlobalMode(ctx, cfg.Harnesses, pol.Global(), func(string) {}, func(string) {})
	}
	return nil
}

// validatedPolicy is team's last validated policy for cfg's organization and
// environment: its cache, or the profile's copy of the selected team's. A corrupt cache
// grants nothing; only a new, validated response repairs it.
func validatedPolicy(cfg *config.Config, team string) (config.Policy, bool) {
	cached, ok, err := routing.LoadPolicy(team)
	if err != nil {
		return config.Policy{}, false
	}
	if ok && !cached.AppliesTo(cfg.OrganizationID, cfg.AuthURL) {
		ok = false
	}
	if !ok && cfg.Policy.TeamID == team && cfg.Policy.AppliesTo(cfg.OrganizationID, cfg.AuthURL) && !cfg.Policy.FetchedAt.IsZero() {
		cached, ok = cfg.Policy, true
	}
	return cached, ok
}

func (app *App) currentTeamPolicy(ctx context.Context, cfg *config.Config, team string) (config.Policy, error) {
	cached, ok := validatedPolicy(cfg, team)
	if ok && time.Since(cached.FetchedAt) < policyRefreshInterval {
		return cached, nil
	}
	scoped := *cfg
	scoped.ProjectID = team
	if err := app.refreshCollectionPolicy(ctx, &scoped); err != nil {
		if ok {
			return cached, nil
		}
		return config.Policy{}, err
	}
	return scoped.Policy, nil
}

// policyRefresher keeps every team this machine exports for fresh while the relay runs:
// the teams with a key here, and the selected team (global mode's default project).
func (app *App) policyRefresher() *daemon.PolicyRefresher {
	return &daemon.PolicyRefresher{
		Interval: policyRefreshInterval,
		Discover: 5 * time.Second,
		Teams: func() []string {
			cfg, err := app.loadConfig()
			if err != nil {
				return nil
			}
			teams := keystore.CollectionProjects()
			selected := cfg.Policy.TeamID
			if selected == "" && cfg.Policy.Global() {
				selected = cfg.Policy.DefaultProjectID
			}
			if selected != "" && !slices.Contains(teams, selected) {
				teams = append(teams, selected)
			}
			return teams
		},
		Fetched: func(team string) time.Time {
			cfg, err := app.loadConfig()
			if err != nil {
				return time.Time{}
			}
			if cached, ok := validatedPolicy(cfg, team); ok {
				return cached.FetchedAt
			}
			return time.Time{}
		},
		Refresh: func(ctx context.Context, team string) error {
			cfg, err := app.loadConfig()
			if err != nil {
				return err
			}
			scoped := *cfg
			scoped.ProjectID = team
			return app.refreshCollectionPolicy(ctx, &scoped)
		},
	}
}
