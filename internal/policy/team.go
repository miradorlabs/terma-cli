package policy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// FetchTeam asks t's organization for t's policy with that organization's own sign-in
// (auth.LoadCredentialFor), never the profile's active one, and reads it with wildcards
// (api.Client.TeamPolicy). On a profile set up with a server key, which stays single-team,
// it reads with t's own key from keys.v2.json. The policy is stamped with t's team,
// organization and environment, which alone make it t's (routing.State.ApplyRefresh).
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
	// Only an explicit offline fixture skips the organization's credential.
	case config.PolicyStub() != "":
		client = api.NewAnonymous(t.AuthURL, s.Version)
	case cfg.ServerKeySignIn:
		// The key goes only to the auth host it was set up against, as a login does.
		if cfg.AuthURL != cfg.ServerKeyAuthURL {
			return config.Policy{}, fmt.Errorf("this machine's server key was set up against %s, not %s — run `terma setup` with TERMA_API_KEY set", cfg.ServerKeyAuthURL, cfg.AuthURL)
		}
		key, _, err := keystore.GetTeam(cfg.Dir, t.EnrollmentID)
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
		cred, err := auth.LoadCredentialFor(cfg.Dir, cfg.ProfileName, t.OrganizationID)
		if err != nil {
			return config.Policy{}, err
		}
		if client, err = api.New(&scoped, api.Options{Version: s.Version, ProjectID: t.TeamID, Credential: cred}); err != nil {
			return config.Policy{}, err
		}
	}
	pol, err := client.TeamPolicy(ctx)
	if err != nil {
		return config.Policy{}, fmt.Errorf("fetch team %s's collection policy: %w", t.TeamID, err)
	}
	pol.OrganizationID, pol.AuthURL, pol.TeamID = t.OrganizationID, t.AuthURL, t.TeamID
	if pol.Global() {
		pol.DefaultProjectID = t.TeamID
	}
	return pol, nil
}

// RefreshTeam fetches t's policy (FetchTeam) as the refresher applies it: the policy, or
// the class of its failure (Classify), under the epoch t had as the request started, so a
// success already in flight never undoes a refusal recorded meanwhile. The policy is also
// kept as its team's file (StorePolicy), what the refresher fetched; the routing state is
// what routing uses. The error is the failure, for the relay's log.
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
	// Best effort: a file that cannot be written changes nothing the routing state decides.
	_ = routing.StorePolicy(cfg, &pol)
	r.Policy = &pol
	return r, nil
}

// Classify is how a refresh failed, by status, reason and context, never by message
// (contract C3):
//
//   - a token refresh refused with ORG_NOT_ACCESSIBLE: OrgAccessLost, the team Refused at once;
//   - a 403 refusing the policy read with TEAM_NOT_FOUND: Terminal, suspending the team;
//   - any other 401, no sign-in for the organization, or one from another environment:
//     SignIn, until the developer signs in again;
//   - anything else (another 403, a 5xx, a 429, the network, a locked keychain, a team of
//     another environment): Transient, the last policy in force until it expires.
func Classify(err error) routing.ErrorClass {
	var refresh *api.RefreshError
	var apiErr *api.Error
	var otherEnv *auth.ErrWrongEnvironment
	refreshing := errors.As(err, &refresh)
	switch {
	case err == nil:
		return ""
	case refreshing && errors.As(refresh.Err, &apiErr) && apiErr.Unauthenticated() && apiErr.Reason == api.ReasonOrgNotAccessible:
		return routing.OrgAccessLost
	case !refreshing && errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden && apiErr.Reason == api.ReasonTeamNotFound:
		return routing.Terminal
	case errors.As(err, &apiErr) && apiErr.Unauthenticated(), errors.Is(err, auth.ErrNotLoggedIn), errors.As(err, &otherEnv):
		return routing.SignIn
	}
	return routing.Transient
}
