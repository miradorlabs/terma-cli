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

// decision is one key bound to one organization by this run.
type decision struct{ key, org string }

// tickNeeds are the bindings tk's policy needs with no decision in st. An entry under an
// owner decided otherwise is skipped, and said in tk's notes.
func tickNeeds(st routing.State, tk *tick) []bindingNeed {
	t := tk.team
	if t.Personal || t.Policy.Global() {
		return nil
	}
	var entries []routing.Entry
	seen := map[string]bool{}
	exactOnly := true
	for _, raw := range t.Policy.Repositories {
		e, ok := routing.ParseEntry(raw, t.Policy.Wildcards)
		if !ok || seen[e.Key()] {
			continue
		}
		seen[e.Key()] = true
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
// organizations and teams named by orgName and teamName. As for a tick, an exact-only
// team's repository names its owner, which may be bound whole in their place.
func pendingNeeds(st routing.State, orgName, teamName func(string) string) []bindingNeed {
	exactOnly := map[string]bool{}
	for _, t := range st.Teams {
		exactOnly[t.TeamID] = !slices.ContainsFunc(t.Policy.Repositories, func(raw string) bool {
			e, ok := routing.ParseEntry(raw, t.Policy.Wildcards)
			return ok && e.Wildcard()
		})
	}
	var needs []bindingNeed
	for _, p := range doctor.Pending(st) {
		n := bindingNeed{key: p.Key, org: p.Organization, orgName: cmp.Or(orgName(p.Organization), p.Organization),
			entry: p.Entry, teamName: cmp.Or(teamName(p.TeamID), p.TeamID)}
		if exactOnly[p.TeamID] {
			n.owner = routing.Owner(p.Key)
		}
		needs = append(needs, n)
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

// indexNeeds indexes needs for the binding rules they share with doctor and --bind.
func indexNeeds(needs []bindingNeed) routing.Needs {
	out := make([]routing.Need, 0, len(needs))
	for _, n := range needs {
		out = append(out, routing.Need{Key: n.key, Organization: n.org})
	}
	return routing.IndexNeeds(out)
}

// conflicts are the keys more than one organization needs, each with one need per
// organization, in the order needs first names them.
func conflicts(needs []bindingNeed, ix routing.Needs) ([]string, map[string][]bindingNeed) {
	var keys []string
	by := map[string][]bindingNeed{}
	seen := map[routing.Need]bool{}
	for _, n := range needs {
		if !ix.Conflict(n.key) || seen[routing.Need{Key: n.key, Organization: n.org}] {
			continue
		}
		seen[routing.Need{Key: n.key, Organization: n.org}] = true
		if by[n.key] == nil {
			keys = append(keys, n.key)
		}
		by[n.key] = append(by[n.key], n)
	}
	return keys, by
}

// orgNames are the organizations' names as needs give them.
func orgNames(needs []bindingNeed) map[string]string {
	names := map[string]string{}
	for _, n := range needs {
		if _, ok := names[n.org]; !ok {
			names[n.org] = n.orgName
		}
	}
	return names
}

// decideNeeds decides all, every binding left to decide, as mode says, the checklist drawn
// by run. Conflicts are found across all of them, whatever mode decides. A conflict is
// never decided for the developer, and an owner whose binding would give another
// organization's need under it away waits until that is decided: held says which.
func decideNeeds(all []bindingNeed, mode bindingMode, run func(*prompt.Form) ([]prompt.Item, error)) (ops []neededOp, held []string, err error) {
	if len(all) == 0 || mode == leavePending {
		return nil, nil, nil
	}
	ix := indexNeeds(all)
	names := orgNames(all)
	// What a run that ticks asks about: its own needs, and a team ticked before's need that
	// one of them would settle: the same key (a conflict), or its owner, for another
	// organization. So it is decided in the same form, never given away unseen.
	tickedBy := map[string][]string{}
	for _, n := range all {
		if n.tick != nil {
			tickedBy[n.key] = append(tickedBy[n.key], n.org)
		}
	}
	elsewhere := func(key, org string) bool {
		return slices.ContainsFunc(tickedBy[key], func(o string) bool { return o != org })
	}
	shown := slices.DeleteFunc(slices.Clone(all), func(n bindingNeed) bool {
		if n.tick != nil || mode.earlier() {
			return false
		}
		sharedKey := ix.Conflict(n.key) && len(tickedBy[n.key]) > 0
		return !sharedKey && !elsewhere(routing.Owner(n.key), n.org)
	})
	var order []decision
	if mode.asks() {
		form, slots := bindingsForm(shown, all, ix)
		if len(form.Items) == 0 {
			return nil, nil, nil
		}
		result, err := run(form)
		if errors.Is(err, prompt.ErrCancelled) {
			return nil, nil, errCancelled
		}
		if err != nil {
			return nil, nil, err
		}
		order = formDecisions(shown, slots, result)
	} else {
		seen := map[decision]bool{}
		for _, n := range shown {
			d := decision{n.key, n.org}
			if ix.Conflict(n.key) || (n.tick == nil && !mode.earlier()) || seen[d] {
				continue
			}
			seen[d] = true
			order = append(order, d)
		}
	}
	decided := make(map[string]bool, len(order))
	for _, d := range order {
		decided[d.key] = true
	}
	kept := order[:0:0]
	for _, d := range order {
		if s, ok := ix.Settles(d.key, d.org, func(k string) bool { return decided[k] }); ok {
			held = append(held, fmt.Sprintf("%s is left to decide: binding it to %s would also give it %s, which %s needs; decide that first",
				d.key, cmp.Or(names[d.org], d.org), s.Key, cmp.Or(names[s.Organization], s.Organization)))
			continue
		}
		kept = append(kept, d)
	}
	return withHolders(all, kept, names), held, nil
}

// withHolders makes each decision an op with every need in all it decides: those of the
// same organization for its key, or for an owner bound whole, under it. A repository bound
// while its owner is bound to another organization is an exception to the owner, which
// routing allows only when asked for.
func withHolders(all []bindingNeed, decided []decision, names map[string]string) []neededOp {
	by := map[decision][]bindingNeed{}
	for _, n := range all {
		by[decision{n.key, n.org}] = append(by[decision{n.key, n.org}], n)
		if n.owner != "" {
			by[decision{n.owner, n.org}] = append(by[decision{n.owner, n.org}], n)
		}
	}
	orgOf := make(map[string]string, len(decided))
	for _, d := range decided {
		orgOf[d.key] = d.org
	}
	ops := make([]neededOp, 0, len(decided))
	for _, d := range decided {
		op := neededOp{op: bindingOp{key: d.key, org: d.org}, orgName: cmp.Or(names[d.org], d.org)}
		for _, n := range by[d] {
			if n.tick == nil {
				op.earlier = true
			} else if !slices.Contains(op.holders, n.tick) {
				op.holders = append(op.holders, n.tick)
			}
		}
		if org, ok := orgOf[routing.Owner(d.key)]; ok && org != d.org {
			op.op.override = true
		}
		ops = append(ops, op)
	}
	return ops
}
