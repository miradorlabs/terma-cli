package cmd

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/keystore"
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
func refreshCollectionPolicy(ctx context.Context, cfg *config.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pol, err := fetchPolicy(ctx, cfg)
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
		return applyGlobalMode(ctx, cfg.Harnesses, pol.Global(), func(string) {}, func(string) {})
	}
	return nil
}

func currentTeamPolicy(ctx context.Context, cfg *config.Config, team string) (config.Policy, error) {
	cached, ok, err := routing.LoadPolicy(team)
	if err != nil {
		// A corrupt cache grants nothing. Only a new, validated response can
		// repair it; a rejected fetch leaves capture disabled.
		ok = false
	}
	if ok && !cached.AppliesTo(cfg.OrganizationID, cfg.AuthURL) {
		ok = false
	}
	if err == nil && !ok && cfg.Policy.TeamID == team && cfg.Policy.AppliesTo(cfg.OrganizationID, cfg.AuthURL) && !cfg.Policy.FetchedAt.IsZero() {
		cached, ok = cfg.Policy, true
	}
	if ok && time.Since(cached.FetchedAt) < policyRefreshInterval {
		return cached, nil
	}
	scoped := *cfg
	scoped.ProjectID = team
	if err := refreshCollectionPolicy(ctx, &scoped); err != nil {
		if ok {
			return cached, nil
		}
		return config.Policy{}, err
	}
	return scoped.Policy, nil
}

func pollCollectionPolicy(ctx context.Context) {
	for {
		if cfg, err := loadConfig(); err == nil {
			teams := keystore.CollectionProjects()
			selected := cfg.Policy.TeamID
			if selected == "" && cfg.Policy.Global() {
				selected = cfg.Policy.DefaultProjectID
			}
			if selected != "" && !slices.Contains(teams, selected) {
				teams = append(teams, selected)
			}
			for _, team := range teams {
				if ctx.Err() != nil {
					return
				}
				scoped := *cfg
				scoped.ProjectID = team
				_, _ = currentTeamPolicy(ctx, &scoped, team)
			}
		}
		// Discover newly connected teams promptly. Fresh caches avoid network calls;
		// each team's policy still refreshes only once per minute.
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
