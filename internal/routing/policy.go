// Package routing decides where a record goes. Route walks a profile's routing state
// (State: the selected teams, their last validated policies and the developer's
// bindings) and sends each record to the first team allowed to collect it, personal
// last. It also keeps each team's collection policy in the state directory
// (config.PoliciesDir), the one copy hooks, doctor, the relay and delivery read today.
package routing

import (
	"cmp"
	"errors"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// EffectivePolicy is team's stored policy, else an unscoped or same-team fallback, else
// collect nothing; it never borrows another team's grant, and an expired one grants
// nothing.
func EffectivePolicy(stateDir string, fallback config.Policy, team string) config.Policy {
	p, ok, err := config.ReadPolicy(stateDir, team)
	switch {
	case err == nil && ok && p.AppliesTo(fallback.OrganizationID, fallback.AuthURL):
	case err == nil && !ok && (fallback.TeamID == "" || fallback.TeamID == team):
		p = fallback
	default:
		return config.NoPolicy("", "")
	}
	if p.Expired(time.Now()) {
		return config.NoPolicy(p.OrganizationID, p.AuthURL)
	}
	return p
}

// StorePolicy records a freshly fetched policy as its team's, and as the profile's team
// when the profile has none or chose it (setup). It refuses a policy fetched under
// another login's organization or environment, or whose revision moved backwards; every
// fetcher stores through it.
func StorePolicy(cfg *config.Config, pol *config.Policy) error {
	if !pol.AppliesTo(cfg.OrganizationID, cfg.AuthURL) {
		return errors.New("the collection policy fetched belongs to another organization or environment")
	}
	path, err := config.PolicyPath(cfg.StateDir, pol.TeamID)
	if err != nil {
		return err
	}
	file, err := config.LoadFile(cfg.Dir)
	if err != nil {
		return err
	}
	profile := file.Profiles[cmp.Or(cfg.ProfileName, file.ActiveProfile)]
	if profile == nil {
		profile = &config.Profile{}
	}
	if profile.OrganizationID != "" && profile.OrganizationID != cfg.OrganizationID {
		return errors.New("organization changed while fetching collection policy")
	}
	if err := writePolicyFile(cfg.StateDir, path, pol); err != nil {
		return err
	}
	// Only a profile that records something new is rewritten: refreshes leave config.json alone.
	selects := profile.Team != pol.TeamID && (profile.Team == "" || cfg.Policy.TeamID == pol.TeamID)
	if profile.OrganizationID != "" && !selects {
		return nil
	}
	var rejected error
	err = config.UpdateProfile(cfg.Dir, cfg.ProfileName, func(p *config.Profile) {
		// The team is recorded only under the sign-in the policy was fetched for: one as
		// someone else, stored since, cleared the profile for itself (config.Profile.Account).
		if p.Account != cfg.Account {
			rejected = errSignedInSince
			return
		}
		if p.OrganizationID == "" && cfg.OrganizationID != "" {
			p.SelectOrganization(cfg.OrganizationID, "")
		}
		switch {
		case p.OrganizationID != cfg.OrganizationID:
			rejected = errors.New("organization changed while fetching collection policy")
		case p.Team == "" || cfg.Policy.TeamID == pol.TeamID:
			if p.Team != pol.TeamID {
				p.TeamName = ""
			}
			p.Team = pol.TeamID
		}
	})
	if err != nil {
		return err
	}
	return rejected
}

// StoreTeamPolicy writes pol as its team's policy file in stateDir without touching
// config.json or checking a profile's single organization. It requires TeamID,
// OrganizationID, and AuthURL and refuses a backward revision for the same org/env.
func StoreTeamPolicy(stateDir string, pol *config.Policy) error {
	if pol == nil || pol.TeamID == "" || pol.OrganizationID == "" || pol.AuthURL == "" {
		return errors.New("a team's collection policy is stored only with its team, organization and environment")
	}
	path, err := config.PolicyPath(stateDir, pol.TeamID)
	if err != nil {
		return err
	}
	return writePolicyFile(stateDir, path, pol)
}

func writePolicyFile(stateDir, path string, pol *config.Policy) error {
	return flock.Locked(path, 5*time.Second, func() error {
		prev, ok, _ := config.ReadPolicy(stateDir, pol.TeamID)
		if ok && prev.OrganizationID == pol.OrganizationID && prev.AuthURL == pol.AuthURL && prev.Revision > pol.Revision {
			return errors.New("collection policy revision moved backwards")
		}
		return config.WritePolicy(stateDir, *pol)
	})
}

// errSignedInSince is a sign-in as someone else, stored while a policy was fetched.
var errSignedInSince = errors.New("signed in as someone else while fetching collection policy")

// ValidatedPolicy is team's last validated policy for cfg's organization and
// environment; an unreadable one grants nothing.
func ValidatedPolicy(cfg *config.Config, team string) (config.Policy, bool) {
	cached, ok, err := config.ReadPolicy(cfg.StateDir, team)
	if err != nil {
		return config.Policy{}, false
	}
	if ok && !cached.AppliesTo(cfg.OrganizationID, cfg.AuthURL) {
		ok = false
	}
	if !ok && cfg.Policy.TeamID == team && cfg.Policy.AppliesTo(cfg.OrganizationID, cfg.AuthURL) && !cfg.Policy.FetchedAt.IsZero() {
		cached, ok = cfg.Policy, true
	}
	return cached, ok
}
