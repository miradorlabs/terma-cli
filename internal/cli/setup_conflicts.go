package cli

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

type conflictSeverity int

const (
	// conflictPending means setup left a conflicting binding undecided so matching sessions
	// are held until the developer chooses an organization with `terma teams`.
	conflictPending conflictSeverity = iota
	// conflictNotice means routing has a deterministic outcome (preserving an existing
	// developer decision, or routing a same-org overlap to the first team), which the
	// developer should know about.
	conflictNotice
)

type policyConflict struct {
	severity conflictSeverity
	subject  string
	summary  string
	nextStep string
	options  []conflictOption
}

type parsedClaim struct {
	orgID    string
	teamID   string
	entry    routing.Entry
	key      string
	owner    string
	pattern  string
	incoming bool
}

// planSetupBindings automatically binds every unambiguous repository/owner listed by
// targets' policies without prompting, preserves all existing developer decisions, and
// reports every policy conflict (cross-org overlaps, conflicts with existing bindings or
// refusals, and same-org duplicate policy entries).
func planSetupBindings(targets []discoveredTeam, existingTeams []routing.Team, existingBindings map[string]routing.Binding, names teamNames) ([]bindingChoice, []policyConflict) {
	orgNames := maps.Clone(names.Orgs)
	if orgNames == nil {
		orgNames = map[string]string{}
	}
	tNames := maps.Clone(names.Teams)
	if tNames == nil {
		tNames = map[string]string{}
	}
	for _, dt := range targets {
		if dt.orgName != "" {
			orgNames[dt.team.OrganizationID] = dt.orgName
		}
		if dt.teamName != "" {
			tNames[dt.team.TeamID] = dt.teamName
		}
	}
	orgLabel := func(id string) string { return cmp.Or(orgNames[id], id) }
	teamDisplay := func(orgID, teamID string) string {
		return orgTeamLabel(orgNames[orgID], cmp.Or(tNames[teamID], teamID))
	}

	claims := collectSetupClaims(targets, existingTeams)
	if len(claims) == 0 {
		return nil, nil
	}

	var conflicts []policyConflict
	seenExisting := map[string]bool{}
	conflictedOwner := map[string]bool{}
	conflictedExact := map[string]bool{}

	// 1. Check incoming claims against existing developer decisions (never overwrite them).
	for _, c := range claims {
		if !c.incoming {
			continue
		}
		conflict, pendingOwner := checkExistingDecisionConflict(c, existingBindings, orgLabel, teamDisplay)
		if pendingOwner {
			conflictedOwner[c.owner] = true
		}
		if conflict != nil {
			dedup := c.orgID + "\x00" + c.key + "\x00" + conflict.summary
			if !seenExisting[dedup] {
				seenExisting[dedup] = true
				conflicts = append(conflicts, *conflict)
			}
		}
	}

	// 2. Index undecided claims across all effective teams to detect cross-org conflicts.
	wildOrgs := map[string][]string{}
	exactOrgs := map[string][]string{}
	ownerExactKeys := map[string][]string{}
	var wildOwnerOrder, exactKeyOrder []string

	for _, c := range claims {
		if alreadyDecided(existingBindings, c.entry) {
			continue
		}
		if c.entry.Wildcard() {
			if len(wildOrgs[c.owner]) == 0 {
				wildOwnerOrder = append(wildOwnerOrder, c.owner)
			}
			wildOrgs[c.owner] = appendUnique(wildOrgs[c.owner], c.orgID)
		} else {
			if len(exactOrgs[c.key]) == 0 {
				exactKeyOrder = append(exactKeyOrder, c.key)
			}
			exactOrgs[c.key] = appendUnique(exactOrgs[c.key], c.orgID)
			ownerExactKeys[c.owner] = appendUnique(ownerExactKeys[c.owner], c.key)
		}
	}

	for _, owner := range wildOwnerOrder {
		wOrgs := wildOrgs[owner]
		if len(wOrgs) > 1 {
			conflictedOwner[owner] = true
			for _, repoKey := range ownerExactKeys[owner] {
				conflictedExact[repoKey] = true
			}
			conflicts = append(conflicts, policyConflict{
				severity: conflictPending,
				subject:  owner + "/*",
				summary:  fmt.Sprintf("%s/* is claimed by multiple organizations (%s); left pending", owner, joinOrgLabels(wOrgs, orgLabel)),
				nextStep: fmt.Sprintf("Run `terma teams --bind %s=<org>` or `terma teams --refuse %s` to resolve.", owner, owner),
				options:  wildcardConflictOptions(owner, wOrgs, orgLabel),
			})
			continue
		}
		wildOrg := wOrgs[0]
		var otherOrgs, otherRepos []string
		for _, repoKey := range ownerExactKeys[owner] {
			for _, o := range exactOrgs[repoKey] {
				if o != wildOrg {
					conflictedOwner[owner] = true
					conflictedExact[repoKey] = true
					otherOrgs = appendUnique(otherOrgs, o)
					otherRepos = appendUnique(otherRepos, repoKey)
				}
			}
		}
		if len(otherOrgs) > 0 {
			conflicts = append(conflicts, policyConflict{
				severity: conflictPending,
				subject:  owner + "/*",
				summary: fmt.Sprintf("%s/* (%s) conflicts with %s (%s); left pending",
					owner, orgLabel(wildOrg), strings.Join(otherRepos, ", "), joinOrgLabels(otherOrgs, orgLabel)),
				nextStep: fmt.Sprintf("Run `terma teams --bind %s=<org>` or `terma teams --bind <repo>=<org>` to resolve.", owner),
				options:  wildcardVsExactOptions(owner, wildOrg, otherRepos, exactOrgs, orgLabel),
			})
		}
	}

	for _, repoKey := range exactKeyOrder {
		if conflictedExact[repoKey] {
			continue
		}
		orgs := exactOrgs[repoKey]
		if len(orgs) > 1 {
			conflictedExact[repoKey] = true
			conflicts = append(conflicts, policyConflict{
				severity: conflictPending,
				subject:  repoKey,
				summary:  fmt.Sprintf("%s is claimed by multiple organizations (%s); left pending", repoKey, joinOrgLabels(orgs, orgLabel)),
				nextStep: fmt.Sprintf("Run `terma teams --bind %s=<org>` or `terma teams --refuse %s` to resolve.", repoKey, repoKey),
				options:  exactConflictOptions(repoKey, orgs, orgLabel),
			})
		}
	}

	// 3. Emit automatic bindings for every non-conflicting incoming claim (owners first).
	ownerBoundOrg := map[string]string{}
	var ownerChoices, repoChoices []bindingChoice
	seenChoice := map[string]bool{}

	for _, c := range claims {
		if !c.incoming || !c.entry.Wildcard() || alreadyDecided(existingBindings, c.entry) || conflictedOwner[c.owner] {
			continue
		}
		if !seenChoice[c.owner] {
			seenChoice[c.owner] = true
			ownerBoundOrg[c.owner] = c.orgID
			ownerChoices = append(ownerChoices, bindingChoice{key: c.owner, org: c.orgID})
		}
	}
	for _, c := range claims {
		if !c.incoming || c.entry.Wildcard() || alreadyDecided(existingBindings, c.entry) || conflictedExact[c.key] {
			continue
		}
		if ownerBoundOrg[c.owner] == c.orgID {
			continue
		}
		if !seenChoice[c.key] {
			seenChoice[c.key] = true
			repoChoices = append(repoChoices, bindingChoice{key: c.key, org: c.orgID})
		}
	}

	// 4. Detect same-org duplicate policy patterns across distinct teams.
	type orgPattern struct{ orgID, pattern string }
	var patternOrder []orgPattern
	patternTeams := map[orgPattern][]string{}
	for _, c := range claims {
		op := orgPattern{c.orgID, c.pattern}
		if len(patternTeams[op]) == 0 {
			patternOrder = append(patternOrder, op)
		}
		patternTeams[op] = appendUnique(patternTeams[op], c.teamID)
	}
	for _, op := range patternOrder {
		teams := patternTeams[op]
		if len(teams) <= 1 {
			continue
		}
		labels := make([]string, 0, len(teams))
		for _, id := range teams {
			labels = append(labels, cmp.Or(tNames[id], id))
		}
		conflicts = append(conflicts, policyConflict{
			severity: conflictNotice,
			summary: fmt.Sprintf("%s is listed by multiple teams in %s (%s); sessions will route to %s",
				op.pattern, orgLabel(op.orgID), strings.Join(labels, ", "), labels[0]),
			nextStep: "Run `terma teams` to adjust which teams are active on this machine.",
		})
	}

	return append(ownerChoices, repoChoices...), conflicts
}

func collectSetupClaims(targets []discoveredTeam, existingTeams []routing.Team) []parsedClaim {
	var claims []parsedClaim
	incomingTeams := map[string]bool{}
	addTeam := func(t routing.Team, incoming bool) {
		if t.Personal || t.Policy.Global() || t.Policy.Unset || len(t.Policy.Repositories) == 0 {
			return
		}
		seenPattern := map[string]bool{}
		for _, raw := range t.Policy.Repositories {
			e, ok := routing.ParseEntry(raw, t.Policy.Wildcards)
			if !ok {
				continue
			}
			pattern := config.Normalize(e.Raw)
			if pattern == "" || seenPattern[pattern] {
				continue
			}
			seenPattern[pattern] = true
			owner := e.Key()
			if !e.Wildcard() {
				owner = routing.Owner(e.Key())
			}
			claims = append(claims, parsedClaim{
				orgID:    t.OrganizationID,
				teamID:   t.TeamID,
				entry:    e,
				key:      e.Key(),
				owner:    owner,
				pattern:  pattern,
				incoming: incoming,
			})
		}
	}
	for _, dt := range targets {
		incomingTeams[dt.team.TeamID] = true
		addTeam(dt.team, true)
	}
	for _, t := range existingTeams {
		if incomingTeams[t.TeamID] {
			continue
		}
		addTeam(t, false)
	}
	return claims
}

func checkExistingDecisionConflict(c parsedClaim, existing map[string]routing.Binding, orgLabel func(string) string, teamDisplay func(string, string) string) (*policyConflict, bool) {
	if len(existing) == 0 {
		return nil, false
	}
	teamName := teamDisplay(c.orgID, c.teamID)
	if !c.entry.Wildcard() {
		if b, ok := existing[c.key]; ok {
			if b.Refused {
				return &policyConflict{
					severity: conflictNotice,
					summary:  fmt.Sprintf("%s is refused on this machine, so %s will not collect it", c.key, teamName),
					nextStep: "Run `terma teams` to review or change repository bindings.",
				}, false
			}
			if b.Organization != c.orgID {
				return &policyConflict{
					severity: conflictNotice,
					summary:  fmt.Sprintf("%s is already bound to %s, so %s will not collect it", c.key, orgLabel(b.Organization), teamName),
					nextStep: "Run `terma teams` to review or change repository bindings.",
				}, false
			}
			return nil, false
		}
		if ob, ok := existing[c.owner]; ok {
			if ob.Refused {
				return &policyConflict{
					severity: conflictNotice,
					summary:  fmt.Sprintf("%s is under %s (refused on this machine), so %s will not collect it", c.key, c.owner, teamName),
					nextStep: "Run `terma teams` to review or change repository bindings.",
				}, false
			}
			if ob.Organization != c.orgID {
				return &policyConflict{
					severity: conflictNotice,
					summary:  fmt.Sprintf("%s is under %s (bound to %s), so %s will not collect it", c.key, c.owner, orgLabel(ob.Organization), teamName),
					nextStep: "Run `terma teams` to review or change repository bindings.",
				}, false
			}
		}
		return nil, false
	}
	if ob, ok := existing[c.owner]; ok {
		if ob.Refused {
			return &policyConflict{
				severity: conflictNotice,
				summary:  fmt.Sprintf("%s/* is refused on this machine, so %s will not collect it", c.owner, teamName),
				nextStep: "Run `terma teams` to review or change repository bindings.",
			}, false
		}
		if ob.Organization != c.orgID {
			return &policyConflict{
				severity: conflictNotice,
				summary:  fmt.Sprintf("%s/* conflicts with %s already bound to %s, so %s will not collect it", c.owner, c.owner, orgLabel(ob.Organization), teamName),
				nextStep: "Run `terma teams` to review or change repository bindings.",
			}, false
		}
		return nil, false
	}
	var conflictingChildren []string
	for _, k := range slices.Sorted(maps.Keys(existing)) {
		if !config.IsRepositoryKey(k) || routing.Owner(k) != c.owner {
			continue
		}
		b := existing[k]
		if b.Refused || b.Organization != c.orgID {
			conflictingChildren = append(conflictingChildren, k)
		}
	}
	if len(conflictingChildren) > 0 {
		return &policyConflict{
			severity: conflictPending,
			subject:  c.owner + "/*",
			summary: fmt.Sprintf("%s/* (%s) conflicts with existing decisions under %s (%s); left pending",
				c.owner, orgLabel(c.orgID), c.owner, strings.Join(conflictingChildren, ", ")),
			nextStep: fmt.Sprintf("Run `terma teams --bind %s=<org>` or `terma teams --refuse %s` to resolve.", c.owner, c.owner),
			options:  wildcardVsExistingOptions(c.owner, c.orgID, conflictingChildren, orgLabel),
		}, true
	}
	return nil, false
}

func appendUnique(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

func joinOrgLabels(orgIDs []string, orgLabel func(string) string) string {
	out := make([]string, 0, len(orgIDs))
	for _, id := range orgIDs {
		out = append(out, orgLabel(id))
	}
	return strings.Join(out, ", ")
}
