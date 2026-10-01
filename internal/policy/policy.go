// Package policy fetches an organization's collection policy with the developer's login,
// never a telemetry key, and keeps each team's last validated one (internal/routing holds
// the cache).
package policy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// RefreshInterval is how long a validated policy is used before it is fetched again.
const RefreshInterval = time.Minute

// Source fetches policies as one terma build.
type Source struct {
	Version string
	// ModeChanged applies a refreshed policy of the profile's own team machine-wide
	// (global mode on or off).
	ModeChanged func(ctx context.Context, cfg *config.Config, pol config.Policy) error
}

// Fetch asks cfg's organization for the collection policy of cfg's project, else its
// default team.
func (s Source) Fetch(ctx context.Context, cfg *config.Config) (config.Policy, error) {
	var client *api.Client
	// Only an explicit offline fixture skips the developer's login.
	if config.PolicyStub() != "" {
		client = api.NewAnonymous(cfg.AuthURL, s.Version)
	} else {
		cred, err := auth.LoadCredential(cfg.ProfileName)
		if err != nil {
			return config.Policy{}, err
		}
		if cfg.OrganizationID == "" {
			cfg.OrganizationID = cred.OrganizationID
		}
		if cfg.OrganizationID != cred.OrganizationID {
			return config.Policy{}, errors.New("collection policy login belongs to another organization — run `terma setup`")
		}
		// Policy always uses the developer login, even while TERMA_API_KEY is set.
		policyConfig := *cfg
		policyConfig.APIKey = ""
		client, err = api.New(&policyConfig, api.Options{Version: s.Version, ProjectID: cfg.ProjectID, Credential: cred})
		if err != nil {
			return config.Policy{}, err
		}
	}
	pol, err := client.CollectionPolicy(ctx)
	if err != nil {
		return config.Policy{}, fmt.Errorf("fetch the organization's collection policy: %w", err)
	}
	pol.OrganizationID, pol.AuthURL = cfg.OrganizationID, cfg.AuthURL
	pol.TeamID = cmp.Or(cfg.ProjectID, pol.DefaultProjectID)
	if pol.Global() && config.PolicyStub() == "" {
		pol.DefaultProjectID = cfg.ProjectID
	}
	return pol, nil
}

// Refresh fetches and stores cfg's project's policy; a failure keeps the last validated
// one, which stops granting anything once it expires (config.MaxPolicyAge).
func (s Source) Refresh(ctx context.Context, cfg *config.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pol, err := s.Fetch(ctx, cfg)
	if err != nil {
		return err
	}
	if err := routing.StorePolicy(cfg, &pol); err != nil {
		return err
	}
	cfg.Policy = pol
	file, err := config.LoadFile()
	if err != nil {
		return err
	}
	if p := file.Profiles[cfg.ProfileName]; p != nil && p.Policy != nil && p.Policy.TeamID == pol.TeamID && s.ModeChanged != nil {
		return s.ModeChanged(ctx, cfg, pol)
	}
	return nil
}

// Current is team's policy: the validated one while it is fresh, else a refreshed one,
// else the stale validated one until it expires; a team never validated has none.
func (s Source) Current(ctx context.Context, cfg *config.Config, team string) (config.Policy, error) {
	cached, ok := routing.ValidatedPolicy(cfg, team)
	if ok && time.Since(cached.FetchedAt) < RefreshInterval {
		return cached, nil
	}
	scoped := *cfg
	scoped.ProjectID = team
	if err := s.Refresh(ctx, &scoped); err != nil {
		if ok && !cached.Expired(time.Now()) {
			return cached, nil
		}
		return config.Policy{}, err
	}
	return scoped.Policy, nil
}
