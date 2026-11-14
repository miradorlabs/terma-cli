package routing

import (
	"cmp"
	"maps"
	"slices"
	"time"
)

// LocationKind is what is known about where a record ran.
type LocationKind string

// Location kinds.
const (
	// Unknown: no placement covers the record yet, or none can be trusted.
	Unknown LocationKind = "unknown"
	// OutsideGit: a hook saw the session run outside any git checkout with a hosted origin.
	OutsideGit LocationKind = "outside_git"
	// Repository: the record ran in the checkout whose origin Repository names.
	Repository LocationKind = "repository"
	// Mixed: the record's run touched repositories of two organizations.
	Mixed LocationKind = "mixed"
)

// Location is where a record ran.
type Location struct {
	Kind LocationKind
	// Repository is the checkout's origin, when Kind is Repository; any form Normalize
	// reads. One that names no repository is treated as unknown, never as personal's.
	Repository string
	// Narrow is set on an unknown record that arrived while personal could not take
	// unknown records: it never can later, whatever is unticked since.
	Narrow bool
}

// Outcome is what becomes of a record.
type Outcome string

// Outcomes.
const (
	// Send it to the team named.
	Send Outcome = "send"
	// Hold it: briefly in the relay, in the spool for a hook event, then drop it. It is
	// never passed to another team.
	Hold Outcome = "hold"
	// Drop it: it does not leave the machine.
	Drop Outcome = "drop"
)

// Reason says why Route decided as it did, in a word doctor and the relay's counters
// group by.
type Reason string

// Reasons.
const (
	ReasonMatched           Reason = "matched"
	ReasonPersonal          Reason = "personal"
	ReasonUnknownToPersonal Reason = "unknown_to_personal"
	// ReasonBlocked: the team that would collect it cannot send now (Decision.Blocked).
	ReasonBlocked Reason = "blocked"
	// ReasonPending: the developer has not decided the binding the entry needs.
	ReasonPending Reason = "pending"
	ReasonUnknown Reason = "unknown"
	ReasonMixed   Reason = "mixed"
	ReasonNoMatch Reason = "no_match"
	// ReasonBoundNotCollected: bound to an organization none of whose selected teams
	// collects it.
	ReasonBoundNotCollected Reason = "bound_not_collected"
	// ReasonNotReady: the routing state is still Preparing (State.Ready), so nothing is
	// sent until the migration commits it.
	ReasonNotReady Reason = "not_ready"
)

// Decision is where Route sends a record, and why.
type Decision struct {
	Outcome Outcome
	// EnrollmentID is the team sent to, or holding the record; "" for a drop, or an
	// unknown record waiting for its placement.
	EnrollmentID string
	// Entry is the policy entry that matched or blocked, so a route can be explained.
	Entry string
	// Authority is who decided about the repository; for a pending hold, Key is the
	// binding the developer has yet to decide.
	Authority Authority
	Reason    Reason
	// Blocked is the holding team's state when Reason is ReasonBlocked.
	Blocked TeamState
}

// Router routes against one routing state, each team's entries read once: a caller keeps
// one per state it loads, and compiles a new one when it loads a new generation. A Router
// over a state that is not Ready sends nothing: Route drops every record and Recheck keeps
// every queued one waiting for the commit.
type Router struct {
	s     State
	teams []compiled
}

// compiled is one team's entries: exact repositories by key, and wildcards most specific
// first. A policy that collects everything or nothing has neither.
type compiled struct {
	exact map[string]Entry
	wild  []Entry
}

// NewRouter compiles s for routing. It keeps its own copy, so changing s afterwards never
// changes what the router decides.
func NewRouter(s State) *Router {
	s.Teams, s.Bindings = slices.Clone(s.Teams), maps.Clone(s.Bindings)
	r := &Router{s: s, teams: make([]compiled, len(s.Teams))}
	for i, t := range s.Teams {
		c := compiled{exact: map[string]Entry{}}
		if !t.Policy.Global() && !t.Policy.Unset {
			for _, raw := range t.Policy.Repositories {
				e, ok := ParseEntry(raw, t.Policy.Wildcards)
				switch {
				case !ok:
				case e.wildcard:
					c.wild = append(c.wild, e)
				case c.exact[e.key].Raw == "":
					c.exact[e.key] = e
				}
			}
			slices.SortStableFunc(c.wild, func(a, b Entry) int { return cmp.Compare(b.fixed, a.fixed) })
		}
		r.teams[i] = c
	}
	return r
}

// match is the most specific entry of compiled team i that names a normalised repo.
func (r *Router) match(i int, repo string) (Entry, bool) {
	if e, ok := r.teams[i].exact[repo]; ok {
		return e, true
	}
	for _, e := range r.teams[i].wild {
		if e.matches(repo) {
			return e, true
		}
	}
	return Entry{}, false
}

// Route is where a record that ran at loc goes: the first selected team whose policy
// matches it, organizations before personal. An organization sends only what the
// developer bound to it, by both the repository's authority and the matching entry's
// binding; a team that cannot send holds what its last list matches instead of passing
// it on; personal takes only what no organization is bound to.
func (r *Router) Route(loc Location, now time.Time) Decision {
	if !r.s.Ready() {
		return Decision{Outcome: Drop, Reason: ReasonNotReady}
	}
	switch loc.Kind {
	case Mixed:
		return Decision{Outcome: Drop, Reason: ReasonMixed}
	case OutsideGit:
		return r.personal("", Authority{Kind: Free}, now)
	case Repository:
		if repo := Normalize(loc.Repository); validRepo(repo) {
			return r.repository(repo, now)
		}
		loc.Narrow = true
	}
	if !loc.Narrow && r.s.unknownToPersonal(now) {
		return Decision{Outcome: Send, EnrollmentID: r.s.Teams[0].EnrollmentID, Reason: ReasonUnknownToPersonal}
	}
	return Decision{Outcome: Hold, Reason: ReasonUnknown}
}

func (r *Router) repository(repo string, now time.Time) Decision {
	auth := r.s.decided(repo)
	// Among the organization teams the authority lets match, the most specific entry
	// first, then the developer's order.
	best, bestEntry := -1, Entry{}
	for i, t := range r.s.Teams {
		if t.Personal || auth.Kind == RefusedAll {
			continue
		}
		e, ok := r.match(i, repo)
		if !ok || auth.Kind == Bound && (t.OrganizationID != auth.Organization || !r.s.consents(e, t.OrganizationID)) {
			continue
		}
		if best < 0 || moreSpecific(e, bestEntry) {
			best, bestEntry = i, e
		}
	}
	if auth.Kind == Free && best >= 0 {
		auth = Authority{Kind: Pending, Key: bestEntry.Key()}
	}
	switch {
	case auth.Kind == Free || auth.Kind == RefusedAll:
		return r.personal(repo, auth, now)
	case best < 0:
		return Decision{Outcome: Drop, Authority: auth, Reason: ReasonBoundNotCollected}
	}
	t := r.s.Teams[best]
	d := Decision{Outcome: Send, EnrollmentID: t.EnrollmentID, Entry: bestEntry.Raw, Authority: auth, Reason: ReasonMatched}
	switch st := t.StateAt(now); {
	case auth.Kind == Pending:
		d.Outcome, d.Reason = Hold, ReasonPending
	case st != Active:
		d.Outcome, d.Reason, d.Blocked = Hold, ReasonBlocked, st
	}
	return d
}

// personal sends repo, or a location outside git when repo is "", to personal when its
// policy collects it, while it is Active; a blocked personal team holds it.
func (r *Router) personal(repo string, auth Authority, now time.Time) Decision {
	last := len(r.s.Teams) - 1
	if last < 0 || !r.s.Teams[last].Personal {
		return Decision{Outcome: Drop, Authority: auth, Reason: ReasonNoMatch}
	}
	p := r.s.Teams[last]
	d := Decision{Outcome: Send, EnrollmentID: p.EnrollmentID, Authority: auth, Reason: ReasonPersonal}
	if !p.collectsEverything() {
		e, ok := r.match(last, repo)
		if repo == "" || !ok {
			return Decision{Outcome: Drop, Authority: auth, Reason: ReasonNoMatch}
		}
		d.Entry = e.Raw
	}
	if st := p.StateAt(now); st != Active {
		d.Outcome, d.Reason, d.Blocked = Hold, ReasonBlocked, st
	}
	return d
}

// UnknownToPersonal reports whether an unknown record arriving now may go to personal,
// which the relay records on arrival (Location.Narrow).
func UnknownToPersonal(s State, now time.Time) bool { return s.Ready() && s.unknownToPersonal(now) }

// unknownToPersonal: personal is the only team, is Active and collects everything, and
// nothing is bound to an organization the record could belong to.
func (s State) unknownToPersonal(now time.Time) bool {
	return len(s.Teams) == 1 && s.Teams[0].collectsEverything() && s.Teams[0].StateAt(now) == Active &&
		!s.boundToAnOrganization()
}

// OrganizationOf is the organization a location belongs to, for telling when a run
// crosses organizations: the one its repository is bound to, else the one of the team
// Route sends or holds it for, personal included. A pending repository belongs to no
// organization a run can share, so it reports pending. False when nothing collects or
// binds it, and for a repository that names no repository, as Route holds it.
func (r *Router) OrganizationOf(loc Location, now time.Time) (org string, pending, ok bool) {
	if repo := Normalize(loc.Repository); loc.Kind == Repository && validRepo(repo) {
		if a := r.s.decided(repo); a.Kind == Bound {
			return a.Organization, false, true
		}
	}
	loc.Narrow = true
	d := r.Route(loc, now)
	switch {
	case d.Authority.Kind == Pending:
		return "", true, true
	case d.Outcome == Drop || d.EnrollmentID == "":
		return "", false, false
	}
	return r.s.Team(d.EnrollmentID).OrganizationID, false, true
}
