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

// neededOp is a decision about a key, with every team in this run that needs it (holders)
// and whether a team ticked before does (earlier): it is undone only when every holder
// fails to tick and no earlier team needs it.
type neededOp struct {
	op      bindingOp
	orgName string
	holders []*tick
	earlier bool
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
	kind       slotKind
	needs      []int
	owner, key string
	org        string
}

// decision is one key bound to one organization by this run.
type decision struct{ key, org string }

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
		switch key, b, decided := st.Decision(e); {
		case decided && b.Refused:
			tk.notes = append(tk.notes, raw+" is skipped: "+key+" is refused to every organization")
		case decided && b.Organization != t.OrganizationID:
			tk.notes = append(tk.notes, raw+" is skipped: "+key+" is bound to another organization")
		case !decided:
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

// settles are the other organizations' needs that binding owner (a key routing.Owner gives
// "" for) to org would settle
// against them: a repository under it another organization needs, which no decision of
// this run binds itself. Routing gives such a repository to its owner's organization.
func settles(all []bindingNeed, owner, org string, decided []decision) []bindingNeed {
	var out []bindingNeed
	for _, n := range all {
		if n.org == org || n.key == owner || routing.Owner(n.key) != owner ||
			slices.ContainsFunc(decided, func(d decision) bool { return d.key == n.key }) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// decideNeeds decides all, every binding left to decide, as mode says, the checklist drawn
// by run. Conflicts are found across all of them, whatever mode decides. A conflict is
// never decided for the developer, and an owner whose binding would settle another
// organization's need under it waits until that is decided: held says which.
func decideNeeds(all []bindingNeed, mode bindingMode, run func(*prompt.Form) ([]prompt.Item, error)) (ops []neededOp, held []string, err error) {
	if len(all) == 0 || mode == leavePending {
		return nil, nil, nil
	}
	conflictKeys, conflicted := conflicts(all)
	// What a run that ticks asks about: its own needs, and any key they share with another
	// organization's, so a conflict with a team ticked before is a conflict too.
	shown := slices.DeleteFunc(slices.Clone(all), func(n bindingNeed) bool {
		return n.tick == nil && !mode.earlier() && !(slices.Contains(conflictKeys, n.key) && slices.ContainsFunc(all, func(o bindingNeed) bool {
			return o.key == n.key && o.tick != nil
		}))
	})
	var decided []decision
	if mode.asks() {
		form, slots := bindingsForm(shown, all)
		result, err := run(form)
		if errors.Is(err, prompt.ErrCancelled) {
			return nil, nil, errCancelled
		}
		if err != nil {
			return nil, nil, err
		}
		decided = formDecisions(shown, slots, result)
	} else {
		for _, n := range shown {
			if conflicted[n.key] != nil || (n.tick == nil && !mode.earlier()) || slices.Contains(decided, decision{n.key, n.org}) {
				continue
			}
			decided = append(decided, decision{n.key, n.org})
		}
	}
	kept := decided[:0:0]
	for _, d := range decided {
		if s := settles(all, d.key, d.org, decided); routing.Owner(d.key) == "" && len(s) > 0 {
			held = append(held, fmt.Sprintf("%s is left to decide: binding it to %s would settle %s, which %s needs; decide that first",
				d.key, orgNameOf(all, d.org), s[0].key, s[0].orgName))
			continue
		}
		kept = append(kept, d)
	}
	return withHolders(all, kept), held, nil
}

// orgNameOf is org's name as all's needs give it.
func orgNameOf(all []bindingNeed, org string) string {
	if i := slices.IndexFunc(all, func(n bindingNeed) bool { return n.org == org }); i >= 0 {
		return all[i].orgName
	}
	return org
}

// withHolders makes each decision an op with every need in all it decides: those of the
// same organization for its key, or for an owner bound whole, under it. A repository bound
// while its owner is bound to another organization is an exception to the owner, which
// routing allows only when asked for.
func withHolders(all []bindingNeed, decided []decision) []neededOp {
	ops := make([]neededOp, 0, len(decided))
	for _, d := range decided {
		op := neededOp{op: bindingOp{key: d.key, org: d.org}, orgName: orgNameOf(all, d.org)}
		for _, n := range all {
			if n.org != d.org || (n.key != d.key && n.owner != d.key) {
				continue
			}
			if n.tick == nil {
				op.earlier = true
			} else if !slices.Contains(op.holders, n.tick) {
				op.holders = append(op.holders, n.tick)
			}
		}
		if owner := routing.Owner(d.key); owner != "" && slices.ContainsFunc(decided, func(o decision) bool { return o.key == owner && o.org != d.org }) {
			op.op.override = true
		}
		ops = append(ops, op)
	}
	return ops
}

// bindingsForm is shown, the needs a run asks about, as lines: each organization's keys,
// ticked, except an owner whose binding would settle another organization's need, in red
// and unticked; an exact-only team's owner with two or more listed repositories as a pick
// between them (the default) and the whole owner; then, in red, each key more than one
// organization needs, to bind to one of them or leave to decide, which it starts on. all is
// every need left to decide, for the conflicts and owners shown ones touch.
func bindingsForm(shown, all []bindingNeed) (*prompt.Form, []bindingSlot) {
	form := &prompt.Form{Title: "Bind these to the organizations that need them; one left unticked waits, holding what it matches (`terma teams --refuse <key>` refuses one):"}
	var slots []bindingSlot
	add := func(it prompt.Item, s bindingSlot) {
		form.Items = append(form.Items, it)
		slots = append(slots, s)
	}
	conflictKeys, conflicted := conflicts(all)
	group := 0
	var orgs []string
	for _, n := range shown {
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
		// Each key once, its repositories grouped by owner for collapsing.
		var keys []int
		byOwner := map[string][]int{}
		var owners []string
		for i, n := range shown {
			if n.org != org || conflicted[n.key] != nil || slices.ContainsFunc(keys, func(j int) bool { return shown[j].key == n.key }) {
				continue
			}
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
			item := prompt.Item{Label: n.key, Detail: "listed by " + teamEntry(n.key, n.entry, n.teamName) + exceptionNote(all, n),
				Kind: prompt.Check, Selected: true, Heading: head(n.orgName)}
			if s := settles(all, n.key, org, nil); len(s) > 0 {
				item.Selected, item.Alert = false, true
				item.Detail = "binding it gives " + s[0].key + ", which " + s[0].orgName + " needs, to " + n.orgName + ": decide that first"
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
			if first.owner == "" || len(ids) < 2 || ownerShared(all, o, org) {
				continue
			}
			group++
			add(prompt.Item{Label: "only the listed repositories under " + o, Detail: "the owner's others stay free: personal may collect them",
				Kind: prompt.Radio, Group: group, Selected: true, Heading: head(first.orgName)}, bindingSlot{kind: slotListedOnly, owner: o, org: org})
			add(prompt.Item{Label: "the whole owner " + o, Detail: "every repository under it belongs to " + first.orgName,
				Kind: prompt.Radio, Group: group}, bindingSlot{kind: slotWholeOwner, needs: ids, owner: o, org: org})
		}
	}
	heading := conflictHeading
	for _, key := range conflictKeys {
		if !slices.ContainsFunc(shown, func(n bindingNeed) bool { return n.key == key }) {
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
func exceptionNote(all []bindingNeed, n bindingNeed) string {
	owner := routing.Owner(n.key)
	if owner == "" {
		return ""
	}
	if i := slices.IndexFunc(all, func(o bindingNeed) bool { return o.key == owner && o.org != n.org }); i >= 0 {
		return "; an exception to " + owner + ", which " + all[i].orgName + " needs"
	}
	return ""
}

// ownerShared reports whether a need of an organization other than org names owner, as its
// key or its owner: binding it whole would take what that organization needs.
func ownerShared(all []bindingNeed, owner, org string) bool {
	return slices.ContainsFunc(all, func(n bindingNeed) bool {
		return n.org != org && (n.key == owner || routing.Owner(n.key) == owner)
	})
}

// formDecisions are the bindings the checklist's answer makes: a whole owner picked binds
// it in place of its listed repositories.
func formDecisions(shown []bindingNeed, slots []bindingSlot, result []prompt.Item) []decision {
	var out []decision
	whole := map[decision]bool{}
	for k, s := range slots {
		if s.kind == slotWholeOwner && result[k].Selected {
			whole[decision{s.owner, s.org}] = true
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
				if d := (decision{n.key, n.org}); !whole[decision{routing.Owner(n.key), n.org}] && !slices.Contains(out, d) {
					out = append(out, d)
				}
			}
		case slotConflict:
			out = append(out, decision{s.key, s.org})
		}
	}
	return out
}
