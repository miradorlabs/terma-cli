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

// Settles is a need of another organization that binding key to org would settle against
// it, ok false when there is none: a repository under key, an owner, that the other
// organization needs and that decided, nil for none, does not say is decided with it. A
// repository key settles nothing.
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
