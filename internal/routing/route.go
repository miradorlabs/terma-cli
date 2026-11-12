package routing

import "time"

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
	// Repository is the origin as gitx.RepositoryID makes it, when Kind is Repository.
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

// Decision is where Route sends a record, and why.
type Decision struct {
	Outcome Outcome
	// EnrollmentID is the team sent to, or holding the record; "" for a drop, or an
	// unknown record waiting for its placement.
	EnrollmentID string
	// Entry is the policy entry that matched or blocked, so a route can be explained.
	Entry string
	// Authority is who decided about the repository.
	Authority Authority
	// Reason says why, in a word doctor and the relay's counters can group by.
	Reason string
}

// Route is where a record that ran at loc goes: the first selected team whose policy
// matches it, organizations before personal. An organization sends only what the
// developer bound to it, by both the repository's authority and the matching entry's
// binding; a team that cannot send holds what its last list matches instead of passing
// it on; personal takes only what no organization is bound to.
func Route(loc Location, s State, now time.Time) Decision {
	switch loc.Kind {
	case Mixed:
		return Decision{Outcome: Drop, Reason: "mixed"}
	case OutsideGit:
		return s.routePersonal("", Authority{Kind: Free}, now)
	case Repository:
		return s.routeRepository(loc.Repository, now)
	}
	if !loc.Narrow && s.unknownToPersonal(now) {
		return Decision{Outcome: Send, EnrollmentID: s.Teams[0].EnrollmentID, Reason: "unknown_to_personal"}
	}
	return Decision{Outcome: Hold, Reason: "unknown"}
}

// UnknownToPersonal reports whether an unknown record arriving now may go to personal,
// which the relay records on arrival (Location.Narrow).
func UnknownToPersonal(s State, now time.Time) bool { return s.unknownToPersonal(now) }

// unknownToPersonal: personal is the only team, is Active and collects everything, and
// nothing is bound to an organization the record could belong to.
func (s State) unknownToPersonal(now time.Time) bool {
	return len(s.Teams) == 1 && s.Teams[0].collectsEverything() && s.Teams[0].StateAt(now) == Active &&
		!s.boundToAnOrganization()
}

func (s State) routeRepository(repo string, now time.Time) Decision {
	auth := s.authority(repo)
	if auth.Kind == Free || auth.Kind == RefusedAll {
		return s.routePersonal(repo, auth, now)
	}
	// Among the organization teams whose entry matches, the most specific entry first,
	// then the developer's order.
	var best *Team
	var bestEntry Entry
	for i := range s.Teams {
		t := &s.Teams[i]
		if t.Personal {
			continue
		}
		e, ok := t.match(repo)
		if !ok {
			continue
		}
		if auth.Kind == Bound && (t.OrganizationID != auth.Organization || !s.consents(e, t.OrganizationID)) {
			continue
		}
		if best == nil || moreSpecific(e, bestEntry) {
			best, bestEntry = t, e
		}
	}
	switch {
	case best == nil:
		return Decision{Outcome: Drop, Authority: auth, Reason: "bound_not_collected"}
	case auth.Kind == Pending:
		return Decision{Outcome: Hold, EnrollmentID: best.EnrollmentID, Entry: bestEntry.Raw, Authority: auth, Reason: "pending"}
	case best.StateAt(now) != Active:
		return Decision{Outcome: Hold, EnrollmentID: best.EnrollmentID, Entry: bestEntry.Raw, Authority: auth, Reason: string(best.StateAt(now))}
	}
	return Decision{Outcome: Send, EnrollmentID: best.EnrollmentID, Entry: bestEntry.Raw, Authority: auth, Reason: "matched"}
}

// routePersonal sends repo, or a location outside git when repo is "", to personal when
// its policy collects it, while it is Active; a blocked personal team holds it.
func (s State) routePersonal(repo string, auth Authority, now time.Time) Decision {
	if len(s.Teams) == 0 || !s.Teams[len(s.Teams)-1].Personal {
		return Decision{Outcome: Drop, Authority: auth, Reason: "no_match"}
	}
	p := s.Teams[len(s.Teams)-1]
	d := Decision{Outcome: Send, EnrollmentID: p.EnrollmentID, Authority: auth, Reason: "personal"}
	if !p.collectsEverything() {
		e, ok := p.match(repo)
		if repo == "" || !ok {
			return Decision{Outcome: Drop, Authority: auth, Reason: "no_match"}
		}
		d.Entry = e.Raw
	}
	if st := p.StateAt(now); st != Active {
		d.Outcome, d.Reason = Hold, string(st)
	}
	return d
}

// OrganizationOf is the organization a location belongs to, for telling when a run
// crosses organizations: the one its repository is bound to, else the one of the team
// Route sends or holds it for, personal included. Pending belongs to no organization
// a run can share, so it reports pending. False when nothing collects or binds it.
func OrganizationOf(loc Location, s State, now time.Time) (org string, pending, ok bool) {
	if loc.Kind == Repository {
		switch auth := s.authority(loc.Repository); auth.Kind {
		case Bound:
			return auth.Organization, false, true
		case Pending:
			return "", true, true
		}
	}
	d := Route(Location{Kind: loc.Kind, Repository: loc.Repository, Narrow: true}, s, now)
	if d.Outcome == Drop || d.EnrollmentID == "" {
		return "", false, false
	}
	return s.team(d.EnrollmentID).OrganizationID, false, true
}
