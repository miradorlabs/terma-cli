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

// bindingsForm is asked, the needs a run asks about, as lines: each organization's keys,
// ticked, except an owner whose binding would give away another organization's need under
// it that the form's defaults leave undecided, in red and unticked; an exact-only team's
// owner with two or more listed repositories as a pick between them (the default) and the
// whole owner; then, in red, each key more than one organization needs, to bind to one of
// them or leave to decide, which it starts on. pending is every need left to decide,
// indexed by rules, for the conflicts and owners the asked ones touch.
func bindingsForm(asked, pending []bindingNeed, rules routing.Needs) (*prompt.Form, []bindingSlot) {
	form := &prompt.Form{Title: "Bind these to the organizations that need them; one left unticked waits, holding what it matches (`terma teams --refuse <key>` refuses one):"}
	var slots []bindingSlot
	add := func(it prompt.Item, s bindingSlot) {
		form.Items = append(form.Items, it)
		slots = append(slots, s)
	}
	names := orgNames(pending)
	conflictKeys, conflicted := conflicts(pending, rules)
	// What Enter decides: every asked key no other organization needs, grouped by
	// organization in the order first asked.
	defaults := map[string]bool{}
	var orgs []string
	byOrg := map[string][]int{}
	for i, n := range asked {
		if rules.Conflict(n.Key) {
			continue
		}
		defaults[n.Key] = true
		if byOrg[n.Organization] == nil {
			orgs = append(orgs, n.Organization)
		}
		byOrg[n.Organization] = append(byOrg[n.Organization], i)
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
		for _, i := range byOrg[org] {
			n := asked[i]
			if seen[n.Key] {
				continue
			}
			seen[n.Key] = true
			keys = append(keys, i)
			if o := routing.Owner(n.Key); o != "" {
				if byOwner[o] == nil {
					owners = append(owners, o)
				}
				byOwner[o] = append(byOwner[o], i)
			}
		}
		for _, i := range keys {
			n := asked[i]
			if o := routing.Owner(n.Key); o != "" && len(byOwner[o]) > collapseAt {
				continue
			}
			item := prompt.Item{Label: n.Key, Detail: "listed by " + teamEntry(n.Key, n.entry, n.teamName) + exceptionNote(rules, names, n),
				Kind: prompt.Check, Selected: true, Heading: head(n.orgName)}
			if s, ok := rules.Settles(n.Key, org, func(k string) bool { return defaults[k] }); ok {
				item.Selected, item.Alert = false, true
				item.Detail = "binding it gives " + s.Key + ", which " + cmp.Or(names[s.Organization], s.Organization) + " needs, to " + n.orgName + ": decide that first"
			}
			add(item, bindingSlot{kind: slotKeys, needs: []int{i}, org: org})
		}
		for _, o := range owners {
			ids := byOwner[o]
			first := asked[ids[0]]
			if len(ids) > collapseAt {
				add(prompt.Item{Label: fmt.Sprintf("%s: its %d listed repositories", o, len(ids)), Detail: "listed by " + first.teamName,
					Kind: prompt.Check, Selected: true, Heading: head(first.orgName)}, bindingSlot{kind: slotKeys, needs: ids, org: org})
			}
			if first.owner == "" || len(ids) < 2 || rules.Shared(o, org) {
				continue
			}
			group++
			add(prompt.Item{Label: "only the listed repositories under " + o, Detail: "the owner's others stay free: personal may collect them",
				Kind: prompt.Radio, Group: group, Selected: true, Heading: head(first.orgName)}, bindingSlot{kind: slotListedOnly, owner: o, org: org})
			add(prompt.Item{Label: "the whole owner " + o, Detail: "every repository under it belongs to " + first.orgName,
				Kind: prompt.Radio, Group: group}, bindingSlot{kind: slotWholeOwner, needs: ids, owner: o, org: org})
		}
	}
	askedKeys := make(map[string]bool, len(asked))
	for _, n := range asked {
		askedKeys[n.Key] = true
	}
	heading := conflictHeading
	for _, key := range conflictKeys {
		if !askedKeys[key] {
			continue
		}
		group++
		for _, n := range conflicted[key] {
			add(prompt.Item{Label: "bind " + key + " to " + n.orgName, Detail: "listed by " + teamEntry(n.Key, n.entry, n.teamName),
				Kind: prompt.Radio, Group: group, Alert: true, Heading: heading}, bindingSlot{kind: slotConflict, key: key, org: n.Organization})
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
	if org, ok := ix.OwnerElsewhere(n.Key, n.Organization); ok {
		return "; an exception to " + routing.Owner(n.Key) + ", which " + cmp.Or(names[org], org) + " needs"
	}
	return ""
}

// formDecisions are the bindings the checklist's answer makes. A whole owner picked binds
// it in place of the repository decisions its organization's ticked lines under it make:
// replaced says which, for each, so the teams that needed those hold the owner.
func formDecisions(asked []bindingNeed, slots []bindingSlot, result []prompt.Item) (decisions []decision, replaced map[decision][]decision) {
	replaced = map[decision][]decision{}
	whole, seen := map[decision]bool{}, map[decision]bool{}
	for k, s := range slots {
		if d := (decision{Key: s.owner, Organization: s.org}); s.kind == slotWholeOwner && result[k].Selected {
			whole[d], seen[d] = true, true
			decisions = append(decisions, d)
		}
	}
	for k, s := range slots {
		if !result[k].Selected {
			continue
		}
		switch s.kind {
		case slotKeys:
			for _, i := range s.needs {
				d := decision(asked[i].Need)
				if seen[d] {
					continue
				}
				seen[d] = true
				if o := (decision{Key: routing.Owner(d.Key), Organization: d.Organization}); whole[o] {
					replaced[o] = append(replaced[o], d)
					continue
				}
				decisions = append(decisions, d)
			}
		case slotConflict:
			decisions = append(decisions, decision{Key: s.key, Organization: s.org})
		}
	}
	return decisions, replaced
}
