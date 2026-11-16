package routing

import "slices"

// Need is a binding an organization's entry needs that no decision covers.
type Need struct{ Key, Organization string }

// Needs indexes the bindings left to decide for the rules about deciding them, which
// `terma teams`, `terma teams --bind` and doctor share: a key more than one organization
// needs is a conflict only the developer settles, and an owner bound while another
// organization needs a repository under it gives that repository away (Route decides a
// repository with no decision of its own by its owner's).
type Needs struct {
	// orgs are each key's organizations, in the order first named.
	orgs map[string][]string
	// under are each owner's needed repositories, in the order first named.
	under map[string][]string
}

// IndexNeeds indexes needs, their keys normalised as Entry.Key gives them, working out
// each key's owner once.
func IndexNeeds(needs []Need) Needs {
	ix := Needs{orgs: make(map[string][]string, len(needs)), under: map[string][]string{}}
	for _, n := range needs {
		orgs, seen := ix.orgs[n.Key]
		if slices.Contains(orgs, n.Organization) {
			continue
		}
		if !seen && validRepo(n.Key) {
			ix.under[owner(n.Key)] = append(ix.under[owner(n.Key)], n.Key)
		}
		ix.orgs[n.Key] = append(orgs, n.Organization)
	}
	return ix
}

// Organizations are the organizations that need key, in the order first named.
func (ix Needs) Organizations(key string) []string { return ix.orgs[key] }

// Conflict reports whether more than one organization needs key.
func (ix Needs) Conflict(key string) bool { return len(ix.orgs[key]) > 1 }

// Settles finds a repository that binding the owner key to org would hand to org though
// another organization needs it: Route gives a repository with no decision of its own to
// its owner's organization. A repository that decided reports as decided (decided may be
// nil) gets a decision of its own, so binding the owner takes nothing from it. ok is false
// when there is no such repository, and always for a repository key.
func (ix Needs) Settles(key, org string, decided func(key string) bool) (n Need, ok bool) {
	for _, repo := range ix.under[key] {
		if decided != nil && decided(repo) {
			continue
		}
		if i := slices.IndexFunc(ix.orgs[repo], func(o string) bool { return o != org }); i >= 0 {
			return Need{Key: repo, Organization: ix.orgs[repo][i]}, true
		}
	}
	return Need{}, false
}

// Shared reports whether an organization other than org needs ownerKey itself or a
// repository under it: binding the owner whole to org would take what that one needs.
func (ix Needs) Shared(ownerKey, org string) bool {
	other := func(o string) bool { return o != org }
	if slices.ContainsFunc(ix.orgs[ownerKey], other) {
		return true
	}
	return slices.ContainsFunc(ix.under[ownerKey], func(repo string) bool { return slices.ContainsFunc(ix.orgs[repo], other) })
}

// OwnerElsewhere is another organization that needs key's owner, ok false when none does
// or key is an owner: binding the repository makes it an exception to that owner.
func (ix Needs) OwnerElsewhere(key, org string) (string, bool) {
	if !validRepo(key) {
		return "", false
	}
	o := owner(key)
	if i := slices.IndexFunc(ix.orgs[o], func(x string) bool { return x != org }); i >= 0 {
		return ix.orgs[o][i], true
	}
	return "", false
}

// OwnersTaking checks keys against s, the routing state a run leaves: each owner among them
// bound to an organization that takes a repository another organization's entry needs.
// Such a repository has no decision of its own, so Route decides it by its owner's
// binding. Each owner maps to the first need it takes. It reads the bindings as Route
// does, so it never disagrees with where a record goes; a repository key takes nothing.
func (s State) OwnersTaking(keys []string) map[string]Need {
	boundTo := map[string]string{}
	for _, k := range keys {
		if b, ok := s.Bindings[k]; ok && !b.Refused && isOwnerKey(k) {
			boundTo[k] = b.Organization
		}
	}
	taken := map[string]Need{}
	if len(boundTo) == 0 {
		return taken
	}
	for _, t := range s.Teams {
		if t.Personal || t.Policy.Global() || t.Policy.Unset {
			continue
		}
		for _, raw := range t.Policy.Repositories {
			e, ok := ParseEntry(raw, t.Policy.Wildcards)
			if !ok || e.wildcard {
				continue
			}
			o := owner(e.key)
			org, bound := boundTo[o]
			if _, found := taken[o]; !bound || found || org == t.OrganizationID {
				continue
			}
			if s.decided(e.key).Key == o {
				taken[o] = Need{Key: e.key, Organization: t.OrganizationID}
			}
		}
	}
	return taken
}
