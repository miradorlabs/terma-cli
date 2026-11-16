package doctor

import (
	"strings"

	"github.com/miradorlabs/terma-cli/internal/routing"
)

// PendingBinding is a binding a ticked organization's entry needs that no one decided.
type PendingBinding struct {
	Key, Organization, TeamID, Entry string
}

// Pending lists the bindings st's ticked organizations' entries need with no decision
// covering them, once per key and organization: a key two organizations need is listed
// for each, a conflict only the developer can settle.
func Pending(st routing.State) []PendingBinding {
	var out []PendingBinding
	seen := map[routing.Need]bool{}
	for _, t := range st.Teams {
		if t.Personal || t.Policy.Global() || t.Policy.Unset {
			continue
		}
		for _, raw := range t.Policy.Repositories {
			e, ok := routing.ParseEntry(raw, t.Policy.Wildcards)
			if !ok || seen[routing.Need{Key: e.Key(), Organization: t.OrganizationID}] || decided(st, e) {
				continue
			}
			seen[routing.Need{Key: e.Key(), Organization: t.OrganizationID}] = true
			out = append(out, PendingBinding{Key: e.Key(), Organization: t.OrganizationID, TeamID: t.TeamID, Entry: raw})
		}
	}
	return out
}

// IndexPending indexes pending for the binding rules (routing.Needs).
func IndexPending(pending []PendingBinding) routing.Needs {
	needs := make([]routing.Need, 0, len(pending))
	for _, p := range pending {
		needs = append(needs, routing.Need{Key: p.Key, Organization: p.Organization})
	}
	return routing.IndexNeeds(needs)
}

// PendingKey is one key left to decide, with every organization whose entry needs it.
type PendingKey struct {
	Key   string
	Needs []PendingBinding
	// Waits is, for an owner one organization needs, a repository under it that another
	// organization needs: binding the owner first would give that repository away, so it is
	// decided first. Its Key is "" when there is none.
	Waits routing.Need
}

// Conflict reports whether more than one organization needs the key: only the developer
// can say whose it is.
func (k PendingKey) Conflict() bool { return len(k.Needs) > 1 }

// PendingKeys is Pending grouped by key, in the order Pending first names each.
func PendingKeys(st routing.State) []PendingKey {
	pending := Pending(st)
	ix := IndexPending(pending)
	var out []PendingKey
	at := map[string]int{}
	for _, p := range pending {
		if i, ok := at[p.Key]; ok {
			out[i].Needs = append(out[i].Needs, p)
			continue
		}
		at[p.Key] = len(out)
		k := PendingKey{Key: p.Key, Needs: []PendingBinding{p}}
		if !ix.Conflict(p.Key) {
			k.Waits, _ = ix.Settles(p.Key, p.Organization, nil)
		}
		out = append(out, k)
	}
	return out
}

// JoinAnd joins words as a sentence lists them: "a", "a and b", "a, b and c".
func JoinAnd(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// decided reports whether a decision covers what e needs: its key's, or for an exact
// entry its owner's, whichever organization it names.
func decided(st routing.State, e routing.Entry) bool {
	_, _, ok := st.Decision(e)
	return ok
}
