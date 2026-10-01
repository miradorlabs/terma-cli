package daemon

import (
	"errors"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// ResolverDeps is what a Resolver asks of the CLI.
type ResolverDeps struct {
	// Mint asks for a claimed project's missing key; nil when none may be minted.
	Mint func(projectID string)
	// AgentName names the agent behind a tool label, which its key is kept under.
	AgentName func(tool string) string
	// Endpoint is the ingest host a project's telemetry goes to.
	Endpoint func(projectID string) string
}

// Resolver turns a claim into a Policy: the project's ingest host, the claiming agent's key
// (else the spool key, else a background mint while the parts wait), and relay.CapturePolicy.
func Resolver(cfg *config.Config, r ResolverDeps) func(claim.Claim) (relay.Policy, error) {
	mint := r.Mint
	return func(c claim.Claim) (relay.Policy, error) {
		key := keystore.GetFor(r.AgentName(c.Tool), c.ProjectID)
		if key == "" {
			key = keystore.Get(c.ProjectID)
		}
		if key == "" {
			if mint != nil {
				mint(c.ProjectID)
			}
			return relay.Policy{}, relay.ErrNoKey
		}
		org := cfg.Policy
		// Reread the profile so a refreshed policy also governs queued exports.
		if file, err := config.LoadFile(); err != nil {
			return relay.Policy{}, err
		} else if p := file.Profiles[cfg.ProfileName]; p == nil && cfg.ProfileName != "" {
			// The startup policy would outlive a sign-out or a removed profile.
			return relay.Policy{}, errors.New("profile removed; restart the relay")
		} else if p != nil {
			if p.OrganizationID != cfg.OrganizationID {
				return relay.Policy{}, errors.New("organization changed; restart the relay")
			}
			if p.Policy == nil || !p.Policy.AppliesTo(cfg.OrganizationID, cfg.AuthURL) || p.Policy.TeamID == "" && config.PolicyStub() == "" {
				org = config.NoPolicy(cfg.OrganizationID, cfg.AuthURL)
			} else {
				org = *p.Policy
			}
		}
		globalPrimary := org.Global() && (org.TeamID == "" || org.TeamID == c.ProjectID)
		org = routing.EffectivePolicy(org, c.ProjectID)
		if cfg.ProfileName != "" && org.FetchedAt.IsZero() && config.PolicyStub() == "" {
			// Unknown policy must neither grant nor drop: a new team's exports wait for its fetch.
			return relay.Policy{}, errors.New("no validated collection policy for this team")
		}
		in := Capture{Org: org, Primary: globalPrimary}
		if c.Tool != "" {
			in.Harness = r.AgentName(c.Tool)
		}
		if rec, ok, err := routing.LoadRecord(c.ProjectID); err != nil {
			in.RecordErr = err
		} else if ok {
			in.Record = &rec
		}
		pol := CapturePolicy(in)
		pol.Endpoint, pol.Key = r.Endpoint(c.ProjectID), key
		return pol, nil
	}
}

// CatchAll is where global mode files what nothing placed, rereading the policy every 10
// seconds so a mode switch reaches a running relay.
func CatchAll(policy func() config.Policy) func() (claim.Claim, bool) {
	var mu sync.Mutex
	var at time.Time
	var cached config.Policy
	return func() (claim.Claim, bool) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) > 10*time.Second {
			cached, at = policy(), time.Now()
		}
		if !cached.Global() || cached.DefaultProjectID == "" {
			return claim.Claim{}, false
		}
		return claim.Claim{ProjectID: cached.DefaultProjectID}, true
	}
}
