package policy

import (
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

// ErrOtherEnvironment is a team selected in another environment than the profile now
// points at (TERMA_ENV, an endpoint override): it is neither fetched nor sent to, so it
// fails closed, and it expires as any team no refresh reaches. The routing state is not
// rewritten for it: the developer unticks it, and ticks the teams of the environment they
// use now.
var ErrOtherEnvironment = errors.New("the team was selected in another environment than this profile's")

// ErrAccountMismatch is a stored user sign-in belonging to a different account than the
// profile expects: FetchTeam refuses rather than refreshing one account's team with
// another person's token.
var ErrAccountMismatch = errors.New("stored sign-in belongs to another account")

// FetchTeam asks the gateway for t's collection policy using the profile's single user
// token (auth.LoadCredential), or, on a server-key profile, that team's key in keys.json
// (keystore.Get). The returned policy is stamped with t's team, organization, and
// environment, which alone make it t's (routing.State.ApplyRefresh).
func (s Source) FetchTeam(ctx context.Context, cfg *config.Config, t routing.Team) (config.Policy, error) {
	if t.TeamID == "" || t.OrganizationID == "" || t.AuthURL == "" {
		return config.Policy{}, errors.New("a team is fetched only with its team, organization and environment")
	}
	if t.AuthURL != cfg.AuthURL {
		return config.Policy{}, fmt.Errorf("team %s: %w (%s, not %s)", t.TeamID, ErrOtherEnvironment, t.AuthURL, cfg.AuthURL)
	}
	scoped := *cfg
	scoped.ProjectID, scoped.OrganizationID, scoped.APIKey = t.TeamID, t.OrganizationID, ""
	var client *api.Client
	switch {
	// Only an explicit offline fixture skips the profile's credential.
	case config.PolicyStub() != "":
		client = api.NewAnonymous(t.AuthURL, s.Version)
	case cfg.ServerKeySignIn:
		// A server-key profile reads only its configured team and only against the auth host
		// it was set up for.
		if t.TeamID != cfg.Team {
			return config.Policy{}, fmt.Errorf("this machine is set up with team %s's server key, which fetches only that team's policy, not team %s's", cfg.Team, t.TeamID)
		}
		if cfg.AuthURL != cfg.ServerKeyAuthURL {
			return config.Policy{}, fmt.Errorf("this machine's server key was set up against %s, not %s — run `terma setup` with TERMA_API_KEY set", cfg.ServerKeyAuthURL, cfg.AuthURL)
		}
		key, err := keystore.Get(cfg.Dir, t.TeamID)
		if err != nil {
			return config.Policy{}, err
		}
		if key == "" {
			return config.Policy{}, fmt.Errorf("no server key for team %s on this machine — run `terma setup` with TERMA_API_KEY set", t.TeamID)
		}
		scoped.APIKey = key
		if client, err = api.New(&scoped, api.Options{Version: s.Version, ProjectID: t.TeamID}); err != nil {
			return config.Policy{}, err
		}
	default:
		cred, err := auth.LoadCredential(cfg.Dir, cfg.ProfileName)
		if err != nil {
			return config.Policy{}, err
		}
		if cfg.Account == "" || cred.Account() != cfg.Account {
			return config.Policy{}, fmt.Errorf("team %s: %w", t.TeamID, ErrAccountMismatch)
		}
		if client, err = api.New(&scoped, api.Options{Version: s.Version, ProjectID: t.TeamID, Credential: cred}); err != nil {
			return config.Policy{}, err
		}
	}
	pol, err := client.CollectionPolicy(ctx)
	if err != nil {
		return config.Policy{}, fmt.Errorf("fetch team %s's collection policy: %w", t.TeamID, err)
	}
	pol.OrganizationID, pol.AuthURL, pol.TeamID = t.OrganizationID, t.AuthURL, t.TeamID
	if pol.Global() && config.PolicyStub() == "" {
		pol.DefaultProjectID = t.TeamID
	}
	return pol, nil
}

// RefreshTeam fetches t's policy (FetchTeam) as the refresher applies it: the policy, or
// the class of its failure (Classify), under the epoch t had as the request started, so a
// delayed result never overwrites a newer transition.
//
// RefreshTeam does not persist the policy file: the caller applies the result inside
// routing.Update (which checks the state's account/generation and team epoch) and, only
// after routing.Update returns successfully, writes the committed team
// (*committed.Team(r.EnrollmentID)) via routing.StoreTeamPolicy (never inside the Update
// closure or from an uncommitted local copy). That ensures a stale-epoch or
// stale-generation result never overwrites policies/<team>.json and any non-personal
// global policy is persisted sanitized. Callers that handle OrgAccessLost org-wide
// should snapshot per-enrollment epochs across t.OrganizationID before invoking
// RefreshTeam and apply them via State.ApplyOrgAccessLost.
func (s Source) RefreshTeam(ctx context.Context, cfg *config.Config, t routing.Team) (routing.Refresh, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r := routing.Refresh{EnrollmentID: t.EnrollmentID, Epoch: t.Epoch}
	pol, err := s.FetchTeam(ctx, cfg, t)
	r.At = time.Now().UTC()
	if err != nil {
		r.Failure = Classify(err)
		return r, err
	}
	r.Policy = &pol
	return r, nil
}

// Classify maps a policy refresh failure to a routing.ErrorClass by status, AIP-193
// reason, and typed error, never by message text:
//
//   - HTTP 403 ORG_NOT_ACCESSIBLE: OrgAccessLost (authoritative loss of org membership);
//   - HTTP 403 PROJECT_NOT_FOUND: Terminal (1st suspends, 2nd refuses);
//   - HTTP 401, SIGN_IN_RETIRED, missing login, wrong-environment login, or account
//     mismatch: SignIn;
//   - anything else (403 AUTHORIZATION_UNAVAILABLE, bare 403, 404, 5xx, 429, network,
//     locked keychain, ErrOtherEnvironment): Transient.
func Classify(err error) routing.ErrorClass {
	if err == nil {
		return ""
	}
	var apiErr *api.Error
	var otherEnv *auth.ErrWrongEnvironment
	switch {
	case errors.As(err, &apiErr) && apiErr.OrgNotAccessible():
		return routing.OrgAccessLost
	case errors.As(err, &apiErr) && apiErr.ProjectNotFound():
		return routing.Terminal
	case errors.Is(err, auth.ErrSignInRetired),
		errors.Is(err, auth.ErrNotLoggedIn),
		errors.Is(err, ErrAccountMismatch),
		errors.As(err, &otherEnv),
		errors.As(err, &apiErr) && (apiErr.Unauthenticated() || apiErr.SignInRetired()):
		return routing.SignIn
	}
	return routing.Transient
}
