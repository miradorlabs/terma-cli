package routing

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// State is one profile's routing state: the teams this machine sends to, in order, and
// the developer's bindings. It is the only store of either, and Route reads nothing else.
type State struct {
	// Generation counts the writes; Update advances it.
	Generation int64 `json:"generation"`
	// Profile is the profile the state belongs to; a state for another is never read.
	Profile string `json:"profile"`
	// Teams are the selected teams, organizations first and personal, if selected, last.
	Teams []Team `json:"teams"`
	// Bindings are the developer's decisions, by normalised owner or repository.
	Bindings map[string]Binding `json:"bindings,omitempty"`
	// Migration records how this state was made from the profile's single team; nil for
	// a state set up afresh.
	Migration *Migration `json:"migration,omitempty"`
}

// SameGeneration reports whether b is the generation of the state a was: the same number
// of writes, and the same enrollments, so a state lost and made again is never taken for
// the one it replaced.
func SameGeneration(a, b State) bool {
	return a.Generation == b.Generation &&
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
	// Epoch advances on every terminal refusal: only a refresh started in the current
	// epoch can change the team's grant (ApplyRefresh).
	Epoch int64 `json:"epoch"`
	// Policy is the last validated policy. A blocked team keeps blocking with its list.
	Policy config.Policy `json:"policy"`
	// Refusals counts refreshes refused in a row for a known terminal reason.
	Refusals int `json:"refusals,omitempty"`
	// RefusedAt is when the team became Refused, which the daily recheck reads; zero
	// while it is not.
	RefusedAt time.Time `json:"refused_at,omitzero"`
	// SignInNeeded is set while the organization's sign-in is refused.
	SignInNeeded bool `json:"sign_in_needed,omitempty"`
	// LastError is the class of the last failed refresh, for doctor; "" after a success.
	LastError ErrorClass `json:"last_error,omitempty"`
}

// TeamState is whether a team can send now, and if not, why it blocks.
type TeamState string

// Team states. Every state but Active blocks: the records its last list matches are
// held, then dropped, and never passed to the next team.
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
	// SignIn is a 401: the team blocks until the developer signs in again. Like a
	// terminal refusal it advances the epoch, so a success already in flight under the
	// refused sign-in cannot clear it.
	SignIn ErrorClass = "sign_in"
	// Terminal is a 403 the backend says is final (not a member, team deleted): the first
	// suspends the team, the second in a row makes it Refused.
	Terminal ErrorClass = "terminal"
	// OrgAccessLost is a token refresh refused with ORG_NOT_ACCESSIBLE: the developer is
	// no longer a member of the team's organization. It makes the team Refused at once,
	// where Terminal waits for a second refusal: the membership refusal is authoritative,
	// and the revoked session could never fetch the policy that confirms it.
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

// ApplyRefresh records r on its enrollment and reports whether it changed anything. A
// result for an ended enrollment, or from a request started before the last terminal
// refusal or refused sign-in, is discarded, so a success already in flight cannot undo
// either. A policy whose revision moved backwards is refused.
func (s *State) ApplyRefresh(r Refresh) (bool, error) {
	t := s.Team(r.EnrollmentID)
	if t == nil || r.Epoch != t.Epoch {
		return false, nil
	}
	wasRefused := t.Refusals >= 2
	switch {
	case r.Policy != nil:
		if !t.owns(*r.Policy) || r.Policy.Revision < t.Policy.Revision {
			return false, errors.New("the refreshed policy is another team's, organization's or environment's, or an older revision")
		}
		pol := *r.Policy
		if pol.Global() && !t.Personal {
			// Only a personal organization may collect everything: anyone else's such
			// policy collects nothing.
			pol.Mode, pol.Repositories = config.ModeRepo, nil
		}
		t.Policy, t.Refusals, t.RefusedAt, t.SignInNeeded, t.LastError = pol, 0, time.Time{}, false, ""
	case r.Failure == Terminal:
		t.Refusals++
		t.Epoch++
		t.LastError = Terminal
	case r.Failure == OrgAccessLost:
		t.Refusals = 2
		t.Epoch++
		t.LastError = OrgAccessLost
	case r.Failure == SignIn:
		t.SignInNeeded, t.LastError = true, SignIn
		t.Epoch++
	case r.Failure == Transient:
		t.LastError = Transient
	default:
		return false, fmt.Errorf("refresh for %s carries neither a policy nor a failure", r.EnrollmentID)
	}
	if !wasRefused && t.Refusals >= 2 {
		t.RefusedAt = r.At
		if t.RefusedAt.IsZero() {
			t.RefusedAt = time.Now()
		}
		t.RefusedAt = t.RefusedAt.UTC()
	}
	return true, nil
}

// Team is the selected team whose enrollment is id, or nil.
func (s *State) Team(id string) *Team {
	for i := range s.Teams {
		if s.Teams[i].EnrollmentID == id {
			return &s.Teams[i]
		}
	}
	return nil
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
	for key, b := range s.Bindings {
		if t.Personal && !b.Refused && b.Organization == t.OrganizationID {
			return "", fmt.Errorf("%s is bound to the personal organization: unbind it first", key)
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

// Deselect ends an enrollment. Bindings stay: what is bound to the team's organization
// collects nowhere until the developer unbinds it. A legacy project mapped to it maps to
// nothing from now on, so its queued records are dropped.
func (s *State) Deselect(enrollmentID string) bool {
	for i, t := range s.Teams {
		if t.EnrollmentID == enrollmentID {
			s.Teams = append(s.Teams[:i:i], s.Teams[i+1:]...)
			s.Migration.forgetLegacy(enrollmentID)
			return true
		}
	}
	return false
}

// Selected reports whether enrollmentID is still selected.
func (s State) Selected(enrollmentID string) bool {
	return s.Team(enrollmentID) != nil
}

// check reports the first way s breaks the shape every reader relies on.
func (s State) check() error {
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
	for key, b := range s.Bindings {
		if key != Normalize(key) || !isOwnerKey(key) && !validRepo(key) {
			return fmt.Errorf("binding %q is not a normalised owner or repository", key)
		}
		if b.Refused == (b.Organization != "") {
			return fmt.Errorf("binding %q must be either refused or bound to one organization", key)
		}
		if n := len(s.Teams); n > 0 && s.Teams[n-1].Personal && !b.Refused && b.Organization == s.Teams[n-1].OrganizationID {
			return fmt.Errorf("binding %q names the personal organization", key)
		}
	}
	return s.Migration.check(s)
}

// NewEnrollmentID names a new enrollment: a selection (Select), or the migration's of a
// profile's one team.
func NewEnrollmentID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
