package routing

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// accountKeyLen matches auth.AccountKeyLen without pulling keychain dependencies into
// internal/routing (which hooks import).
const accountKeyLen = 16

// ValidAccount reports whether a names a routing state owner: either a 16-hex account key
// or sendas.NoPersonLane for a server-key profile.
func ValidAccount(a string) bool {
	if a == sendas.NoPersonLane {
		return true
	}
	return len(a) == accountKeyLen && strings.Trim(a, "0123456789abcdef") == ""
}

// State is one profile's routing state: the teams this machine sends to, in order. It is
// the only store of them, and Route reads nothing else.
type State struct {
	// StateID uniquely identifies this state instance; Create and Replace mint a fresh
	// random ID so a Delete-then-Create or Replace over a corrupt file never ABA-matches
	// an Expect captured from an earlier state.
	StateID string `json:"state_id,omitempty"`
	// Generation counts all writes; Update, Create, and Replace advance it.
	Generation int64 `json:"generation"`
	// ConsentGeneration counts writes that change Teams membership/order/identity
	// (Select, Deselect, Create, Replace). Background policy
	// refreshes advance Generation without advancing ConsentGeneration, so interactive
	// consent editors (terma teams) can CAS on ExpectConsent() without failing on every
	// minute's refresh.
	ConsentGeneration int64 `json:"consent_generation,omitempty"`
	// Profile is the profile the state belongs to; a state for another is never read.
	Profile string `json:"profile"`
	// Account is the lane that owns this state: a 16-hex account key for a signed-in user,
	// or sendas.NoPersonLane for a server-key profile.
	Account string `json:"account"`
	// Teams are the selected teams, organizations first and personal, if selected, last.
	Teams []Team `json:"teams"`
}

// Expect returns the compare-and-set precondition matching this exact state snapshot
// (its account, state instance ID, and write generation).
func (s State) Expect() Expect {
	g := s.Generation
	return Expect{Account: s.Account, StateID: s.StateID, Generation: &g}
}

// ExpectConsent returns the compare-and-set precondition matching this state's account,
// state instance ID, and consent generation, allowing concurrent policy refreshes while
// rejecting concurrent team edits or account switches.
func (s State) ExpectConsent() Expect {
	g := s.ConsentGeneration
	return Expect{Account: s.Account, StateID: s.StateID, ConsentGeneration: &g}
}

// SameGeneration reports whether b is the same generation of the state a was: the same
// state instance, write count, owning account, and enrollments, so a state replaced for
// another account or recreated from scratch is never taken for the one it replaced.
func SameGeneration(a, b State) bool {
	return a.StateID == b.StateID && a.Generation == b.Generation && a.Account == b.Account &&
		slices.EqualFunc(a.Teams, b.Teams, func(x, y Team) bool { return x.EnrollmentID == y.EnrollmentID })
}

// Team is one enrollment: a team selected on this machine, with its last validated
// policy and what its refreshes have reported since.
type Team struct {
	// EnrollmentID names this selection; ticking the team again is a new one.
	EnrollmentID   string `json:"enrollment_id"`
	OrganizationID string `json:"organization_id"`
	// AuthURL is the sign-in and API environment of the team's organization.
	AuthURL string `json:"auth_url"`
	// TeamID is the team's project ID.
	TeamID   string `json:"team_id"`
	Personal bool   `json:"personal,omitempty"`
	// Epoch advances on every state-changing refresh result (policy success, Terminal,
	// OrgAccessLost, SignIn): only a refresh started in the current epoch can change the
	// team's grant or error state (ApplyRefresh, ApplyOrgAccessLost).
	Epoch int64 `json:"epoch"`
	// Policy is the last validated policy. A blocked team keeps blocking with its list.
	Policy config.Policy `json:"policy"`
	// Refusals counts refreshes refused in a row for a known terminal reason.
	Refusals int `json:"refusals,omitempty"`
	// RefusedAt is when the team became Refused, which the daily recheck reads; zero
	// while it is not.
	RefusedAt time.Time `json:"refused_at,omitzero"`
	// SignInNeeded is set while the profile's sign-in is refused.
	SignInNeeded bool `json:"sign_in_needed,omitempty"`
	// LastError is the class of the last failed refresh, for doctor; "" after a success.
	LastError ErrorClass `json:"last_error,omitempty"`
}

// TeamState is whether a team can send now, and if not, why it blocks.
type TeamState string

// Team states. Every state but Active blocks. Route passes a Refused team over, so the
// next team gets what its list matches; under any other state those records are held,
// then dropped, and never passed on.
const (
	Active       TeamState = "active"
	Expired      TeamState = "expired"
	SignInNeeded TeamState = "sign_in_needed"
	Suspended    TeamState = "suspended"
	Refused      TeamState = "refused"
)

// StateAt is t's state at now, worked out from the clock so an expired policy blocks
// even when nothing has written the state since.
func (t Team) StateAt(now time.Time) TeamState {
	switch {
	case t.Refusals >= 2:
		return Refused
	case t.SignInNeeded:
		// Ahead of a suspension: both block, and signing in is what the developer can do.
		return SignInNeeded
	case t.Refusals == 1:
		return Suspended
	case t.Policy.FetchedAt.IsZero() || t.Policy.Expired(now):
		return Expired
	}
	return Active
}

// refused reports whether t is Refused for good, so Route passes it over; unlike StateAt
// it needs no clock.
func (t Team) refused() bool { return t.Refusals >= 2 }

// owns reports whether p was fetched for t: its team, under its organization's login, in
// its environment. Each must be named, since two blanks establish no scope: every fetch
// stamps the organization and environment (internal/policy). Any other policy never
// becomes t's grant.
func (t Team) owns(p config.Policy) bool {
	return t.TeamID != "" && t.OrganizationID != "" && t.AuthURL != "" &&
		p.TeamID == t.TeamID && p.OrganizationID == t.OrganizationID && p.AuthURL == t.AuthURL
}

// collectsEverything reports whether t's policy collects every location it may: only a
// personal team's can.
func (t Team) collectsEverything() bool {
	return t.Personal && t.Policy.Global()
}

// ErrorClass is how a refresh failed, which decides what it does to the team.
type ErrorClass string

// Refresh outcomes.
const (
	// Transient covers network errors, 5xx, 429 and any 403 without a known terminal
	// reason: the last policy stays in force until it expires.
	Transient ErrorClass = "transient"
	// SignIn is a 401, retired sign-in, or missing/wrong-environment login: the team
	// blocks until the developer signs in again. It advances the epoch so an in-flight
	// success cannot clear it.
	SignIn ErrorClass = "sign_in"
	// Terminal is a 403 the backend says is final for the team (PROJECT_NOT_FOUND): the
	// first suspends the team, the second in a row makes it Refused.
	Terminal ErrorClass = "terminal"
	// OrgAccessLost is a per-request 403 ORG_NOT_ACCESSIBLE: the developer no longer has
	// access to the organization. It makes matching-epoch enrollments in that organization
	// Refused at once without waiting for a second refusal.
	OrgAccessLost ErrorClass = "org_access_lost"
)

// Refresh is what one policy fetch for an enrollment returned: a policy, or how it
// failed. Epoch is the team's epoch when the request started.
type Refresh struct {
	EnrollmentID string
	Epoch        int64
	Policy       *config.Policy
	Failure      ErrorClass
	// At is when the result came back, which stamps a team that becomes Refused; zero is
	// now.
	At time.Time
}

// sanitizeTeamPolicy downgrades a global policy on a non-personal team to repository
// mode with no repositories so only a personal organization may collect everything.
func sanitizeTeamPolicy(pol config.Policy, personal bool) config.Policy {
	if pol.Global() && !personal {
		pol.Mode, pol.Repositories = config.ModeRepo, nil
	}
	return pol
}

// ApplyRefresh records r on its enrollment and reports whether it changed anything. A
// result for an ended enrollment, or from a request started in an older epoch, is
// discarded (returning false, nil) so neither a delayed success nor a delayed
// refusal/transient can overwrite a newer state transition; two concurrent refreshers on
// the same team intentionally let the first win and discard the second. A policy whose
// revision moved backwards is refused.
//
// An OrgAccessLost failure passed here refuses only r.EnrollmentID; callers enforcing an
// org-wide 403 ORG_NOT_ACCESSIBLE across sibling enrollments should call
// ApplyOrgAccessLost with epochs snapshotted before the request started.
func (s *State) ApplyRefresh(r Refresh) (bool, error) {
	t := s.team(r.EnrollmentID)
	if t == nil || r.Epoch != t.Epoch {
		return false, nil
	}
	wasRefused := t.Refusals >= 2
	switch {
	case r.Policy != nil:
		if !t.owns(*r.Policy) || r.Policy.Revision < t.Policy.Revision {
			return false, errors.New("the refreshed policy is another team's, organization's or environment's, or an older revision")
		}
		pol := sanitizeTeamPolicy(*r.Policy, t.Personal)
		t.Policy, t.Refusals, t.RefusedAt, t.SignInNeeded, t.LastError = pol, 0, time.Time{}, false, ""
		t.Epoch++
	case r.Failure == Terminal:
		t.Refusals++
		t.Epoch++
		t.LastError = Terminal
		if !wasRefused && t.Refusals >= 2 {
			t.RefusedAt = refusedTimestamp(r.At)
		}
	case r.Failure == OrgAccessLost:
		t.markOrgAccessLost(refusedTimestamp(r.At))
	case r.Failure == SignIn:
		t.SignInNeeded, t.LastError = true, SignIn
		t.Epoch++
	case r.Failure == Transient:
		t.LastError = Transient
	default:
		return false, fmt.Errorf("refresh for %s carries neither a policy nor a failure", r.EnrollmentID)
	}
	return true, nil
}

// ApplyOrgAccessLost refuses every enrollment in organizationID and authURL whose epoch
// still matches epochs[enrollmentID] (captured before the policy request started). Each
// refused enrollment becomes Refused (OrgAccessLost) with its epoch advanced and RefusedAt
// stamped if newly refused. Enrollments in other organizations or environments, or
// siblings whose epoch already advanced (for example via a newer successful refresh), are
// untouched.
func (s *State) ApplyOrgAccessLost(organizationID, authURL string, epochs map[string]int64, at time.Time) bool {
	if organizationID == "" || authURL == "" || len(epochs) == 0 {
		return false
	}
	var changed bool
	ts := refusedTimestamp(at)
	for i := range s.Teams {
		t := &s.Teams[i]
		if t.OrganizationID != organizationID || t.AuthURL != authURL {
			continue
		}
		wantEpoch, ok := epochs[t.EnrollmentID]
		if !ok || t.Epoch != wantEpoch {
			continue
		}
		t.markOrgAccessLost(ts)
		changed = true
	}
	return changed
}

func (t *Team) markOrgAccessLost(ts time.Time) {
	wasRefused := t.Refusals >= 2
	if t.Refusals < 2 {
		t.Refusals = 2
	}
	t.Epoch++
	t.LastError = OrgAccessLost
	if !wasRefused {
		t.RefusedAt = ts
	}
}

func refusedTimestamp(at time.Time) time.Time {
	if at.IsZero() {
		return time.Now().UTC()
	}
	return at.UTC()
}

// team returns a mutable pointer to enrollment id within the routing package.
func (s *State) team(id string) *Team {
	for i := range s.Teams {
		if s.Teams[i].EnrollmentID == id {
			return &s.Teams[i]
		}
	}
	return nil
}

// Team returns a defensive copy of the selected team whose enrollment is id, or false.
func (s State) Team(id string) (Team, bool) {
	t := s.team(id)
	if t == nil {
		return Team{}, false
	}
	cp := *t
	cp.Policy.Repositories = slices.Clone(t.Policy.Repositories)
	return cp, true
}

// Select adds t under a new enrollment, organizations before personal, and returns the
// enrollment ID. t must carry its first validated policy: there is no team waiting for one.
func (s *State) Select(t Team) (string, error) {
	if t.Policy.FetchedAt.IsZero() || t.OrganizationID == "" || !t.owns(t.Policy) {
		return "", errors.New("a team is selected only with its organization and its own validated policy")
	}
	if t.Policy.Global() && !t.Personal {
		return "", errors.New("only a personal organization's policy may collect everything")
	}
	for _, have := range s.Teams {
		if have.TeamID == t.TeamID {
			return "", fmt.Errorf("team %s is already selected", t.TeamID)
		}
		if t.Personal && have.Personal {
			return "", errors.New("only one personal team can be selected")
		}
	}
	id, err := NewEnrollmentID()
	if err != nil {
		return "", err
	}
	t.EnrollmentID, t.Epoch, t.Refusals, t.RefusedAt, t.SignInNeeded, t.LastError = id, 0, 0, time.Time{}, false, ""
	if t.Personal || len(s.Teams) == 0 || !s.Teams[len(s.Teams)-1].Personal {
		s.Teams = append(s.Teams, t)
	} else {
		s.Teams = append(s.Teams[:len(s.Teams)-1:len(s.Teams)-1], t, s.Teams[len(s.Teams)-1])
	}
	return id, nil
}

// Deselect ends an enrollment.
func (s *State) Deselect(enrollmentID string) bool {
	for i, t := range s.Teams {
		if t.EnrollmentID == enrollmentID {
			s.Teams = append(s.Teams[:i:i], s.Teams[i+1:]...)
			return true
		}
	}
	return false
}

// Selected reports whether enrollmentID is still selected.
func (s State) Selected(enrollmentID string) bool {
	return s.team(enrollmentID) != nil
}

// check reports the first way s breaks the shape every reader relies on.
func (s State) check() error {
	if !ValidAccount(s.Account) {
		return fmt.Errorf("invalid account %q in routing state", s.Account)
	}
	seen := map[string]bool{}
	for i, t := range s.Teams {
		switch {
		case t.EnrollmentID == "" || seen[t.EnrollmentID]:
			return fmt.Errorf("team %d has a missing or repeated enrollment", i)
		case t.Personal && i != len(s.Teams)-1:
			return errors.New("the personal team must be last")
		case t.Policy.Global() && !t.Personal:
			return fmt.Errorf("team %s collects everything but is not personal", t.TeamID)
		case !t.owns(t.Policy):
			return fmt.Errorf("team %s holds a policy fetched for another team, organization or environment", t.TeamID)
		}
		seen[t.EnrollmentID] = true
	}
	return nil
}

// NewEnrollmentID names a new enrollment.
func NewEnrollmentID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
