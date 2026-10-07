package daemon

import (
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

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
	// Mint asks for a claimed project's missing key; nil when none may be minted.
	Mint func(projectID string)
	// AgentName names the agent behind a tool label, which its key is kept under.
	AgentName func(tool string) string
	// RelayTargets names the agents among the developer's choices that send through the relay.
	RelayTargets func(selected []string) []string
	// Endpoint is the ingest host a project's telemetry goes to.
	Endpoint func(projectID string) string
}

// Resolver turns a claim into a Policy: the project's ingest host, the claiming agent's key
// (else the spool key, else a background mint while the parts wait), and relay.CapturePolicy.
func Resolver(cfg *config.Config, r ResolverDeps) func(claim.Claim) (relay.Policy, error) {
	mint := r.Mint
	var mu sync.Mutex
	var lockedUntil time.Time
	var lockedErr error
	return func(c claim.Claim) (relay.Policy, error) {
		// A locked keychain is asked again only after a while: on Linux every ask can raise
		// an unlock prompt, and the relay resolves far more often than a person can answer.
		mu.Lock()
		if time.Now().Before(lockedUntil) {
			err := lockedErr
			mu.Unlock()
			return relay.Policy{}, err
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
			return relay.Policy{}, err
		}
		if key == "" {
			if mint != nil {
				mint(c.ProjectID)
			}
			return relay.Policy{}, relay.ErrNoKey
		}
		selected, chosen := cfg.Policy, cfg.Harnesses
		// Reread the profile's team and its policy so a refreshed policy also governs queued
		// exports; a profile signed into another organization since keeps the policies it
		// stored, as this machine collects for every organization it is signed into.
		file, err := config.LoadFile(cfg.Dir)
		if err != nil {
			return relay.Policy{}, err
		}
		if p := file.Profiles[cfg.ProfileName]; p == nil && cfg.ProfileName != "" {
			// The startup policy would outlive a sign-out or a removed profile.
			return relay.Policy{}, errors.New("profile removed; restart the relay")
		} else if p != nil {
			chosen = p.Harnesses
			if stored, ok, err := config.ReadPolicy(cfg.StateDir, p.Team); err != nil || !ok || !stored.SameEnvironment(cfg.AuthURL) {
				selected = config.NoPolicy(p.OrganizationID, cfg.AuthURL)
			} else {
				selected = stored
			}
		}
		now := *cfg
		now.Policy = selected
		if p := file.Profiles[cfg.ProfileName]; p != nil {
			now.Teams = p.CollectedTeams()
		}
		// Only a team this machine collects for is granted: a claim for a team selected
		// nowhere since, or for another team's global policy, is a session nothing placed.
		// Hooks under another profile claim for that profile's teams (`terma config`), and
		// the one relay forwards for every profile's.
		collected := routing.Collection(&now)
		global, hasGlobal := collected.Global()
		globalPrimary := hasGlobal && (global.TeamID == "" || global.TeamID == c.ProjectID)
		org := routing.EffectivePolicy(cfg.StateDir, selected, c.ProjectID)
		switch {
		case org.Global() && !globalPrimary,
			org.Validated() && !collectedFor(file, collected, c.ProjectID):
			org = config.NoPolicy(org.OrganizationID, org.AuthURL)
		}
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
		return pol, nil
	}
}

// collectedFor reports whether team is one this machine collects for: in the running
// profile's collection, or selected in any profile of the installation.
func collectedFor(file *config.File, collected config.Policies, team string) bool {
	if slices.ContainsFunc(collected, func(p config.Policy) bool { return p.Team() == team }) {
		return true
	}
	for _, p := range file.Profiles {
		if slices.Contains(slices.Collect(maps.Values(p.CollectedTeams())), team) {
			return true
		}
	}
	return false
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
