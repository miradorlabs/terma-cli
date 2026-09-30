package cmd

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/routing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// relayResolver turns a claim into where its session's telemetry goes: the project's
// own ingest host (projectEndpoint, the spool's order), the key this machine holds for
// the claiming agent (else the project's spool key), and what the routing record lets
// through.
//
// No key yet — a repository the platform connected, where no `terma install` ran — asks
// mint for one in the background (relayKeyMinter): the session's parts wait in the hold
// meanwhile, as they do for any keyless claim. Content and signals are
// relay.CapturePolicy's: the organization's policy (fetched by `terma setup`) is the
// ceiling, and the developer's routing record for the project can only narrow it.
func relayResolver(cfg *config.Config, mint func(projectID string)) func(claim.Claim) (relay.Policy, error) {
	return func(c claim.Claim) (relay.Policy, error) {
		key := keystore.GetFor(registered.NameForTool(c.Tool), c.ProjectID)
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
			in.Harness = registered.NameForTool(c.Tool)
		}
		if rec, ok, err := routing.LoadRecord(c.ProjectID); err != nil {
			in.RecordErr = err
		} else if ok {
			in.Record = &rec
		}
		pol := relay.CapturePolicy(in)
		pol.Endpoint, pol.Key = projectEndpoint(cfg, c.ProjectID), key
		return pol, nil
	}
}

// relayCatchAll is where global mode files what nothing placed: the organization's
// default project. The policy is read again at most every 10 seconds, so a `terma
// setup` that switches mode takes effect in a running relay (the service outlives it).
func relayCatchAll() func() (claim.Claim, bool) {
	var mu sync.Mutex
	var at time.Time
	var cached config.Policy
	return func() (claim.Claim, bool) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) > 10*time.Second {
			cached, at = hookPolicy(), time.Now()
		}
		if !cached.Global() || cached.DefaultProjectID == "" {
			return claim.Claim{}, false
		}
		return claim.Claim{ProjectID: cached.DefaultProjectID}, true
	}
}
