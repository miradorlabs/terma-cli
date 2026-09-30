package daemon

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// ResolverDeps is what a Resolver asks of the CLI.
type ResolverDeps struct {
	// Mint asks for a claimed project's missing key; nil when none may be minted.
	Mint func(projectID string)
	// AgentName is the agent whose hooks carry a tool label, which its key is kept under.
	AgentName func(tool string) string
	// Endpoint is the ingest host a project's telemetry goes to.
	Endpoint func(projectID string) string
}

// Resolver turns a claim into where its session's telemetry goes: the project's
// own ingest host (ResolverDeps.Endpoint), the key this machine holds for
// the claiming agent (else the project's spool key), and what the routing record lets
// through.
//
// No key yet — a repository the platform connected, where no `terma install` ran — asks
// mint for one in the background (KeyMinter): the session's parts wait in the hold
// meanwhile, as they do for any keyless claim. Content and signals are
// relay.CapturePolicy's: the organization's policy (fetched by `terma setup`) is the
// ceiling, and the developer's routing record for the project can only narrow it.
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
		// Reread the profile so a refreshed policy also governs queued exports. The
		// resolver's cache bounds these local reads; hooks never fetch the network.
		if file, err := config.LoadFile(); err != nil {
			return relay.Policy{}, err
		} else if p := file.Profiles[cfg.ProfileName]; p != nil {
			if p.OrganizationID != cfg.OrganizationID {
				return relay.Policy{}, errors.New("organization changed; restart the relay")
			}
			if p.Policy == nil || !p.Policy.AppliesTo(cfg.OrganizationID, cfg.AuthURL) || p.Policy.TeamID == "" && os.Getenv("TERMA_POLICY_STUB") == "" {
				org = config.Policy{Mode: config.ModeRepo, Signals: []string{}, OrganizationID: cfg.OrganizationID, AuthURL: cfg.AuthURL}
			} else {
				org = *p.Policy
			}
		}
		globalPrimary := org.Global() && (org.TeamID == "" || org.TeamID == c.ProjectID)
		org = routing.EffectivePolicy(org, c.ProjectID)
		if cfg.ProfileName != "" && org.FetchedAt.IsZero() && os.Getenv("TERMA_POLICY_STUB") == "" {
			// A new team's first exports wait for its background fetch, just as they
			// wait for a missing key. Unknown policy must neither grant nor drop them.
			return relay.Policy{}, errors.New("no validated collection policy for this team")
		}
		in := relay.Capture{Org: org, Primary: globalPrimary}
		if c.Tool != "" {
			in.Harness = r.AgentName(c.Tool)
		}
		if rec, ok, err := routing.LoadRecord(c.ProjectID); err != nil {
			in.RecordErr = err
		} else if ok {
			in.Record = &rec
		}
		pol := relay.CapturePolicy(in)
		pol.Endpoint, pol.Key = r.Endpoint(c.ProjectID), key
		return pol, nil
	}
}

// CatchAll is where global mode files what nothing placed: the organization's
// default project. The policy is read again at most every 10 seconds, so a `terma
// setup` that switches mode takes effect in a running relay (the service outlives it).
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
