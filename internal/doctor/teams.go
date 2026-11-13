package doctor

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// Check keys of a machine that routes by teams.
const (
	KeyTeams     = "teams"
	KeyMigration = "migration"
)

// Routing is a migrated machine's routing as doctor judges it: the profile's routing state
// and what the probes found of its keys and Mixed runs. Env.Routing is nil on a machine
// that still routes by its one team, which doctor checks as before.
type Routing struct {
	State routing.State
	// Err says why the profile's routing state cannot be read; Missing that it has none;
	// Unfinished that the move to routing by teams has not committed it, saying why when
	// the relay recorded it. Any of them sends nothing.
	Err        error
	Missing    bool
	Unfinished string
	// Teams and Orgs name teams and organizations by id, as `terma teams` last listed them.
	Teams, Orgs map[string]string
	// Key reports, masked, an enrollment's key on this machine: "" when it has none.
	Key func(enrollmentID string) string
	// MixedSessions are the sessions whose current run crossed organizations, and so sends
	// no telemetry, active within the last day; OlderMixed counts the others the run
	// ledger keeps. MixedDropped counts what the relay dropped from such runs.
	MixedSessions []string
	OlderMixed    int
	MixedDropped  int
	// Blocked counts what else the relay kept from leaving, as its last stop recorded it,
	// by what blocked it: "pending:<binding>" for a binding still to decide and
	// "team:<enrollment>" for a team that could not send.
	Blocked map[string]int
}

// label names t: its organization's and its own name, else its id.
func (r Routing) label(t routing.Team) string {
	name := cmp.Or(r.Teams[t.TeamID], t.TeamID)
	if org := r.Orgs[t.OrganizationID]; org != "" && org != name {
		return org + " " + name
	}
	return name
}

func (r Routing) org(id string) string { return cmp.Or(r.Orgs[id], id) }

// unusable reports whether r's routing state sends nothing at all: unreadable, missing, or
// not yet committed by the move to routing by teams.
func (r Routing) unusable() bool { return r.Err != nil || r.Missing || r.Unfinished != "" }

// MigrationBlock is why the last relay start did not move this machine to routing by
// teams. The machine goes on collecting by its one team meanwhile.
type MigrationBlock struct {
	// Detail says why, and Fix what the developer does about it.
	Detail, Fix string
	// Fail: only the developer can unblock it, as with a second install the agents' hooks
	// run. Otherwise a later relay start may move the machine by itself, and doctor warns.
	Fail bool
}

// MigrationCheck judges b: a failure when only the developer can unblock it, else a warning.
func MigrationCheck(b MigrationBlock) Check {
	status := Warn
	if b.Fail {
		status = Fail
	}
	return Check{Status: status, Detail: "this machine has not moved to routing by teams: " + b.Detail, Fix: b.Fix}
}

// TeamsCheck judges the ticked teams. It fails while the routing state is unreadable or
// missing (nothing is sent), while a team is suspended or refused, and while a binding a
// ticked organization's entry needs is undecided: each blocks what it covers until the
// developer acts. An expired team or one whose sign-in was refused warns.
func TeamsCheck(r Routing, now time.Time) Check {
	switch {
	case r.Err != nil:
		return Check{Status: Fail, Detail: "this machine's routing state cannot be read, so nothing is sent: " + r.Err.Error(), Fix: "terma setup"}
	case r.Missing:
		return Check{Status: Fail, Detail: "this profile has no routing state, so nothing is sent", Fix: "terma setup"}
	case r.Unfinished != "":
		return Check{Status: Fail, Detail: "this machine's move to routing by teams has not finished, so nothing is sent until the relay finishes it: " + r.Unfinished,
			Fix: "terma teams (it restarts the relay, which finishes the move)"}
	case len(r.State.Teams) == 0:
		return Check{Status: Warn, Detail: "no team is ticked, so nothing is collected", Fix: "terma teams"}
	}
	var parts, failing, warning []string
	fix := ""
	for _, t := range r.State.Teams {
		st := t.StateAt(now)
		parts = append(parts, r.label(t)+" ("+words(string(st))+")")
		switch st {
		case routing.Suspended, routing.Refused:
			failing = append(failing, r.label(t)+" is "+words(string(st))+" ("+refusalWords(t.LastError)+"), and blocks what it lists")
			fix = cmp.Or(fix, "terma teams --release "+quoteWord(cmp.Or(r.Teams[t.TeamID], t.TeamID))+" (or wait for its policy to come back)")
		case routing.SignInNeeded:
			warning = append(warning, r.label(t)+" needs you to sign in to "+r.org(t.OrganizationID)+" again")
			fix = cmp.Or(fix, "terma teams --org "+quoteWord(r.org(t.OrganizationID)))
		case routing.Expired:
			warning = append(warning, r.label(t)+"'s policy has not been refreshed for over a week, so it blocks what it lists")
			fix = cmp.Or(fix, "check the network; the relay refreshes it every minute")
		case routing.Active:
			if r.Key != nil && r.Key(t.EnrollmentID) == "" {
				warning = append(warning, "no key for "+r.label(t)+" on this machine yet: what it collects waits until the relay creates one")
			}
		}
	}
	for _, p := range Pending(r.State) {
		failing = append(failing, p.Key+" is undecided, so what "+p.Entry+" matches is held")
		fix = cmp.Or(fix, "terma teams --bind "+p.Key+"="+quoteWord(r.org(p.Organization))+" (or --refuse "+p.Key+")")
	}
	detail := strings.Join(parts, ", ")
	if len(r.MixedSessions) > 0 || r.OlderMixed > 0 || r.MixedDropped > 0 {
		detail += "; " + MixedSummary(r)
	}
	switch {
	case len(failing) > 0:
		return Check{Status: Fail, Detail: strings.Join(append(failing, warning...), "; "), Fix: fix}
	case len(warning) > 0:
		return Check{Status: Warn, Detail: strings.Join(warning, "; "), Fix: fix}
	}
	return Check{Status: Pass, Detail: detail}
}

// MixedSummary says what runs that crossed organizations dropped.
func MixedSummary(r Routing) string {
	s := fmt.Sprintf("%d session run(s) crossed organizations and send no telemetry until they resume", len(r.MixedSessions)+r.OlderMixed)
	if r.OlderMixed > 0 {
		s += fmt.Sprintf(" (%d idle for over a day)", r.OlderMixed)
	}
	if r.MixedDropped > 0 {
		s += fmt.Sprintf("; the relay dropped %d record(s) from such runs", r.MixedDropped)
	}
	return s
}

func words(s string) string { return strings.ReplaceAll(s, "_", " ") }

func quoteWord(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

// refusalWords says why a team was refused, from its last error.
func refusalWords(c routing.ErrorClass) string {
	switch c {
	case routing.OrgAccessLost:
		return "you no longer have access to its organization"
	case routing.Terminal:
		return "its organization says the team is gone"
	}
	return "its refreshes were refused"
}

// PendingBinding is a binding a ticked organization's entry needs that no one decided.
type PendingBinding struct {
	Key, Organization, TeamID, Entry string
}

// Pending lists the bindings st's ticked organizations' entries need with no decision
// covering them, each once.
func Pending(st routing.State) []PendingBinding {
	var out []PendingBinding
	for _, t := range st.Teams {
		if t.Personal || t.Policy.Global() || t.Policy.Unset {
			continue
		}
		for _, raw := range t.Policy.Repositories {
			e, ok := routing.ParseEntry(raw, t.Policy.Wildcards)
			if !ok || slices.ContainsFunc(out, func(p PendingBinding) bool { return p.Key == e.Key() }) || decided(st, e) {
				continue
			}
			out = append(out, PendingBinding{Key: e.Key(), Organization: t.OrganizationID, TeamID: t.TeamID, Entry: raw})
		}
	}
	return out
}

// decided reports whether a decision covers what e needs: its key's, or for an exact
// entry its owner's, whichever organization it names.
func decided(st routing.State, e routing.Entry) bool {
	if _, ok := st.Bindings[e.Key()]; ok {
		return true
	}
	_, ok := st.Bindings[routing.Owner(e.Key())]
	return !e.Wildcard() && ok
}

// RoutedRepository is where Route sends a session started in the working copy whose git
// directory is gitDir ("" outside git), and the team it goes to when it is sent.
func RoutedRepository(r Routing, gitDir string, repoErr error, now time.Time) (Check, *routing.Team) {
	if repoErr != nil {
		return Check{Status: Fail, Detail: repoErr.Error()}, nil
	}
	if r.unusable() {
		return Check{Status: Fail, Detail: "nothing is sent without a routing state", Fix: "terma setup"}, nil
	}
	loc, where := routing.Location{Kind: routing.OutsideGit}, "outside any repository with a hosted origin"
	if origin := gitx.RepositoryFS(gitDir); gitDir != "" && origin != "" {
		loc, where = routing.Location{Kind: routing.Repository, Repository: origin}, origin
	}
	d := routing.NewRouter(r.State).Route(loc, now)
	var team *routing.Team
	if i := slices.IndexFunc(r.State.Teams, func(t routing.Team) bool { return t.EnrollmentID == d.EnrollmentID }); i >= 0 {
		team = &r.State.Teams[i]
	}
	switch {
	case d.Outcome == routing.Send:
		detail := where + " goes to " + r.label(*team)
		if d.Entry != "" {
			detail += " (" + d.Entry + ")"
		}
		return Check{Status: Pass, Detail: detail}, team
	case d.Reason == routing.ReasonPending:
		return Check{Status: Warn, Detail: where + " is held: " + d.Authority.Key + " is undecided",
			Fix: "terma teams --bind " + d.Authority.Key + "=<organization> (or --refuse " + d.Authority.Key + ")"}, nil
	case team != nil:
		return Check{Status: Warn, Detail: where + " is held: " + r.label(*team) + " would collect it, but is " + words(string(d.Blocked)), Fix: "terma teams"}, nil
	case d.Reason == routing.ReasonBoundNotCollected:
		return Check{Status: Warn, Detail: where + " is bound to " + r.org(d.Authority.Organization) + ", and no ticked team of it collects it, so nothing here is collected"}, nil
	}
	return Check{Status: Warn, Detail: where + ": no ticked team collects it, so nothing here is collected"}, nil
}

// TeamRows are doctor's context on a machine that routes by teams: each ticked team, its
// state and last error, its key and what it lists; the bindings; what is undecided; and
// what Mixed runs dropped.
func TeamRows(r Routing, now time.Time) []Row {
	if r.unusable() {
		return []Row{{"Teams", "none: no usable routing state, so nothing is sent"}}
	}
	var rows []Row
	label := "Teams"
	for _, t := range r.State.Teams {
		key := "no key yet"
		if r.Key != nil {
			key = cmp.Or(r.Key(t.EnrollmentID), key)
		}
		lists := strings.Join(t.Policy.Repositories, ", ")
		switch {
		case t.Personal && t.Policy.Global():
			lists = "everything not bound to an organization"
		case lists == "":
			lists = "nothing until an admin chooses repositories"
		}
		state := words(string(t.StateAt(now)))
		if t.LastError != "" {
			state += ", last refresh " + words(string(t.LastError))
		}
		rows = append(rows, Row{label, fmt.Sprintf("%s — %s, key %s: %s", r.label(t), state, key, lists)})
		label = ""
	}
	if len(rows) == 0 {
		rows = append(rows, Row{"Teams", "none ticked — run `terma teams`"})
	}
	keys := make([]string, 0, len(r.State.Bindings))
	for k := range r.State.Bindings {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	label = "Bindings"
	for _, k := range keys {
		b := r.State.Bindings[k]
		who := r.org(b.Organization)
		if b.Refused {
			who = "refused to every organization"
		}
		rows = append(rows, Row{label, k + " → " + who})
		label = ""
	}
	for _, p := range Pending(r.State) {
		rows = append(rows, Row{"Undecided", p.Key + ", needed by " + p.Entry})
	}
	if len(r.MixedSessions) > 0 || r.OlderMixed > 0 || r.MixedDropped > 0 {
		rows = append(rows, Row{"Mixed", MixedSummary(r)})
		for _, s := range r.MixedSessions {
			rows = append(rows, Row{"", "session " + s})
		}
	}
	return append(rows, r.blockedRows()...)
}

// blockedRows say what the relay held, then dropped, for each pending binding and blocked
// team, as its last stop counted it.
func (r Routing) blockedRows() []Row {
	var rows []Row
	label := "Relay held"
	for _, k := range slices.Sorted(maps.Keys(r.Blocked)) {
		what := k
		switch kind, name, _ := strings.Cut(k, ":"); kind {
		case "pending":
			what = name + ", undecided"
		case "team":
			what = name + ", no longer ticked"
			if i := slices.IndexFunc(r.State.Teams, func(t routing.Team) bool { return t.EnrollmentID == name }); i >= 0 {
				what = r.label(r.State.Teams[i]) + ", which could not send"
			}
		case "overflow":
			what = "other blockers"
		}
		rows = append(rows, Row{label, fmt.Sprintf("%d record(s) for %s", r.Blocked[k], what)})
		label = ""
	}
	return rows
}
