package routing

import (
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Recheck sends a queued or held record only to its pinned enrollment while that team is
// Active and still covers the location; waits while the team may recover; and discards
// (never re-routing to Personal or another team) on refusal, deselection, a newer policy
// claiming the location, policy narrowing, or Mixed.
func TestRecheckNeverReroutes(t *testing.T) {
	t.Parallel()
	verdict := func(s *State, d Destination) Verdict {
		t.Helper()
		v, _ := NewRouter(*s).Recheck(d, now)
		return v
	}

	s := fixture(t)
	eAcme := enrollment(t, s, "acme-platform")
	d := Destination{EnrollmentID: eAcme, Location: repo("github.com/acme/api")}

	if got := verdict(s, d); got != SendNow {
		t.Fatalf("active matching team: %s, want send", got)
	}

	// Recoverable blocks wait rather than dropping or re-routing.
	for name, block := range map[string]func(*State){
		"expired":        func(st *State) { st.Teams[0].Policy.FetchedAt = now.Add(-config.MaxPolicyAge - time.Hour) },
		"sign_in_needed": func(st *State) { st.Teams[0].SignInNeeded = true },
		"suspended":      func(st *State) { st.Teams[0].Refusals = 1 },
	} {
		st := fixture(t)
		block(st)
		dCur := Destination{EnrollmentID: enrollment(t, st, "acme-platform"), Location: repo("github.com/acme/api")}
		if got := verdict(st, dCur); got != Wait {
			t.Errorf("%s: got %s, want wait", name, got)
		}
	}

	// Terminal states and destination changes discard rather than re-routing to Personal.
	for name, change := range map[string]func(*State){
		"refused": func(st *State) {
			st.Teams[0].Refusals = 2
		},
		"deselected_and_personal_collects": func(st *State) {
			st.Deselect(enrollment(t, st, "acme-platform"))
		},
		"a_newer_policy_claims_it": func(st *State) {
			st.Teams[1].Policy.Repositories = []string{"github.com/acme/*"}
			st.Teams[1].Policy.UpdatedAt = now.Add(-time.Hour)
			st.Teams[0].Policy.UpdatedAt = now.Add(-48 * time.Hour)
		},
		"policy_dropped_repo": func(st *State) {
			st.Teams[0].Policy.Repositories = []string{"github.com/acme/other"}
		},
		"reselected_new_enrollment": func(st *State) {
			st.Deselect(enrollment(t, st, "acme-platform"))
			if _, err := st.Select(team("acme", "acme-platform", "github.com/acme/*")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		st := fixture(t)
		origID := enrollment(t, st, "acme-platform")
		change(st)
		if got := verdict(st, Destination{EnrollmentID: origID, Location: repo("github.com/acme/api")}); got != Discard {
			t.Errorf("%s: got %s, want drop", name, got)
		}
	}

	if got := verdict(s, Destination{EnrollmentID: "", Location: repo("github.com/acme/api")}); got != Discard {
		t.Fatalf("empty enrollment: %s, want drop", got)
	}
	if got := verdict(s, Destination{EnrollmentID: "never-selected", Location: repo("github.com/acme/api")}); got != Discard {
		t.Fatalf("unknown enrollment: %s, want drop", got)
	}
	if got := verdict(s, Destination{EnrollmentID: eAcme, Location: Location{Kind: Mixed}}); got != Discard {
		t.Fatalf("mixed location: %s, want drop", got)
	}
}

// A record routed to Personal (Unknown or OutsideGit) waits while Personal is temporarily
// blocked (Expired, SignInNeeded, Suspended), and discards when Personal is Refused or
// another team/org binding is added so Unknown can no longer go to Personal.
func TestRecheckPersonalUnknownAndOutsideGit(t *testing.T) {
	t.Parallel()
	onlyPersonal := func() (*State, string) {
		st := &State{Profile: "default", Account: testAccount}
		id, err := st.Select(personal())
		must(t, err)
		return st, id
	}
	for _, loc := range []Location{{Kind: Unknown}, {Kind: OutsideGit}} {
		st, ePers := onlyPersonal()
		d := Destination{EnrollmentID: ePers, Location: loc}
		if v, _ := NewRouter(*st).Recheck(d, now); v != SendNow {
			t.Fatalf("%s active personal: got %s, want send", loc.Kind, v)
		}
		for name, block := range map[string]func(*State){
			"expired":        func(s *State) { s.Teams[0].Policy.FetchedAt = now.Add(-config.MaxPolicyAge - time.Hour) },
			"sign_in_needed": func(s *State) { s.Teams[0].SignInNeeded = true },
			"suspended":      func(s *State) { s.Teams[0].Refusals = 1 },
		} {
			s, id := onlyPersonal()
			block(s)
			if v, _ := NewRouter(*s).Recheck(Destination{EnrollmentID: id, Location: loc}, now); v != Wait {
				t.Errorf("%s/%s: got %s, want wait", loc.Kind, name, v)
			}
		}
		sRef, idRef := onlyPersonal()
		sRef.Teams[0].Refusals = 2
		if v, _ := NewRouter(*sRef).Recheck(Destination{EnrollmentID: idRef, Location: loc}, now); v != Discard {
			t.Errorf("%s/refused: got %s, want drop", loc.Kind, v)
		}
	}
	// Adding a second team drops queued Unknown records pinned to Personal (never re-routes),
	// and Narrow Unknown records also drop.
	s2, id2 := onlyPersonal()
	_, err := s2.Select(team("acme", "acme-platform", "github.com/acme/*"))
	must(t, err)
	if v, _ := NewRouter(*s2).Recheck(Destination{EnrollmentID: id2, Location: Location{Kind: Unknown}}, now); v != Discard {
		t.Fatalf("unknown after second team ticked: got %s, want drop", v)
	}
	sNarrow, idNarrow := onlyPersonal()
	sNarrow.Teams[0].SignInNeeded = true
	if v, _ := NewRouter(*sNarrow).Recheck(Destination{EnrollmentID: idNarrow, Location: Location{Kind: Unknown, Narrow: true}}, now); v != Discard {
		t.Fatalf("narrow unknown on blocked personal: got %s, want drop", v)
	}
}
