package doctor

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// OrderChanges says why the routing order is not fixed.
const OrderChanges = "policies can change at any time: the most recently updated policy wins an overlap, so this order changes whenever a team updates its policy"

// orderNote is doctor's footnote under the routing order.
const orderNote = "Teams are tried top to bottom. The newest policy wins a shared repository."

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

// policyDate is when t's policy was updated, the year left out when it is now's.
func policyDate(t routing.Team, now time.Time) string {
	at := t.Policy.UpdatedAt.Local()
	switch {
	case t.Personal:
		return "always last"
	case t.Policy.UpdatedAt.IsZero():
		return "update date unknown"
	case at.Year() == now.Local().Year():
		return "updated " + at.Format("2 Jan 15:04")
	}
	return "updated " + at.Format("2 Jan 2006")
}

// TeamRows are `terma status`'s rows for a routed profile: doctor's team sections, a line
// each.
func TeamRows(r Routing, now time.Time) []Row {
	var rows []Row
	for _, s := range r.teamSections(now) {
		label := s.Title
		for _, l := range s.Lines {
			v := strings.Repeat("  ", l.Depth) + l.Text
			if l.Note != "" {
				v += " (" + l.Note + ")"
			}
			rows = append(rows, Row{label, v})
			label = ""
		}
	}
	return rows
}

// teamSections are doctor's view of a routed profile: each ticked team in the order routing
// tries them, with its state and what it collects beneath it, then the paused sessions.
func (r Routing) teamSections(now time.Time) []Section {
	teams := Section{Title: "Where your data goes"}
	if r.unusable() || len(r.State.Teams) == 0 {
		c := TeamsCheck(r, now)
		teams.Lines = []Line{{Mark: markOf(c.Status), Text: c.Detail}}
		return []Section{teams}
	}
	order := routing.Order(r.State)
	collected := r.collected(order)
	numbered := len(order) > 1
	for i, t := range order {
		teams.Lines = append(teams.Lines, r.teamLine(t, pick(numbered, fmt.Sprintf("%d. ", i+1), ""), now))
		teams.Lines = append(teams.Lines, collected[t.EnrollmentID]...)
	}
	for _, t := range r.State.Teams {
		if !slices.ContainsFunc(order, func(o routing.Team) bool { return o.EnrollmentID == t.EnrollmentID }) {
			teams.Lines = append(teams.Lines, r.teamLine(t, "", now))
		}
	}
	teams.Lines = append(teams.Lines, r.blockedLines()...)
	if orgs := len(order) - pick(len(order) > 0 && order[len(order)-1].Personal, 1, 0); orgs > 1 {
		teams.Footnote = orderNote
	}
	out := []Section{teams}
	if s, ok := r.pausedSection(); ok {
		out = append(out, s)
	}
	return out
}

// teamLine is t's line: its state as the mark, and why it cannot send when it cannot,
// else when its policy was updated.
func (r Routing) teamLine(t routing.Team, number string, now time.Time) Line {
	l := Line{Mark: MarkOK, Text: number + r.label(t), Note: policyDate(t, now)}
	switch t.StateAt(now) {
	case routing.Expired:
		l.Mark, l.Note = MarkWarn, "policy out of date, its repositories wait"
	case routing.SignInNeeded:
		l.Mark, l.Note = MarkWarn, "sign in again to send"
	case routing.Suspended:
		l.Mark, l.Note = MarkFail, "refused once, its repositories wait"
	case routing.Refused:
		l.Mark, l.Note = MarkFail, refusedNote(t.LastError)
	default:
		switch {
		case t.Policy.Unset:
			l.Note = "no collection policy yet"
		case !t.Policy.Global() && len(policyEntries(t)) == 0:
			l.Note = "no repositories yet"
		}
	}
	return l
}

// refusedNote says why a refused team is skipped.
func refusedNote(c routing.ErrorClass) string {
	switch c {
	case routing.OrgAccessLost:
		return "no access to this organization, skipped"
	case routing.Terminal:
		return "team removed, skipped"
	}
	return "refused, skipped"
}

// collected are the entry lines under each team, by enrollment: every repository entry,
// under the team Route sends it to, the first in routing order whose policy lists it or
// a wildcard covering it. An entry listed again, word for word, later in the order shows
// once, and the teams that list it too but lose it are named. A refused team is passed
// over, so it collects nothing; a global Personal collects everything else.
func (r Routing) collected(order []routing.Team) map[string][]Line {
	type entry struct {
		e      routing.Entry
		by     routing.Team
		alsoBy []routing.Team
	}
	var seen []entry
	out := map[string][]Line{}
	for i, t := range order {
		for _, e := range policyEntries(t) {
			if slices.ContainsFunc(seen, func(s entry) bool { return contains(s.e, e) && contains(e, s.e) }) {
				continue
			}
			at := slices.IndexFunc(order[:i+1], func(o routing.Team) bool {
				return slices.ContainsFunc(policyEntries(o), func(oe routing.Entry) bool { return contains(oe, e) })
			})
			en := entry{e: e, by: order[at]}
			for _, o := range order[at+1:] {
				if slices.ContainsFunc(policyEntries(o), func(oe routing.Entry) bool { return contains(oe, e) }) {
					en.alsoBy = append(en.alsoBy, o)
				}
			}
			seen = append(seen, en)
			id := en.by.EnrollmentID
			out[id] = append(out[id], Line{Depth: 1, Text: e.Raw})
			if note := r.conflict(en.by, en.alsoBy); note != "" {
				out[id] = append(out[id], Line{Depth: 2, Text: note, Quiet: true})
			}
		}
	}
	if n := len(order); n > 0 && order[n-1].Personal && order[n-1].Policy.Global() {
		id := order[n-1].EnrollmentID
		out[id] = append(out[id], Line{Depth: 1, Text: "everything else"})
	}
	return out
}

// conflict says who else lists an entry winner collects.
func (r Routing) conflict(winner routing.Team, others []routing.Team) string {
	if len(others) == 0 {
		return ""
	}
	names := make([]string, len(others))
	org := false
	for i, o := range others {
		names[i] = r.short(o)
		org = org || !o.Personal
	}
	return "Also listed by " + strings.Join(names, " and ") + pick(org, ". This policy is newer.", ", which comes last.")
}

// pausedSection is the sessions the relay stopped sending, when there are any.
func (r Routing) pausedSection() (Section, bool) {
	if len(r.Mixed) == 0 && r.MixedDropped == 0 {
		return Section{}, false
	}
	s := Section{Title: "Paused sessions",
		Footnote: "A session pauses when it may touch another organization's work. It sends nothing until it resumes."}
	if n := len(r.Mixed); n > 0 {
		s.Lines = append(s.Lines, Line{Mark: MarkWarn, Text: fmt.Sprintf("%d %s", n, pick(n == 1, "session is paused", "sessions are paused"))})
		by := map[runs.Reason]int{}
		for _, m := range r.Mixed {
			by[m.Why.Reason]++
		}
		reasons := slices.Collect(maps.Keys(by))
		slices.SortFunc(reasons, func(a, b runs.Reason) int { return cmp.Or(by[b]-by[a], strings.Compare(string(a), string(b))) })
		for _, why := range reasons {
			s.Lines = append(s.Lines, Line{Depth: 1, Text: fmt.Sprintf("%d %s", by[why], pausedWords(why))})
		}
	}
	if d := r.MixedDropped; d > 0 {
		s.Lines = append(s.Lines, Line{Mark: MarkWarn, Text: fmt.Sprintf("%d %s from paused sessions dropped", d, pick(d == 1, "record", "records"))})
	}
	return s, true
}

// pausedWords says, for a reader, why a session paused.
func pausedWords(r runs.Reason) string {
	switch r {
	case runs.Crossed:
		return "worked in another organization's repository"
	case runs.UnknownReach:
		return "ran a command terma could not trace"
	case runs.LocationBound:
		return "touched more folders than terma tracks"
	case runs.Aside:
		return "changed organization while terma was busy"
	}
	return "with a record terma cannot read"
}

// blockedLines are what the relay held and then dropped, by blocker.
func (r Routing) blockedLines() []Line {
	var out []Line
	for _, k := range slices.Sorted(maps.Keys(r.Blocked)) {
		n := r.Blocked[k]
		what := fmt.Sprintf("%d %s", n, pick(n == 1, "record", "records"))
		if kind, name, _ := strings.Cut(k, ":"); kind == "team" {
			what += " for a team no longer ticked"
			if t, ok := r.State.Team(name); ok {
				what = fmt.Sprintf("%d %s for %s", n, pick(n == 1, "record", "records"), r.label(t))
			}
		}
		out = append(out, Line{Mark: MarkWarn, Text: what + " could not be sent"})
	}
	return out
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

func pick[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}

// short names t in a note: its team name alone where no other ticked team shares it,
// else its full label.
func (r Routing) short(t routing.Team) string {
	full := r.label(t)
	name := cmp.Or(r.Teams[t.TeamID], t.TeamID)
	if t.Personal || !strings.HasSuffix(full, name) {
		return full
	}
	for _, o := range r.State.Teams {
		if o.EnrollmentID != t.EnrollmentID && !o.Personal && cmp.Or(r.Teams[o.TeamID], o.TeamID) == name {
			return full
		}
	}
	return name
}
