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

// neededOp is a decision about needs, made with its tick when it has one, so a tick that
// fails takes it back; a decision several ticks share is made with none.
type neededOp struct {
	op      bindingOp
	orgName string
	tick    *tick
}

// bindingMode is how a run decides the bindings left to decide.
type bindingMode int

const (
	// leavePending decides nothing: what an undecided entry matches is held.
	leavePending bindingMode = iota
	// bindTicked binds each key one organization needs, for the teams this run ticks (--yes).
	bindTicked
	// bindPending does the same for every key left to decide, earlier ones too (--bind-all).
	bindPending
	// askTicked shows the checklist for the teams this run ticks (--select on a terminal).
	askTicked
	// askPending shows it for every key left to decide (the `terma teams` checklist).
	askPending
)

// asks reports whether m shows the checklist.
func (m bindingMode) asks() bool { return m == askTicked || m == askPending }

// earlier reports whether m decides the keys teams ticked before this run need too.
func (m bindingMode) earlier() bool { return m == bindPending || m == askPending }

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
	kind         slotKind
	needs        []int
	owner, key   string
	org, orgName string
	tick         *tick
}

// collapseAt is how many of an owner's listed repositories one organization needs before
// the checklist shows them as one line, so a long policy still fits on a terminal.
const collapseAt = 8

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
	for _, k := range doctor.PendingKeys(st) {
		for _, p := range k.Needs {
			needs = append(needs, bindingNeed{key: p.Key, org: p.Organization, orgName: cmp.Or(orgName(p.Organization), p.Organization),
				entry: p.Entry, teamName: cmp.Or(teamName(p.TeamID), p.TeamID)})
		}
	}
	return needs
}

// teamEntry says which team lists a key, and as what entry when that is not the key itself.
func teamEntry(key, entry, team string) string {
	switch {
	case team == "":
		return entry
	case entry == key:
		return team
	}
	return team + " as " + entry
}

// conflicts are the keys more than one organization needs, each with one need per
// organization, in the order needs first names them.
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

// decideNeeds decides needs as mode says, the checklist drawn by run. A conflict is never
// decided for the developer: it stays to decide unless they pick.
func decideNeeds(needs []bindingNeed, mode bindingMode, run func(*prompt.Form) ([]prompt.Item, error)) ([]neededOp, error) {
	if len(needs) == 0 || mode == leavePending {
		return nil, nil
	}
	var ops []neededOp
	if mode.asks() {
		form, slots := bindingsForm(needs)
		result, err := run(form)
		if errors.Is(err, prompt.ErrCancelled) {
			return nil, errCancelled
		}
		if err != nil {
			return nil, err
		}
		ops = formOps(needs, slots, result)
	} else {
		_, conflicted := conflicts(needs)
		for _, n := range needs {
			if conflicted[n.key] != nil || (n.tick == nil && !mode.earlier()) || slices.ContainsFunc(ops, func(o neededOp) bool { return o.op.key == n.key }) {
				continue
			}
			ops = append(ops, neededOp{op: bindingOp{key: n.key, org: n.org}, orgName: n.orgName, tick: n.tick})
		}
	}
	return withExceptions(ops), nil
}

// withExceptions marks each repository bound to one organization while its owner is bound
// to another in the same decisions as an exception to the owner, which routing allows
// (the repository's binding wins for it) only when asked for.
func withExceptions(ops []neededOp) []neededOp {
	for i, o := range ops {
		owner := routing.Owner(o.op.key)
		if owner == "" || owner == o.op.key {
			continue
		}
		if slices.ContainsFunc(ops, func(other neededOp) bool { return other.op.key == owner && other.op.org != o.op.org }) {
			ops[i].op.override = true
		}
	}
	return ops
}

// bindingsForm is every need as a line: each organization's keys, ticked; an exact-only
// team's owner as a pick between its listed repositories (the default) and the whole owner;
// then, in red, each key more than one organization needs, to bind to one of them or leave
// to decide, which it starts on.
func bindingsForm(needs []bindingNeed) (*prompt.Form, []bindingSlot) {
	form := &prompt.Form{Title: "Bind these to the organizations that need them; one left unticked waits, holding what it matches (`terma teams --refuse <key>` refuses one):"}
	var slots []bindingSlot
	add := func(it prompt.Item, s bindingSlot) {
		form.Items = append(form.Items, it)
		slots = append(slots, s)
	}
	conflictKeys, conflicted := conflicts(needs)
	group := 0
	var orgs []string
	for _, n := range needs {
		if conflicted[n.key] == nil && !slices.Contains(orgs, n.org) {
			orgs = append(orgs, n.org)
		}
	}
	for _, org := range orgs {
		heading := ""
		head := func(name string) string {
			if heading != "" {
				return ""
			}
			heading = name
			return name
		}
		var owners []string
		listed := map[string][]int{}
		for i, n := range needs {
			if n.org != org || conflicted[n.key] != nil || slices.ContainsFunc(listed[n.owner], func(j int) bool { return needs[j].key == n.key }) {
				continue
			}
			if n.owner != "" && !slices.Contains(owners, n.owner) {
				owners = append(owners, n.owner)
			}
			listed[n.owner] = append(listed[n.owner], i)
		}
		for i, n := range needs {
			if !slices.Contains(listed[n.owner], i) || n.owner != "" && len(listed[n.owner]) > collapseAt {
				continue
			}
			add(prompt.Item{Label: n.key, Detail: "listed by " + teamEntry(n.key, n.entry, n.teamName) + exceptionNote(needs, n),
				Kind: prompt.Check, Selected: true, Heading: head(n.orgName)}, bindingSlot{kind: slotKeys, needs: []int{i}, org: n.org, orgName: n.orgName, tick: n.tick})
		}
		for _, o := range owners {
			ids := listed[o]
			first := needs[ids[0]]
			if len(ids) > collapseAt {
				add(prompt.Item{Label: fmt.Sprintf("%s: its %d listed repositories", o, len(ids)), Detail: "listed by " + first.teamName,
					Kind: prompt.Check, Selected: true, Heading: head(first.orgName)}, bindingSlot{kind: slotKeys, needs: ids, org: org, orgName: first.orgName, tick: sharedTick(needs, ids)})
			}
			if ownerShared(needs, o, org) {
				continue
			}
			group++
			add(prompt.Item{Label: "only the listed repositories under " + o, Detail: "the owner's others stay free: personal may collect them",
				Kind: prompt.Radio, Group: group, Selected: true, Heading: head(first.orgName)}, bindingSlot{kind: slotListedOnly, owner: o, org: org})
			add(prompt.Item{Label: "the whole owner " + o, Detail: "every repository under it belongs to " + first.orgName,
				Kind: prompt.Radio, Group: group}, bindingSlot{kind: slotWholeOwner, needs: ids, owner: o, org: org, orgName: first.orgName, tick: sharedTick(needs, ids)})
		}
	}
	heading := conflictHeading
	for _, key := range conflictKeys {
		group++
		for _, n := range conflicted[key] {
			add(prompt.Item{Label: "bind " + key + " to " + n.orgName, Detail: "listed by " + teamEntry(n.key, n.entry, n.teamName),
				Kind: prompt.Radio, Group: group, Alert: true, Heading: heading}, bindingSlot{kind: slotConflict, key: key, org: n.org, orgName: n.orgName, tick: n.tick})
			heading = ""
		}
		add(prompt.Item{Label: key + ": decide later", Detail: "what it matches is held", Kind: prompt.Radio, Group: group, Selected: true, Alert: true},
			bindingSlot{kind: slotLater, key: key})
	}
	return form, slots
}

// exceptionNote says, for a repository whose owner another organization needs, that
// binding it makes it an exception to that owner.
func exceptionNote(needs []bindingNeed, n bindingNeed) string {
	owner := routing.Owner(n.key)
	if owner == "" || owner == n.key {
		return ""
	}
	if i := slices.IndexFunc(needs, func(o bindingNeed) bool { return o.key == owner && o.org != n.org }); i >= 0 {
		return "; an exception to " + owner + ", which " + needs[i].orgName + " needs"
	}
	return ""
}

// sharedTick is the tick every one of ids' needs belongs to, or nil when they span ticks.
func sharedTick(needs []bindingNeed, ids []int) *tick {
	t := needs[ids[0]].tick
	for _, i := range ids[1:] {
		if needs[i].tick != t {
			return nil
		}
	}
	return t
}

// ownerShared reports whether a need of an organization other than org names owner, as its
// key or its owner: binding it whole would take what that organization needs.
func ownerShared(needs []bindingNeed, owner, org string) bool {
	return slices.ContainsFunc(needs, func(n bindingNeed) bool {
		return n.org != org && (n.key == owner || n.owner == owner || routing.Owner(n.key) == owner)
	})
}

// formOps are the decisions the checklist's answer makes: a whole owner picked binds it in
// place of its listed repositories.
func formOps(needs []bindingNeed, slots []bindingSlot, result []prompt.Item) []neededOp {
	var ops []neededOp
	whole := map[[2]string]bool{}
	for k, s := range slots {
		if s.kind == slotWholeOwner && result[k].Selected {
			whole[[2]string{s.owner, s.org}] = true
			ops = append(ops, neededOp{op: bindingOp{key: s.owner, org: s.org}, orgName: s.orgName, tick: s.tick})
		}
	}
	for k, s := range slots {
		if !result[k].Selected {
			continue
		}
		switch s.kind {
		case slotKeys:
			for _, i := range s.needs {
				if n := needs[i]; !whole[[2]string{n.owner, n.org}] {
					ops = append(ops, neededOp{op: bindingOp{key: n.key, org: n.org}, orgName: n.orgName, tick: n.tick})
				}
			}
		case slotConflict:
			ops = append(ops, neededOp{op: bindingOp{key: s.key, org: s.org}, orgName: s.orgName, tick: s.tick})
		}
	}
	return ops
}
