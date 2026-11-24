package doctor

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// KeyTeams is doctor's check key for the profile's multi-org routing state and enrollments.
const KeyTeams = "teams"

// Routing is the profile's routing state and runtime observations as doctor judges them.
// Env.Routing is nil only in synthetic single-team doctor tests.
type Routing struct {
	State routing.State
	// Err says why routing/<profile>.json could not be read; Missing that it does not exist.
	Err     error
	Missing bool
	// ActiveAccount is the active profile lane: a 16-hex account key, sendas.NoPersonLane
	// for a server-key profile, "" when signed out, or sendas.AccountUnreadable when the
	// sign-in and profile account cannot be read or paired. State.Account != ActiveAccount
	// fails closed across hooks, relay, and delivery.
	ActiveAccount string
	// ServerKey is true when the profile signs in with a team server key (no_person lane).
	ServerKey bool
	// SingleTeamLabel is TeamLabel(cfg), used when one selected team has an unset policy.
	SingleTeamLabel string
	// Teams and Orgs name teams and organizations by ID.
	Teams, Orgs map[string]string
	// Mixed are sessions whose current run is Mixed (runs.Mixed); MixedDropped counts
	// records the relay dropped from Mixed runs.
	Mixed        []runs.MixedRun
	MixedDropped int
	// Blocked counts records the relay held and dropped by blocker ("pending:<key>",
	// "team:<enrollment>", "overflow").
	Blocked map[string]int
}

// label names t by its organization and team name, falling back to IDs.
func (r Routing) label(t routing.Team) string {
	name := cmp.Or(r.Teams[t.TeamID], t.TeamID)
	if org := r.Orgs[t.OrganizationID]; org != "" && org != name && !strings.HasPrefix(name, org) {
		return org + " " + name
	}
	return name
}

func (r Routing) org(id string) string { return cmp.Or(r.Orgs[id], id) }

func (r Routing) setupFix() string {
	if r.ServerKey {
		return "TERMA_API_KEY=<the team's server key> terma setup"
	}
	return "terma setup"
}

func (r Routing) teamsFix() string {
	if r.ServerKey {
		return r.setupFix()
	}
	return "terma teams"
}

// unusable reports whether r's routing state fails closed (nothing is sent).
func (r Routing) unusable() bool {
	return r.Err != nil || r.Missing || r.ActiveAccount == "" || r.ActiveAccount == sendas.AccountUnreadable || r.State.Account != r.ActiveAccount
}

// TeamsCheck judges the profile's routing state and ticked enrollments. Missing, unreadable,
// or wrong-account routing state fails closed. Suspended/refused enrollments and undecided
// bindings fail; expired policies and SignInNeeded warn (remediated with a single `terma setup`).
func TeamsCheck(r Routing, now time.Time) Check {
	switch {
	case r.Err != nil:
		return Check{Status: Fail, Detail: "this machine's routing state cannot be read, so nothing is sent: " + r.Err.Error(), Fix: r.setupFix()}
	case r.Missing:
		return Check{Status: Fail, Detail: "this profile has no routing state, so nothing is sent", Fix: r.setupFix()}
	case r.ActiveAccount == "":
		return Check{Status: Fail, Detail: "not signed in, so this profile's routing state is inactive and nothing is sent", Fix: r.setupFix()}
	case r.ActiveAccount == sendas.AccountUnreadable:
		return Check{Status: Fail, Detail: "this profile's sign-in and account cannot be read or paired, so nothing is sent", Fix: r.setupFix()}
	case r.State.Account != r.ActiveAccount:
		return Check{Status: Fail, Detail: "this profile's routing state belongs to another account, so nothing is sent", Fix: r.setupFix()}
	case len(r.State.Teams) == 0:
		return Check{Status: Warn, Detail: "no team is ticked, so nothing is collected", Fix: r.teamsFix()}
	}
	var parts, failing, warning []string
	var signInFix, failFix, warnFix string
	for _, t := range r.State.Teams {
		st := t.StateAt(now)
		parts = append(parts, r.label(t)+" ("+words(string(st))+")")
		switch st {
		case routing.Suspended, routing.Refused:
			failing = append(failing, r.label(t)+" is "+words(string(st))+" ("+refusalWords(t.LastError)+"), and blocks what it lists")
			if r.ServerKey {
				failFix = cmp.Or(failFix, r.setupFix())
			} else {
				failFix = cmp.Or(failFix, "terma teams --release "+quoteWord(cmp.Or(r.Teams[t.TeamID], t.TeamID))+" (or wait for its policy to come back)")
			}
		case routing.SignInNeeded:
			warning = append(warning, r.label(t)+" needs you to sign in again, and blocks what it lists")
			signInFix = r.setupFix()
		case routing.Expired:
			warning = append(warning, r.label(t)+"'s policy has not been refreshed for over a week, so it blocks what it lists")
			warnFix = cmp.Or(warnFix, "check the network; the relay refreshes it every minute")
		}
	}
	for _, p := range Pending(r.State) {
		failing = append(failing, p.Key+" is undecided, so what "+p.Entry+" matches is held")
		if r.ServerKey {
			failFix = cmp.Or(failFix, r.setupFix())
		} else {
			failFix = cmp.Or(failFix, "terma teams --bind "+p.Key+"="+quoteWord(r.org(p.Organization))+" (or --refuse "+p.Key+")")
		}
	}
	fix := cmp.Or(signInFix, failFix, warnFix)
	mixed := ""
	if len(r.Mixed) > 0 || r.MixedDropped > 0 {
		mixed = MixedSummary(r)
	}
	withMixed := func(s string) string {
		if mixed == "" {
			return s
		}
		return s + "; " + mixed
	}
	switch {
	case len(failing) > 0:
		return Check{Status: Fail, Detail: withMixed(strings.Join(append(failing, warning...), "; ")), Fix: fix}
	case len(warning) > 0:
		return Check{Status: Warn, Detail: withMixed(strings.Join(warning, "; ")), Fix: fix}
	}
	return Check{Status: Pass, Detail: withMixed(strings.Join(parts, ", "))}
}

// MixedSummary says how many session runs are Mixed, by why, and what the relay dropped
// from them.
func MixedSummary(r Routing) string {
	by, older := map[runs.Reason]int{}, 0
	for _, m := range r.Mixed {
		by[m.Why.Reason]++
		if !m.Recent {
			older++
		}
	}
	s := fmt.Sprintf("%d session run(s) Mixed, sending no telemetry until they resume", len(r.Mixed))
	if len(r.Mixed) > 0 {
		s += ": " + ReasonCounts(by)
	}
	if older > 0 {
		s += fmt.Sprintf(" (%d idle for over a day)", older)
	}
	if r.MixedDropped > 0 {
		s += fmt.Sprintf("; the relay dropped %d record(s) from such runs", r.MixedDropped)
	}
	return s
}

// ReasonCounts says how many runs each reason turned Mixed, most frequent first.
func ReasonCounts(by map[runs.Reason]int) string {
	reasons := slices.Collect(maps.Keys(by))
	slices.SortFunc(reasons, func(a, b runs.Reason) int { return cmp.Or(by[b]-by[a], strings.Compare(string(a), string(b))) })
	parts := make([]string, 0, len(reasons))
	for _, r := range reasons {
		parts = append(parts, fmt.Sprintf("%d %s", by[r], reasonWords(r)))
	}
	return strings.Join(parts, ", ")
}

func reasonWords(r runs.Reason) string {
	switch r {
	case runs.Crossed:
		return "crossed organizations"
	case runs.UnknownReach:
		return "ran a shell command whose reach is not known"
	case runs.LocationBound:
		return "touched more places than the run ledger keeps"
	case runs.Aside:
		return "crossed while the run ledger was busy"
	}
	return "with a run ledger that cannot be read"
}

func whyWords(w runs.Why) string {
	s := reasonWords(w.Reason)
	switch {
	case w.Where == "":
	case w.Reason == runs.Crossed:
		s += ", into " + w.Where
	case w.Reason == runs.UnknownReach:
		s += ": " + w.Where
	}
	return s
}

func words(s string) string { return strings.ReplaceAll(s, "_", " ") }

func quoteWord(s string) string {
	if s == "" || strings.ContainsFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("._/@:=-", r)
	}) {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

func refusalWords(c routing.ErrorClass) string {
	switch c {
	case routing.OrgAccessLost:
		return "you no longer have access to its organization"
	case routing.Terminal:
		return "its organization says the team is gone"
	}
	return "its refreshes were refused"
}

// PendingBinding is a binding a ticked organization's entry needs that no one has decided.
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

func decided(st routing.State, e routing.Entry) bool {
	if _, ok := st.Bindings[e.Key()]; ok {
		return true
	}
	_, ok := st.Bindings[routing.Owner(e.Key())]
	return !e.Wildcard() && ok
}

// RoutedRepository is where Router.Route sends a session started in gitDir ("" outside
// git), and the team it goes to when sent.
func RoutedRepository(r Routing, gitDir string, repoErr error, now time.Time) (Check, *routing.Team) {
	if repoErr != nil {
		return Check{Status: Fail, Detail: repoErr.Error()}, nil
	}
	if r.unusable() {
		return Check{Status: Fail, Detail: "nothing is sent without a usable routing state", Fix: r.setupFix()}, nil
	}
	loc, where := routing.Location{Kind: routing.OutsideGit}, "outside any repository with a hosted origin"
	if origin := gitx.RepositoryFS(gitDir); gitDir != "" && routing.Owner(origin) != "" {
		loc, where = routing.Location{Kind: routing.Repository, Repository: origin}, origin
	}
	d := routing.NewRouter(r.State).Route(loc, now)
	team, hasTeam := r.State.Team(d.EnrollmentID)
	switch {
	case d.Outcome == routing.Send && hasTeam:
		detail := where + " goes to " + r.label(team)
		if d.Entry != "" {
			detail += " (" + d.Entry + ")"
		}
		return Check{Status: Pass, Detail: detail}, &team
	case d.Reason == routing.ReasonPending:
		fix := "terma teams --bind " + d.Authority.Key + "=<organization> (or --refuse " + d.Authority.Key + ")"
		if r.ServerKey {
			fix = r.setupFix()
		}
		return Check{Status: Warn, Detail: where + " is held: " + d.Authority.Key + " is undecided", Fix: fix}, nil
	case hasTeam && d.Blocked == routing.SignInNeeded:
		return Check{Status: Warn, Detail: where + " is held: " + r.label(team) + " would collect it, but needs you to sign in again", Fix: r.setupFix()}, nil
	case hasTeam && (d.Blocked == routing.Suspended || d.Blocked == routing.Refused):
		return Check{Status: Warn, Detail: where + " is held: " + r.label(team) + " would collect it, but is " + words(string(d.Blocked)) + " (" + refusalWords(team.LastError) + ")", Fix: r.teamsFix()}, nil
	case hasTeam:
		return Check{Status: Warn, Detail: where + " is held: " + r.label(team) + " would collect it, but is " + words(string(d.Blocked)), Fix: r.teamsFix()}, nil
	case d.Reason == routing.ReasonBoundNotCollected:
		return Check{Status: Warn, Detail: where + " is bound to " + r.org(d.Authority.Organization) + ", and no ticked team of it collects it, so nothing here is collected"}, nil
	case len(r.State.Teams) == 1 && r.State.Teams[0].Policy.Unset:
		return Check{Status: Warn, Detail: cmp.Or(r.SingleTeamLabel, r.label(r.State.Teams[0])) + " has no collection policy", Fix: NoPolicyStep}, nil
	}
	return Check{Status: Warn, Detail: where + ": no ticked team collects it, so nothing here is collected"}, nil
}

// TeamRows are doctor's context rows for a routed profile: each ticked team and its health,
// bindings, undecided bindings, Mixed runs, and what the relay held.
func TeamRows(r Routing, now time.Time) []Row {
	if r.unusable() {
		return []Row{{"Teams", "none: no usable routing state, so nothing is sent"}}
	}
	var rows []Row
	label := "Teams"
	for _, t := range r.State.Teams {
		lists := strings.Join(t.Policy.Repositories, ", ")
		switch {
		case t.Personal && t.Policy.Global():
			lists = "everything not bound to an organization"
		case lists == "":
			lists = "nothing until an admin chooses repositories"
		}
		state := words(string(t.StateAt(now)))
		if t.LastError == routing.OrgAccessLost {
			state += " (" + refusalWords(t.LastError) + ")"
		} else if t.LastError != "" {
			state += ", last refresh " + words(string(t.LastError))
		}
		rows = append(rows, Row{label, fmt.Sprintf("%s — %s: %s", r.label(t), state, lists)})
		label = ""
	}
	if len(rows) == 0 {
		rows = append(rows, Row{"Teams", "none ticked — run `" + r.teamsFix() + "`"})
	}
	label = "Bindings"
	for _, k := range slices.Sorted(maps.Keys(r.State.Bindings)) {
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

// routedProjectID is the team ID doctor checks against for relay/spool/backend when
// routing by teams: the repository's routed team, else the first active (or selected) team.
func routedProjectID(r Routing, team *routing.Team, now time.Time) string {
	if team != nil {
		return team.TeamID
	}
	if r.unusable() || len(r.State.Teams) == 0 {
		return ""
	}
	for _, t := range r.State.Teams {
		if t.StateAt(now) == routing.Active {
			return t.TeamID
		}
	}
	return r.State.Teams[0].TeamID
}

func (d *run) collectionChecks(timed func(key, name string, fn func() Check)) {
	if d.env.Routing != nil {
		r := *d.env.Routing
		now := time.Now()
		timed(KeyTeams, "teams", func() Check { return TeamsCheck(r, now) })
		timed(KeyProject, "repository collected", func() Check {
			c, team := RoutedRepository(r, d.env.GitDir, d.env.RepoErr, now)
			d.team, d.admitted = team, c.Status == Pass
			if team != nil {
				d.pol = team.Policy
			} else {
				d.pol = config.Policy{}
			}
			d.projectID = routedProjectID(r, team, now)
			return c
		})
		return
	}
	d.pol = d.cfg.Policy.InForce(d.cfg.OrganizationID, d.cfg.AuthURL)
	timed(KeyProject, "repository collected", func() Check {
		if d.env.RepoErr == nil && d.pol.Global() {
			return unlessCaptureOff(d.pol, Check{Status: Pass, Detail: "every session, in global mode: " + GlobalDestination(d.cfg)})
		}
		c := RepositoryCheck(d.pol, TeamLabel(d.cfg), d.env.GitDir, d.env.RepoErr)
		d.admitted = c.Status == Pass
		return unlessCaptureOff(d.pol, c)
	})
	d.projectID = cmp.Or(d.cfg.ProjectID, d.cfg.Policy.TeamID)
}

func (d *run) judgeCommitHooks(now time.Time) CommitHooks {
	if d.env.Routing != nil {
		return judgeCommitHooks(d.env.GitDir, d.team != nil && d.team.Policy.GitHooks, d.team != nil)
	}
	return JudgeCommitHooks(d.env.GitDir, d.pol, now)
}
