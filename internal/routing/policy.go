// Package routing keeps each team's collection policy in the state directory
// (config.PoliciesDir), the one copy hooks, doctor, the relay and delivery read; a
// profile records only which team it selected. One machine collects for several
// organizations at once: every stored policy of the environment counts (Collection), and
// each names the organization that fetched it, whose credential refreshes it.
package routing

import (
	"cmp"
	"errors"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// EffectivePolicy is team's stored policy, else an unscoped or same-team fallback, else
// collect nothing; it never borrows another team's grant, and an expired one grants
// nothing. A policy of another environment is not team's.
func EffectivePolicy(stateDir string, fallback config.Policy, team string) config.Policy {
	p, ok, err := config.ReadPolicy(stateDir, team)
	switch {
	case err == nil && ok && p.SameEnvironment(fallback.AuthURL):
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
// another login's organization or environment than cfg's, or whose revision moved
// backwards; every fetcher stores through it, with cfg scoped to the policy's
// organization (ScopeToTeam). A profile signed into another organization than the
// policy's keeps its own selection: the policy is stored for its team all the same.
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
	err = flock.Locked(path, 5*time.Second, func() error {
		prev, ok, _ := config.ReadPolicy(cfg.StateDir, pol.TeamID)
		if ok && prev.OrganizationID == pol.OrganizationID && prev.AuthURL == pol.AuthURL && prev.Revision > pol.Revision {
			return errors.New("collection policy revision moved backwards")
		}
		// A refresh knows the ids alone; the names setup stored stay.
		if ok && prev.OrganizationID == pol.OrganizationID {
			pol.OrganizationName = cmp.Or(pol.OrganizationName, prev.OrganizationName)
			if prev.TeamID == pol.TeamID {
				pol.TeamName = cmp.Or(pol.TeamName, prev.TeamName)
			}
		}
		return config.WritePolicy(cfg.StateDir, *pol)
	})
	if err != nil {
		return err
	}
	// Only a profile that records something new is rewritten: refreshes leave config.json
	// alone, and another organization's policy never selects the profile's team.
	selects := profile.Team != pol.TeamID && (profile.Team == "" || cfg.Policy.TeamID == pol.TeamID)
	if profile.OrganizationID != "" && (profile.OrganizationID != cfg.OrganizationID || !selects) {
		return nil
	}
	return config.UpdateProfile(cfg.Dir, cfg.ProfileName, func(p *config.Profile) {
		if p.OrganizationID == "" && cfg.OrganizationID != "" {
			p.SelectOrganization(cfg.OrganizationID, "")
		}
		// Signed into another organization while this fetch ran: its selection stands.
		if p.OrganizationID == cfg.OrganizationID && (p.Team == "" || cfg.Policy.TeamID == pol.TeamID) {
			p.Team = pol.TeamID
		}
	})
}

// ValidatedPolicy is team's last validated policy for cfg's environment; an unreadable
// one grants nothing.
func ValidatedPolicy(cfg *config.Config, team string) (config.Policy, bool) {
	cached, ok, err := config.ReadPolicy(cfg.StateDir, team)
	if err != nil {
		return config.Policy{}, false
	}
	if ok && !cached.SameEnvironment(cfg.AuthURL) {
		ok = false
	}
	if !ok && cfg.Policy.TeamID == team && cfg.Policy.SameEnvironment(cfg.AuthURL) && !cfg.Policy.FetchedAt.IsZero() {
		cached, ok = cfg.Policy, true
	}
	return cached, ok
}

// Collection is every policy this machine collects under, as hooks ask them: the
// repository-mode policies, the selected team's first and the rest in team order, then
// the one global policy honoured. Only validated policies of cfg's environment count; an
// expired one is kept, since hooks run on the last validated policy and what leaves is
// decided downstream.
//
// One global policy is honoured: the selected team's, else the only one among the rest.
// Two teams that each collect everything would both claim every session, so when neither
// is selected neither is honoured (Conflicts), until one is narrowed in the Terma web app.
func Collection(cfg *config.Config) config.Policies {
	return collection(cfg, false)
}

// Conflicts are the global policies Collection does not honour: another team's while the
// selected team collects everything, or every one when two unselected teams do.
func Conflicts(cfg *config.Config) config.Policies {
	return collection(cfg, true)
}

func collection(cfg *config.Config, conflicts bool) config.Policies {
	selected := cfg.Policy.InForce(cfg.OrganizationID, cfg.AuthURL)
	stored, _ := config.ListPolicies(cfg.StateDir)
	var repos, globals config.Policies
	if selected.Validated() && !selected.Global() {
		repos = append(repos, selected)
	}
	for _, p := range stored {
		if !p.Validated() || !p.SameEnvironment(cfg.AuthURL) || p.TeamID == selected.TeamID {
			continue
		}
		if p.Global() {
			globals = append(globals, p)
		} else {
			repos = append(repos, p)
		}
	}
	slices.SortStableFunc(globals, func(a, b config.Policy) int { return cmp.Compare(a.TeamID, b.TeamID) })
	var honoured config.Policies
	switch {
	case selected.Validated() && selected.Global():
		honoured = config.Policies{selected}
	case len(globals) == 1:
		honoured, globals = globals, nil
	}
	if conflicts {
		return globals
	}
	return append(repos, honoured...)
}

// ScopeToTeam is cfg as a fetch, mint or refresh for team runs it: that team as the
// project, under the organization whose policy for it is stored, so a team of another
// organization than the profile's is served with that organization's credential.
func ScopeToTeam(cfg *config.Config, team string) *config.Config {
	scoped := *cfg
	scoped.ProjectID = team
	if stored, ok, err := config.ReadPolicy(cfg.StateDir, team); err == nil && ok && stored.SameEnvironment(cfg.AuthURL) && stored.OrganizationID != "" {
		scoped.OrganizationID = stored.OrganizationID
		scoped.OrganizationName = stored.OrganizationName
		scoped.ProjectName = stored.TeamName
	}
	return &scoped
}
