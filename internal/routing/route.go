package routing

import (
	"cmp"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
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
	ReasonUnknown Reason = "unknown"
	ReasonNoMatch Reason = "no_match"
)

// Decision is where Route sends a record, and why.
type Decision struct {
	Outcome Outcome
	// EnrollmentID is the team sent to, or holding the record; "" for a drop, or an
	// unknown record waiting for its placement.
	EnrollmentID string
	// Entry is the policy entry that matched or blocked, so a route can be explained.
	Entry  string
	Reason Reason
	// Blocked is the holding team's state when Reason is ReasonBlocked.
	Blocked TeamState
}

// Router routes against one routing state, each team's entries read once: a caller keeps
// one per state it loads, and compiles a new one when it loads a new generation.
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
	s.Teams = slices.Clone(s.Teams)
	r := &Router{s: s, teams: make([]compiled, len(s.Teams))}
	for i, t := range s.Teams {
		c := compiled{exact: map[string]Entry{}}
		if !t.Policy.Global() && !t.Policy.Unset {
			for _, raw := range t.Policy.Repositories {
				e, ok := ParseEntry(raw, t.Policy.Wildcards)
				switch {
				case !ok:
				case e.Wildcard():
					c.wild = append(c.wild, e)
				case c.exact[e.Key()].Raw == "":
					c.exact[e.Key()] = e
				}
			}
			slices.SortStableFunc(c.wild, func(a, b Entry) int { return cmp.Compare(b.Fixed(), a.Fixed()) })
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
		if e.Matches(repo) {
			return e, true
		}
	}
	return Entry{}, false
}

// Route is where a record that ran at loc goes: to the selected organization team whose
// policy matches it, the most recently updated policy first when several do, else to
// personal. A refused team is passed over; any other team that cannot send holds what its
// last list matches instead of passing it on.
func (r *Router) Route(loc Location, now time.Time) Decision {
	switch loc.Kind {
	case OutsideGit:
		return r.personal("", now)
	case Repository:
		if repo := Normalize(loc.Repository); config.IsRepositoryKey(repo) {
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
	best, bestEntry := -1, Entry{}
	for i, t := range r.s.Teams {
		if t.Personal || t.refused() {
			continue
		}
		e, ok := r.match(i, repo)
		if !ok {
			continue
		}
		if best < 0 || newer(t.Policy, r.s.Teams[best].Policy) {
			best, bestEntry = i, e
		}
	}
	if best < 0 {
		return r.personal(repo, now)
	}
	t := r.s.Teams[best]
	d := Decision{Outcome: Send, EnrollmentID: t.EnrollmentID, Entry: bestEntry.Raw, Reason: ReasonMatched}
	if st := t.StateAt(now); st != Active {
		d.Outcome, d.Reason, d.Blocked = Hold, ReasonBlocked, st
	}
	return d
}

// newer reports whether policy a was last updated after b. A policy of unknown age sorts
// after every dated one, so it never takes a repository from one.
func newer(a, b config.Policy) bool {
	switch {
	case a.UpdatedAt.IsZero():
		return false
	case b.UpdatedAt.IsZero():
		return true
	}
	return a.UpdatedAt.After(b.UpdatedAt)
}

// personal sends repo, or a location outside git when repo is "", to personal when its
// policy collects it, while it is Active; a blocked personal team holds it.
func (r *Router) personal(repo string, now time.Time) Decision {
	last := len(r.s.Teams) - 1
	if last < 0 || !r.s.Teams[last].Personal {
		return Decision{Outcome: Drop, Reason: ReasonNoMatch}
	}
	p := r.s.Teams[last]
	d := Decision{Outcome: Send, EnrollmentID: p.EnrollmentID, Reason: ReasonPersonal}
	if !p.collectsEverything() {
		e, ok := r.match(last, repo)
		if repo == "" || !ok {
			return Decision{Outcome: Drop, Reason: ReasonNoMatch}
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
func UnknownToPersonal(s State, now time.Time) bool { return s.unknownToPersonal(now) }

// unknownToPersonalEligible reports whether personal is structurally the sole destination
// for unknown locations (the only team, and it collects everything), regardless of
// whether personal is temporarily blocked.
func (s State) unknownToPersonalEligible() bool {
	return len(s.Teams) == 1 && s.Teams[0].collectsEverything()
}

// unknownToPersonal: personal is the only team, is Active and collects everything.
func (s State) unknownToPersonal(now time.Time) bool {
	return s.unknownToPersonalEligible() && s.Teams[0].StateAt(now) == Active
}
