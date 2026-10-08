// Package policy fetches an organization's collection policy with the developer's login,
// or, on a profile set up with a server key, with the team's own key, and keeps the
// selected team's last validated one in the state directory (internal/routing).
package policy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// RefreshInterval is how long a validated policy is used before it is fetched again.
const RefreshInterval = time.Minute

// Source fetches policies as one terma build.
type Source struct {
	Version string
}

// Fetch asks cfg's organization for the collection policy of cfg's project: with the
// project's key from the keystore when the profile signed in with a server key, which
// reads only the profile's team, else with the developer's login.
func (s Source) Fetch(ctx context.Context, cfg *config.Config) (config.Policy, error) {
	switch {
	// Only an explicit offline fixture skips the profile's credential.
	case config.PolicyStub() != "":
		return s.fetch(ctx, cfg, api.NewAnonymous(cfg.AuthURL, s.Version))
	case cfg.ServerKeySignIn:
		// A key kept from another organization's team would be stamped with this one's.
		if cfg.ProjectID != cfg.Team {
			return config.Policy{}, fmt.Errorf("this machine is set up with team %s's server key, which fetches only that team's policy, not team %s's", cfg.Team, cfg.ProjectID)
		}
		// The key goes only to the auth host it was set up against, as a login does (CheckEnvironment).
		if cfg.AuthURL != cfg.ServerKeyAuthURL {
			return config.Policy{}, fmt.Errorf("this machine's server key was set up against %s, not %s — run `terma setup` with TERMA_API_KEY set", cfg.ServerKeyAuthURL, cfg.AuthURL)
		}
		key, err := keystore.Get(cfg.Dir, cfg.ProjectID)
		if err != nil {
			return config.Policy{}, err
		}
		if key == "" {
			return config.Policy{}, fmt.Errorf("no server key for team %s on this machine — run `terma setup` with TERMA_API_KEY set", cfg.ProjectID)
		}
		return s.FetchWithKey(ctx, cfg, key)
	}
	cred, err := auth.LoadCredential(cfg.Dir, cfg.ProfileName)
	if err != nil {
		return config.Policy{}, err
	}
	if cfg.OrganizationID == "" {
		cfg.OrganizationID = cred.OrganizationID
	}
	if cfg.OrganizationID != cred.OrganizationID {
		return config.Policy{}, errors.New("collection policy login belongs to another organization — run `terma setup`")
	}
	// A login profile always uses the developer login, even while TERMA_API_KEY is set.
	policyConfig := *cfg
	policyConfig.APIKey = ""
	client, err := api.New(&policyConfig, api.Options{Version: s.Version, ProjectID: cfg.ProjectID, Credential: cred})
	if err != nil {
		return config.Policy{}, err
	}
	return s.fetch(ctx, cfg, client)
}

// FetchWithKey is Fetch with key, cfg's project's own server key, which reads that
// project's policy and no other.
func (s Source) FetchWithKey(ctx context.Context, cfg *config.Config, key string) (config.Policy, error) {
	keyed := *cfg
	keyed.APIKey = key
	client, err := api.New(&keyed, api.Options{Version: s.Version, ProjectID: cfg.ProjectID})
	if err != nil {
		return config.Policy{}, err
	}
	return s.fetch(ctx, cfg, client)
}

// fetch asks client for cfg's project's policy and stamps it with the login and team it
// was fetched for.
func (s Source) fetch(ctx context.Context, cfg *config.Config, client *api.Client) (config.Policy, error) {
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
