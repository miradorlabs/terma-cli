// Package routing keeps each team's collection policy in the state directory
// (config.PoliciesDir), the one copy hooks, doctor, the relay and delivery read; a
// profile records only which team it selected in each organization. One machine collects
// for several organizations at once: the selected team of each (Collection), whose policy
// names the organization that fetched it, whose credential refreshes it.
package routing

import (
	"cmp"
	"errors"
	"io/fs"
	"maps"
	"os"
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
	left := ""
	err = config.UpdateProfile(cfg.Dir, cfg.ProfileName, func(p *config.Profile) {
		if p.OrganizationID == "" && cfg.OrganizationID != "" {
			p.SelectOrganization(cfg.OrganizationID, "")
		}
		// Signed into another organization while this fetch ran: its selection stands.
		if p.OrganizationID == cfg.OrganizationID && (p.Team == "" || cfg.Policy.TeamID == pol.TeamID) {
			prev := p.Team
			p.SelectTeam(pol.TeamID)
			if prev != "" && prev != pol.TeamID && !slices.Contains(slices.Collect(maps.Values(p.CollectedTeams())), prev) {
				left = prev
			}
		}
	})
	if err != nil || left == "" {
		return err
	}
	// The team selected before, now selected in no organization, collects nothing: its
	// policy file would only be refreshed for nothing.
	if prevPath, err := config.PolicyPath(cfg.StateDir, left); err == nil {
		if err := os.Remove(prevPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
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

// Collected is what one pass over the profile's teams found: the policies this machine
// collects under, the global policies it does not honour, and the teams with no validated
// policy.
type Collected struct {
	// Policies are the collection, as hooks ask them: the repository-mode policies, the
	// selected team's first and the rest in team order, then the one global policy honoured.
	Policies config.Policies
	// Conflicts are the global policies not honoured: another team's while the selected
	// team collects everything, or every one when two unselected teams do.
	Conflicts config.Policies
	// Unvalidated are the teams selected whose policy is not stored, unreadable, or of
	// another environment or organization than recorded: each collects nothing until
	// `terma setup` fetches it again. Only their ids are known.
	Unvalidated []config.Policy
}

// Collect reads the policies of the teams the profile selected (config.Config.Teams, one
// per organization set up). Only validated policies of cfg's environment count; an expired
// one is kept, since hooks run on the last validated policy and what leaves is decided
// downstream. A policy file of a team selected nowhere does not count.
//
// One global policy is honoured: the selected team's, else the only one among the rest.
// Two teams that each collect everything would both claim every session, so when neither
// is selected neither is honoured, until one is narrowed in the Terma web app.
func Collect(cfg *config.Config) Collected {
	selected := cfg.Policy.InForce(cfg.OrganizationID, cfg.AuthURL)
	var c Collected
	var repos, globals config.Policies
	if selected.Validated() && !selected.Global() {
		repos = append(repos, selected)
	}
	pinned := len(repos) // the selected team's listing stays first; the rest sort by team
	for _, org := range slices.Sorted(maps.Keys(cfg.Teams)) {
		team := cfg.Teams[org]
		if team == selected.TeamID {
			if !selected.Validated() {
				c.Unvalidated = append(c.Unvalidated, config.Policy{TeamID: team, OrganizationID: org})
			}
			continue // config.Load read the selected team's policy already
		}
		p, ok, err := config.ReadPolicy(cfg.StateDir, team)
		switch {
		case err != nil || !ok || !p.Validated() || !p.SameEnvironment(cfg.AuthURL) || p.OrganizationID != org:
			c.Unvalidated = append(c.Unvalidated, config.Policy{TeamID: team, OrganizationID: org})
		case p.Global():
			globals = append(globals, p)
		default:
			repos = append(repos, p)
		}
	}
	slices.SortStableFunc(repos[pinned:], func(a, b config.Policy) int { return cmp.Compare(a.TeamID, b.TeamID) })
	slices.SortStableFunc(globals, func(a, b config.Policy) int { return cmp.Compare(a.TeamID, b.TeamID) })
	switch {
	case selected.Validated() && selected.Global():
		repos = append(repos, selected)
	case len(globals) == 1:
		repos, globals = append(repos, globals[0]), nil
	}
	c.Policies, c.Conflicts = repos, globals
	return c
}

// Collection is the policies this machine collects under (Collected.Policies).
func Collection(cfg *config.Config) config.Policies { return Collect(cfg).Policies }

// ScopeToTeam is cfg as a fetch, mint or refresh for team runs it: that team as the
// project, under the organization the profile selected it in (config.Config.Teams), else
// the one whose policy for it is stored, else the profile's own. A team of another
// organization than the profile's is so served with that organization's credential.
func ScopeToTeam(cfg *config.Config, team string) *config.Config {
	scoped := *cfg
	scoped.ProjectID, scoped.ProjectName = team, ""
	known := false
	for _, org := range slices.Sorted(maps.Keys(cfg.Teams)) {
		if cfg.Teams[org] == team {
			scoped.OrganizationID, known = org, true
			break
		}
	}
	stored, ok, err := config.ReadPolicy(cfg.StateDir, team)
	if err != nil || !ok || !stored.SameEnvironment(cfg.AuthURL) || known && stored.OrganizationID != scoped.OrganizationID {
		if known && scoped.OrganizationID != cfg.OrganizationID {
			scoped.OrganizationName = ""
		}
		return &scoped
	}
	if !known && stored.OrganizationID != "" {
		scoped.OrganizationID = stored.OrganizationID
	}
	if scoped.OrganizationID != cfg.OrganizationID || stored.OrganizationName != "" {
		scoped.OrganizationName = stored.OrganizationName
	}
	scoped.ProjectName = stored.TeamName
	return &scoped
}
