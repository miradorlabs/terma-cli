package doctor

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/routing"
)

// OrderChanges says why the routing order is not fixed.
const OrderChanges = "policies can change at any time: the most recently updated policy wins an overlap, so this order changes whenever a team updates its policy"

// PolicyAge says when t's policy was last updated, which places it in the routing order.
func PolicyAge(t routing.Team) string {
	switch {
	case t.Personal:
		return "last, collects what no organization does"
	case t.Policy.UpdatedAt.IsZero():
		return "policy update time unknown"
	}
	return "policy updated " + t.Policy.UpdatedAt.Local().Format("2 Jan 2006 15:04")
}

func pick[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}

// TeamRows are doctor's context rows for a routed profile: each ticked team and its health,
// what each repository entry is collected by, the order routing tries the teams
// in, Mixed runs, and what the relay held.
func TeamRows(r Routing, now time.Time) []Row {
	if r.unusable() {
		return []Row{{"Teams", "none: no usable routing state, so nothing is sent"}}
	}
	var rows []Row
	label := "Teams"
	for _, t := range r.State.Teams {
		state := words(string(t.StateAt(now)))
		if t.LastError == routing.OrgAccessLost {
			state += " (" + refusalWords(t.LastError) + ")"
		} else if t.LastError != "" {
			state += ", last refresh " + words(string(t.LastError))
		}
		if !t.Policy.Global() && len(policyEntries(t)) == 0 {
			state += "; lists no repositories yet"
		}
		rows = append(rows, Row{label, r.label(t) + " — " + state})
		label = ""
	}
	if len(rows) == 0 {
		return append(rows, Row{"Teams", "none ticked — run `" + r.teamsFix() + "`"})
	}
	rows = append(rows, r.collectsRows(now)...)
	if order := routing.Order(r.State); len(order) > 1 {
		for i, t := range order {
			rows = append(rows, Row{pick(i == 0, "Routing order", ""), fmt.Sprintf("%d. %s — %s", i+1, r.label(t), PolicyAge(t))})
		}
		rows = append(rows, Row{"", OrderChanges})
	}
	if len(r.Mixed) > 0 || r.MixedDropped > 0 {
		rows = append(rows, Row{"Mixed", MixedSummary(r)})
		for _, m := range r.Mixed {
			if m.Recent {
				rows = append(rows, Row{"", "session " + m.Session + " " + whyWords(m.Why)})
			}
		}
	}
	return append(rows, r.blockedRows()...)
}

// collectsRows name each repository entry, in routing order, with the team Route sends it to:
// the first in routing.Order whose policy lists it or a wildcard covering it. An entry listed
// again, word for word, by a team later in the order is shown once; the teams that list it
// too but lose it are named. A refused team is passed over, so it collects nothing.
func (r Routing) collectsRows(now time.Time) []Row {
	order := routing.Order(r.State)
	type line struct {
		e      routing.Entry
		by     routing.Team
		alsoBy []string
	}
	var lines []line
	for i, t := range order {
		for _, e := range policyEntries(t) {
			at := slices.IndexFunc(order[:i+1], func(o routing.Team) bool {
				return slices.ContainsFunc(policyEntries(o), func(oe routing.Entry) bool { return contains(oe, e) })
			})
			if slices.ContainsFunc(lines, func(l line) bool { return contains(l.e, e) && contains(e, l.e) }) {
				continue
			}
			l := line{e: e, by: order[at]}
			for _, o := range order[at+1:] {
				if slices.ContainsFunc(policyEntries(o), func(oe routing.Entry) bool { return contains(oe, e) }) {
					l.alsoBy = append(l.alsoBy, r.label(o))
				}
			}
			lines = append(lines, l)
		}
	}
	var rows []Row
	held := func(t routing.Team) string {
		if st := t.StateAt(now); st != routing.Active {
			return " — held: " + words(string(st))
		}
		return ""
	}
	for _, l := range lines {
		v := l.e.Raw + " (" + r.label(l.by) + ")" + held(l.by)
		if len(l.alsoBy) > 0 {
			v += pick(strings.Contains(v, " — "), "; ", " — ") + "also listed by " + strings.Join(l.alsoBy, ", ") + ", " + pick(len(l.alsoBy) == 1, "which does", "which do") + " not collect it"
		}
		rows = append(rows, Row{pick(len(rows) == 0, "Collects", ""), v})
	}
	if n := len(order); n > 0 && order[n-1].Personal && order[n-1].Policy.Global() {
		p := order[n-1]
		rows = append(rows, Row{pick(len(rows) == 0, "Collects", ""), "everything else (" + r.label(p) + ")" + held(p)})
	}
	if len(rows) == 0 {
		rows = append(rows, Row{"Collects", "nothing: no ticked team lists a repository"})
	}
	return rows
}

// policyEntries are the repository entries t's policy lists, in its order; none for a
// global or unset policy, and none for an entry Route cannot read.
func policyEntries(t routing.Team) []routing.Entry {
	if t.Policy.Global() || t.Policy.Unset {
		return nil
	}
	var out []routing.Entry
	for _, raw := range t.Policy.Repositories {
		if e, ok := routing.ParseEntry(raw, t.Policy.Wildcards); ok {
			out = append(out, e)
		}
	}
	return out
}

// contains reports whether every repository b matches, a matches too.
func contains(a, b routing.Entry) bool {
	if !a.Overlaps(b) {
		return false
	}
	switch {
	case !a.Wildcard():
		return !b.Wildcard()
	case b.Wildcard():
		return a.Fixed() <= b.Fixed()
	}
	return true
}

func (r Routing) blockedRows() []Row {
	var rows []Row
	label := "Relay held"
	for _, k := range slices.Sorted(maps.Keys(r.Blocked)) {
		what := k
		switch kind, name, _ := strings.Cut(k, ":"); kind {
		case "team":
			what = name + ", no longer ticked"
			if t, ok := r.State.Team(name); ok {
				what = r.label(t) + ", which could not send"
			}
		case "overflow":
			what = "other blockers"
		}
		rows = append(rows, Row{label, fmt.Sprintf("%d record(s) for %s", r.Blocked[k], what)})
		label = ""
	}
	return rows
}
