package daemon

import (
	"errors"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// keychainRetry is how long a relay waits before asking a locked keychain again.
const keychainRetry = 30 * time.Second

// ResolverDeps is what a Resolver asks of the CLI.
type ResolverDeps struct {
	// AgentName names the agent behind a tool label, which its key is kept under.
	AgentName func(tool string) string
	// RelayTargets names the agents among the developer's choices that send through the relay.
	RelayTargets func(selected []string) []string
	// Endpoint is the ingest host a project's telemetry goes to.
	Endpoint func(projectID string) string
	// User is who parts sent to projectID at endpoint name, by Mirador user id; "" names no one.
	User func(projectID, endpoint string) string
}

// Resolver turns a claim into a Policy: the project's ingest host, relay.CapturePolicy, and
// for a profile set up with a server key, that key (the claiming agent's, else the team's).
// A person's profile names no key: the relay sends with their sign-in.
func Resolver(cfg *config.Config, r ResolverDeps) func(claim.Claim) (relay.Policy, error) {
	var mu sync.Mutex
	var lockedUntil time.Time
	var lockedErr error
	// teamKey is a server-key profile's key for c: the claiming agent's, else the team's.
	teamKey := func(c claim.Claim) (string, error) {
		// A locked keychain is asked again only after a while: on Linux every ask can raise
		// an unlock prompt, and the relay resolves far more often than a person can answer.
		mu.Lock()
		if time.Now().Before(lockedUntil) {
			err := lockedErr
			mu.Unlock()
			return "", err
		}
		mu.Unlock()
		key, err := keystore.GetFor(cfg.Dir, r.AgentName(c.Tool), c.ProjectID)
		if key == "" && err == nil {
			key, err = keystore.Get(cfg.Dir, c.ProjectID)
		}
		if err != nil {
			if secret.IsUnavailable(err) {
				mu.Lock()
				lockedUntil, lockedErr = time.Now().Add(keychainRetry), err
				mu.Unlock()
			}
			// Not ErrNoKey: a key the keychain will not give up now is not minted again.
			return "", err
		}
		if key == "" {
			return "", relay.ErrNoKey
		}
		return key, nil
	}
	return func(c claim.Claim) (relay.Policy, error) {
		org, chosen, serverKey := cfg.Policy, cfg.Harnesses, cfg.ServerKeySignIn
		// Reread the profile's team and its policy so a refreshed policy also governs queued exports.
		if file, err := config.LoadFile(cfg.Dir); err != nil {
			return relay.Policy{}, err
		} else if p := file.Profiles[cfg.ProfileName]; p == nil && cfg.ProfileName != "" {
			// The startup policy would outlive a sign-out or a removed profile.
			return relay.Policy{}, errors.New("profile removed; restart the relay")
		} else if p != nil {
			chosen, serverKey = p.Harnesses, p.ServerKeySignIn
			if p.OrganizationID != cfg.OrganizationID {
				return relay.Policy{}, errors.New("organization changed; restart the relay")
			}
			if stored, ok, err := config.ReadPolicy(cfg.StateDir, p.Team); err != nil || !ok || !stored.AppliesTo(cfg.OrganizationID, cfg.AuthURL) {
				org = config.NoPolicy(cfg.OrganizationID, cfg.AuthURL)
			} else {
				org = stored
			}
		}
		key := ""
		if serverKey {
			// A profile set up with a server key sends with it; a person's sends with their
			// sign-in (relay.Options.SignIn).
			var err error
			if key, err = teamKey(c); err != nil {
				return relay.Policy{}, err
			}
		}
		globalPrimary := org.Global() && (org.TeamID == "" || org.TeamID == c.ProjectID)
		org = routing.EffectivePolicy(cfg.StateDir, org, c.ProjectID)
		if cfg.ProfileName != "" && org.FetchedAt.IsZero() && config.PolicyStub() == "" {
			// Unknown policy must neither grant nor drop: a new team's exports wait for its fetch.
			return relay.Policy{}, errors.New("no validated collection policy for this team")
		}
		in := Capture{Org: org, Primary: globalPrimary, Agents: r.RelayTargets(chosen), Repository: c.Repository}
		if c.Tool != "" {
			in.Harness = r.AgentName(c.Tool)
		}
		pol := CapturePolicy(in)
		pol.Endpoint, pol.Key = r.Endpoint(c.ProjectID), key
		if r.User != nil {
			pol.User = r.User(c.ProjectID, pol.Endpoint)
			if pol.User != "" {
				pol.UserAccount = auth.AccountKey(cfg.AuthURL, pol.User)
			}
		}
		return pol, nil
	}
}

// CatchAll is where global mode files what nothing placed, rereading the policy every
// second, as the relay caches its policies, so a mode switch reaches a running relay before
// exports pile up held behind parts that arrived under the old mode.
func CatchAll(policy func() config.Policy) func() (claim.Claim, bool) {
	var mu sync.Mutex
	var at time.Time
	var cached config.Policy
	return func() (claim.Claim, bool) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) > time.Second {
			cached, at = policy(), time.Now()
		}
		if !cached.Global() || cached.DefaultProjectID == "" {
			return claim.Claim{}, false
		}
		return claim.Claim{ProjectID: cached.DefaultProjectID}, true
	}
}
