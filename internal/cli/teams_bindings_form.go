package cli

import (
	"cmp"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

// slotKind is what a line of the bindings checklist decides.
type slotKind int

const (
	// slotKeys binds its needs' keys to their organization: one key, or an owner's listed
	// repositories in one line when there are many.
	slotKeys slotKind = iota
	// slotListedOnly keeps an exact-only team's owner to its listed repositories (the default).
	slotListedOnly
	// slotWholeOwner binds that owner whole, in place of its listed repositories.
	slotWholeOwner
	// slotConflict binds a key more than one organization needs to one of them.
	slotConflict
	// slotLater leaves such a key to decide.
	slotLater
)

// bindingSlot is one line of the bindings checklist.
type bindingSlot struct {
	kind       slotKind
	needs      []int
	owner, key string
	org        string
}

// collapseAt is how many of an owner's listed repositories one organization needs before
// the checklist shows them as one line, so a long policy still fits on a terminal.
const collapseAt = 8

// conflictHeading heads the keys more than one organization needs.
const conflictHeading = "Needed by more than one organization: pick one, or decide later"

// bindingsForm is shown, the needs a run asks about, as lines: each organization's keys,
// ticked, except an owner whose binding would give away another organization's need under
// it that the form's defaults leave undecided, in red and unticked; an exact-only team's
// owner with two or more listed repositories as a pick between them (the default) and the
// whole owner; then, in red, each key more than one organization needs, to bind to one of
// them or leave to decide, which it starts on. all is every need left to decide, indexed
// by ix, for the conflicts and owners shown ones touch.
func bindingsForm(shown, all []bindingNeed, ix routing.Needs) (*prompt.Form, []bindingSlot) {
	form := &prompt.Form{Title: "Bind these to the organizations that need them; one left unticked waits, holding what it matches (`terma teams --refuse <key>` refuses one):"}
	var slots []bindingSlot
	add := func(it prompt.Item, s bindingSlot) {
		form.Items = append(form.Items, it)
		slots = append(slots, s)
	}
	names := orgNames(all)
	conflictKeys, conflicted := conflicts(all, ix)
	// What Enter decides: every shown key no other organization needs.
	defaults := map[string]bool{}
	var orgs []string
	seenOrg := map[string]bool{}
	for _, n := range shown {
		if ix.Conflict(n.key) {
			continue
		}
		defaults[n.key] = true
		if !seenOrg[n.org] {
			seenOrg[n.org] = true
			orgs = append(orgs, n.org)
		}
	}
	group := 0
	for _, org := range orgs {
		heading := ""
		head := func(name string) string {
			if heading != "" {
				return ""
			}
			heading = name
			return name
		}
		// Each key once, its repositories grouped by owner for collapsing.
		var keys []int
		seen := map[string]bool{}
		byOwner := map[string][]int{}
		var owners []string
		for i, n := range shown {
			if n.org != org || ix.Conflict(n.key) || seen[n.key] {
				continue
			}
			seen[n.key] = true
			keys = append(keys, i)
			if o := routing.Owner(n.key); o != "" {
				if byOwner[o] == nil {
					owners = append(owners, o)
				}
				byOwner[o] = append(byOwner[o], i)
			}
		}
		for _, i := range keys {
			n := shown[i]
			if o := routing.Owner(n.key); o != "" && len(byOwner[o]) > collapseAt {
				continue
			}
			item := prompt.Item{Label: n.key, Detail: "listed by " + teamEntry(n.key, n.entry, n.teamName) + exceptionNote(ix, names, n),
				Kind: prompt.Check, Selected: true, Heading: head(n.orgName)}
			if s, ok := ix.Settles(n.key, org, func(k string) bool { return defaults[k] }); ok {
				item.Selected, item.Alert = false, true
				item.Detail = "binding it gives " + s.Key + ", which " + cmp.Or(names[s.Organization], s.Organization) + " needs, to " + n.orgName + ": decide that first"
			}
			add(item, bindingSlot{kind: slotKeys, needs: []int{i}, org: org})
		}
		for _, o := range owners {
			ids := byOwner[o]
			first := shown[ids[0]]
			if len(ids) > collapseAt {
				add(prompt.Item{Label: fmt.Sprintf("%s: its %d listed repositories", o, len(ids)), Detail: "listed by " + first.teamName,
					Kind: prompt.Check, Selected: true, Heading: head(first.orgName)}, bindingSlot{kind: slotKeys, needs: ids, org: org})
			}
			if first.owner == "" || len(ids) < 2 || ix.Shared(o, org) {
				continue
			}
			group++
			add(prompt.Item{Label: "only the listed repositories under " + o, Detail: "the owner's others stay free: personal may collect them",
				Kind: prompt.Radio, Group: group, Selected: true, Heading: head(first.orgName)}, bindingSlot{kind: slotListedOnly, owner: o, org: org})
			add(prompt.Item{Label: "the whole owner " + o, Detail: "every repository under it belongs to " + first.orgName,
				Kind: prompt.Radio, Group: group}, bindingSlot{kind: slotWholeOwner, needs: ids, owner: o, org: org})
		}
	}
	shownKeys := make(map[string]bool, len(shown))
	for _, n := range shown {
		shownKeys[n.key] = true
	}
	heading := conflictHeading
	for _, key := range conflictKeys {
		if !shownKeys[key] {
			continue
		}
		group++
		for _, n := range conflicted[key] {
			add(prompt.Item{Label: "bind " + key + " to " + n.orgName, Detail: "listed by " + teamEntry(n.key, n.entry, n.teamName),
				Kind: prompt.Radio, Group: group, Alert: true, Heading: heading}, bindingSlot{kind: slotConflict, key: key, org: n.org})
			heading = ""
		}
		add(prompt.Item{Label: key + ": decide later", Detail: "what it matches is held", Kind: prompt.Radio, Group: group, Selected: true, Alert: true},
			bindingSlot{kind: slotLater, key: key})
	}
	return form, slots
}

// exceptionNote says, for a repository whose owner another organization needs, that
// binding it makes it an exception to that owner.
func exceptionNote(ix routing.Needs, names map[string]string, n bindingNeed) string {
	if org, ok := ix.OwnerElsewhere(n.key, n.org); ok {
		return "; an exception to " + routing.Owner(n.key) + ", which " + cmp.Or(names[org], org) + " needs"
	}
	return ""
}

// formDecisions are the bindings the checklist's answer makes: a whole owner picked binds
// it in place of its listed repositories.
func formDecisions(shown []bindingNeed, slots []bindingSlot, result []prompt.Item) []decision {
	var out []decision
	whole, seen := map[decision]bool{}, map[decision]bool{}
	for k, s := range slots {
		if s.kind == slotWholeOwner && result[k].Selected {
			whole[decision{s.owner, s.org}], seen[decision{s.owner, s.org}] = true, true
			out = append(out, decision{s.owner, s.org})
		}
	}
	for k, s := range slots {
		if !result[k].Selected {
			continue
		}
		switch s.kind {
		case slotKeys:
			for _, i := range s.needs {
				n := shown[i]
				if d := (decision{n.key, n.org}); !whole[decision{routing.Owner(n.key), n.org}] && !seen[d] {
					seen[d] = true
					out = append(out, d)
				}
			}
		case slotConflict:
			out = append(out, decision{s.key, s.org})
		}
	}
	return out
}
