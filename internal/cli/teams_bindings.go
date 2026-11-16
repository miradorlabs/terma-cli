package cli

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

// bindingNeed is a binding a ticked organization's entry needs that no decision covers: for
// a team ticked in this run (tick), or for one ticked before (tick nil).
type bindingNeed struct {
	key, org, orgName, entry string
	// teamName is the team whose policy lists the entry.
	teamName string
	// owner is the owner of a repository a team lists among exact entries only: it may be
	// bound whole in place of its listed repositories.
	owner string
	tick  *tick
}

// neededOp is a decision about a need, made with its tick when it has one, so a tick that
// fails takes it back.
type neededOp struct {
	op      bindingOp
	orgName string
	tick    *tick
}

// bindingSlot is what a line of the bindings form decides: a need's key (need), an owner
// bound whole (owner), or a conflicting key bound to one organization (key, org; with no
// org, left to decide).
type bindingSlot struct {
	need         int
	owner, key   string
	org, orgName string
	tick         *tick
}

// conflictHeading heads the keys more than one organization needs.
const conflictHeading = "Needed by more than one organization: pick one, or decide later"

// tickNeeds are the bindings tk's policy needs with no decision in st. An entry under an
// owner decided otherwise is skipped, and said in tk's notes.
func tickNeeds(st routing.State, tk *tick) []bindingNeed {
	t := tk.team
	if t.Personal || t.Policy.Global() {
		return nil
	}
	var entries []routing.Entry
	exactOnly := true
	for _, raw := range t.Policy.Repositories {
		e, ok := routing.ParseEntry(raw, t.Policy.Wildcards)
		if !ok || slices.ContainsFunc(entries, func(n routing.Entry) bool { return n.Key() == e.Key() }) {
			continue
		}
		exactOnly = exactOnly && !e.Wildcard()
		switch who, decided := decisionFor(st, e); {
		case decided && who == t.OrganizationID:
		case decided:
			tk.notes = append(tk.notes, raw+" is skipped: "+who)
		default:
			entries = append(entries, e)
		}
	}
	needs := make([]bindingNeed, 0, len(entries))
	for _, e := range entries {
		n := bindingNeed{key: e.Key(), org: t.OrganizationID, orgName: cmp.Or(tk.orgName, t.OrganizationID), entry: e.Raw, teamName: tk.teamName, tick: tk}
		if exactOnly {
			n.owner = routing.Owner(e.Key())
		}
		needs = append(needs, n)
	}
	return needs
}

// decisionFor is the decision covering what e needs: who it is bound to (an organization
// id), or why it is skipped, and whether there is one at all.
func decisionFor(st routing.State, e routing.Entry) (string, bool) {
	keys := []string{e.Key()}
	if !e.Wildcard() {
		keys = append(keys, routing.Owner(e.Key()))
	}
	for _, k := range keys {
		if b, ok := st.Bindings[k]; ok {
			if b.Refused {
				return k + " is refused to every organization", true
			}
			return b.Organization, true
		}
	}
	return "", false
}

// pendingNeeds are the bindings the teams ticked before this run still need in st, their
// organizations and teams named by orgName and teamName.
func pendingNeeds(st routing.State, orgName, teamName func(string) string) []bindingNeed {
	var needs []bindingNeed
	for _, p := range doctor.Pending(st) {
		needs = append(needs, bindingNeed{key: p.Key, org: p.Organization, orgName: cmp.Or(orgName(p.Organization), p.Organization),
			entry: p.Entry, teamName: cmp.Or(teamName(p.TeamID), p.TeamID)})
	}
	return needs
}

// listedBy says which team needs a key, and as what entry when that is not the key itself.
func listedBy(key, entry, team string) string {
	if team == "" {
		return "needed by " + entry
	}
	if entry == key {
		return "listed by " + team
	}
	return "listed by " + team + " as " + entry
}

// conflicts are the keys more than one organization needs, each with its needs, in the
// order needs first names them.
func conflicts(needs []bindingNeed) ([]string, map[string][]bindingNeed) {
	by := map[string][]bindingNeed{}
	var keys []string
	for _, n := range needs {
		if !slices.ContainsFunc(by[n.key], func(o bindingNeed) bool { return o.org == n.org }) {
			by[n.key] = append(by[n.key], n)
		}
	}
	for _, n := range needs {
		if len(by[n.key]) > 1 && !slices.Contains(keys, n.key) {
			keys = append(keys, n.key)
		}
	}
	for k, ns := range by {
		if len(ns) < 2 {
			delete(by, k)
		}
	}
	return keys, by
}

// decideNeeds decides needs. On a terminal (ask) the bindings form shows them all, ticked;
// otherwise, with all, each key one organization needs is bound to it (an exact-only team's
// listed repositories, not their owners), and a pending need from before this run only
// with pending too. A conflict is never decided for the developer: it stays to decide.
func decideNeeds(needs []bindingNeed, ask, all, pending bool) ([]neededOp, error) {
	if len(needs) == 0 {
		return nil, nil
	}
	if ask {
		return askNeeds(needs)
	}
	if !all {
		return nil, nil
	}
	_, conflicted := conflicts(needs)
	var ops []neededOp
	for _, n := range needs {
		if conflicted[n.key] != nil || (n.tick == nil && !pending) || slices.ContainsFunc(ops, func(o neededOp) bool { return o.op.key == n.key }) {
			continue
		}
		ops = append(ops, neededOp{op: bindingOp{key: n.key, org: n.org}, orgName: n.orgName, tick: n.tick})
	}
	return ops, nil
}

// askNeeds shows the bindings form and reads its answer.
func askNeeds(needs []bindingNeed) ([]neededOp, error) {
	form, slots := bindingsForm(needs)
	result, err := prompt.Run(form)
	if errors.Is(err, prompt.ErrCancelled) {
		return nil, errCancelled
	}
	if err != nil {
		return nil, err
	}
	return formOps(needs, slots, result), nil
}

// bindingsForm is every need as a line to tick: each organization's keys, ticked, with an
// exact-only team's owners offered whole, unticked; then each key more than one organization
// needs, in red, to bind to one of them or leave to decide, which it starts on.
func bindingsForm(needs []bindingNeed) (*prompt.Form, []bindingSlot) {
	form := &prompt.Form{Title: "Bind these to the organizations that need them; one left unticked waits, holding what it matches (`terma teams --refuse <key>` refuses one):"}
	var slots []bindingSlot
	conflictKeys, conflicted := conflicts(needs)
	var orgs []string
	for _, n := range needs {
		if conflicted[n.key] == nil && !slices.Contains(orgs, n.org) {
			orgs = append(orgs, n.org)
		}
	}
	for _, org := range orgs {
		heading := ""
		var owners []string
		ownerTick, ownerName := map[string]*tick{}, map[string]string{}
		for i, n := range needs {
			if n.org != org || conflicted[n.key] != nil || slices.ContainsFunc(slots, func(s bindingSlot) bool { return s.need >= 0 && needs[s.need].key == n.key }) {
				continue
			}
			if heading == "" {
				heading = n.orgName
				form.Items = append(form.Items, prompt.Item{Label: n.key, Detail: listedBy(n.key, n.entry, n.teamName), Kind: prompt.Check, Selected: true, Heading: heading})
			} else {
				form.Items = append(form.Items, prompt.Item{Label: n.key, Detail: listedBy(n.key, n.entry, n.teamName), Kind: prompt.Check, Selected: true})
			}
			slots = append(slots, bindingSlot{need: i})
			if n.owner != "" && !slices.Contains(owners, n.owner) && !ownerShared(needs, n.owner, org) {
				owners = append(owners, n.owner)
				ownerTick[n.owner], ownerName[n.owner] = n.tick, n.orgName
			}
		}
		for _, o := range owners {
			listed := 0
			for _, n := range needs {
				if n.org == org && n.owner == o {
					listed++
				}
			}
			form.Items = append(form.Items, prompt.Item{Label: "the whole owner " + o,
				Detail: fmt.Sprintf("in place of the %d listed: every repository under it", listed), Kind: prompt.Check})
			slots = append(slots, bindingSlot{need: -1, owner: o, org: org, orgName: ownerName[o], tick: ownerTick[o]})
		}
	}
	heading := conflictHeading
	for g, key := range conflictKeys {
		for _, n := range conflicted[key] {
			form.Items = append(form.Items, prompt.Item{Label: "bind " + key + " to " + n.orgName, Detail: listedBy(n.key, n.entry, n.teamName),
				Kind: prompt.Radio, Group: g + 1, Alert: true, Heading: heading})
			heading = ""
			slots = append(slots, bindingSlot{need: -1, key: key, org: n.org, orgName: n.orgName, tick: n.tick})
		}
		form.Items = append(form.Items, prompt.Item{Label: key + ": decide later", Detail: "what it matches is held",
			Kind: prompt.Radio, Group: g + 1, Selected: true, Alert: true})
		slots = append(slots, bindingSlot{need: -1, key: key})
	}
	return form, slots
}

// ownerShared reports whether a need of an organization other than org names owner, as its
// key or its owner: binding it whole would take what that organization needs.
func ownerShared(needs []bindingNeed, owner, org string) bool {
	return slices.ContainsFunc(needs, func(n bindingNeed) bool {
		return n.org != org && (n.key == owner || n.owner == owner || routing.Owner(n.key) == owner)
	})
}

// formOps are the decisions the bindings form's answer makes: a whole owner ticked binds it
// in place of its listed repositories.
func formOps(needs []bindingNeed, slots []bindingSlot, result []prompt.Item) []neededOp {
	var ops []neededOp
	whole := map[[2]string]bool{}
	for k, s := range slots {
		if s.owner != "" && result[k].Selected {
			whole[[2]string{s.owner, s.org}] = true
			ops = append(ops, neededOp{op: bindingOp{key: s.owner, org: s.org}, orgName: s.orgName, tick: s.tick})
		}
	}
	for k, s := range slots {
		switch {
		case s.need >= 0:
			n := needs[s.need]
			if result[k].Selected && !whole[[2]string{n.owner, n.org}] {
				ops = append(ops, neededOp{op: bindingOp{key: n.key, org: n.org}, orgName: n.orgName, tick: n.tick})
			}
		case s.key != "" && s.org != "" && result[k].Selected:
			ops = append(ops, neededOp{op: bindingOp{key: s.key, org: s.org}, orgName: s.orgName, tick: s.tick})
		}
	}
	return ops
}
