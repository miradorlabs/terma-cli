package routing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A refresh refused with ORG_NOT_ACCESSIBLE makes the team Refused at once, stamped with
// when, and advances the epoch so a success already in flight cannot undo it.
func TestOrgAccessLostRefusesAtOnce(t *testing.T) {
	t.Parallel()
	s := fixture(t)
	id := enrollment(t, s, "acme-platform")
	inFlight := s.team(id).Epoch
	at := now.Add(time.Minute)
	if changed, err := s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: inFlight, Failure: OrgAccessLost, At: at}); err != nil || !changed {
		t.Fatalf("ApplyRefresh = %v, %v", changed, err)
	}
	tm := s.team(id)
	if tm.StateAt(now) != Refused || tm.Refusals != 2 || tm.LastError != OrgAccessLost || !tm.RefusedAt.Equal(at) {
		t.Fatalf("one ORG_NOT_ACCESSIBLE left %+v", tm)
	}
	if tm.Epoch == inFlight {
		t.Fatal("the epoch did not advance")
	}
	ok := team("acme", "acme-platform", "github.com/acme/*").Policy
	ok.Revision = 2
	if changed, _ := s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: inFlight, Policy: &ok}); changed || s.team(id).StateAt(now) != Refused {
		t.Fatal("a success started before the refusal undid it")
	}

	// Already Refused: another refusal keeps the time it became so.
	must(t, applied(s, Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Failure: Terminal, At: at.Add(time.Hour)}))
	if !s.team(id).RefusedAt.Equal(at) {
		t.Fatalf("RefusedAt moved to %v", s.team(id).RefusedAt)
	}
	// The daily recheck's success restores the team and clears the time.
	must(t, applied(s, Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Policy: &ok}))
	if tm := s.team(id); tm.StateAt(now) != Active || !tm.RefusedAt.IsZero() {
		t.Fatalf("after the recheck: %+v", tm)
	}
}

func applied(s *State, r Refresh) error {
	_, err := s.ApplyRefresh(r)
	return err
}

// Two terminal refusals in a row stamp the time of the second, when the team became
// Refused; a suspension alone stamps nothing.
func TestRefusedAtIsWhenTheTeamBecameRefused(t *testing.T) {
	t.Parallel()
	s := fixture(t)
	id := enrollment(t, s, "clientco-contractors")
	must(t, applied(s, Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Failure: Terminal, At: now}))
	if !s.team(id).RefusedAt.IsZero() {
		t.Fatal("a suspension stamped RefusedAt")
	}
	second := now.Add(time.Hour)
	must(t, applied(s, Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Failure: Terminal, At: second}))
	if tm := s.team(id); tm.StateAt(now) != Refused || !tm.RefusedAt.Equal(second) {
		t.Fatalf("after two refusals: %+v", tm)
	}
	b, err := json.Marshal(s.team(id))
	must(t, err)
	if !strings.Contains(string(b), `"refused_at":"2026-11-12T13:00:00Z"`) {
		t.Fatalf("RefusedAt on the wire: %s", b)
	}
	b, err = json.Marshal(s.team(enrollment(t, s, "acme-platform")))
	must(t, err)
	if strings.Contains(string(b), "refused_at") {
		t.Fatalf("a team never refused carries refused_at: %s", b)
	}
}

// The migration record names a known phase and maps legacy projects only to selected
// enrollments; ending the cutover enrollment ends its mapping too.
func TestMigrationRecord(t *testing.T) {
	t.Parallel()
	s := fixture(t)
	acme := enrollment(t, s, "acme-platform")
	s.Migration = &Migration{Phase: Preparing, Legacy: map[string]string{"acme-platform": acme}}
	must(t, s.check())
	s.Migration.Phase = Committed
	must(t, s.check())

	for name, m := range map[string]*Migration{
		"unknown phase":        {Phase: "done"},
		"no phase":             {Legacy: map[string]string{"acme-platform": acme}},
		"unknown enrollment":   {Phase: Committed, Legacy: map[string]string{"acme-platform": "not-an-enrollment"}},
		"empty legacy project": {Phase: Committed, Legacy: map[string]string{"": acme}},
	} {
		bad := *s
		bad.Migration = m
		if err := bad.check(); err == nil {
			t.Errorf("%s: accepted %+v", name, m)
		}
	}

	if !s.Deselect(acme) {
		t.Fatal("Deselect found no enrollment")
	}
	if _, ok := s.Migration.Legacy["acme-platform"]; ok {
		t.Fatal("the legacy project still maps to the ended enrollment")
	}
	must(t, s.check())
	again, err := s.Select(team("acme", "acme-platform", "github.com/acme/*"))
	must(t, err)
	if again == acme || len(s.Migration.Legacy) != 0 {
		t.Fatal("ticking the team again revived its legacy mapping")
	}
}

// The migration record survives a write and a read, and a state on disk with a broken
// record is refused.
func TestMigrationRecordIsStored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var acme string
	_, err := Create(dir, "default", func(s *State) error {
		var err error
		if acme, err = s.Select(team("acme", "acme-platform", "github.com/acme/*")); err != nil {
			return err
		}
		s.Migration = &Migration{Phase: Preparing, Legacy: map[string]string{"acme-platform": acme}}
		return nil
	})
	must(t, err)
	_, err = Update(dir, "default", func(s *State) error {
		s.Migration.Phase = Committed
		return nil
	})
	must(t, err)
	s, ok, err := Load(dir, "default")
	if err != nil || !ok || s.Migration == nil || s.Migration.Phase != Committed || s.Migration.Legacy["acme-platform"] != acme {
		t.Fatalf("Load = %+v, %v, %v", s.Migration, ok, err)
	}
	if _, err := Update(dir, "default", func(s *State) error {
		s.Migration.Phase = "rolled-back"
		return nil
	}); err == nil {
		t.Fatal("wrote an unknown migration phase")
	}
}

// A state still Preparing sends nothing, whoever reads it: LoadReady reads it as no state,
// and a Router over it drops every record, takes no unknown record for personal, and keeps
// every queued record waiting. Committed, or set up afresh, it routes as usual.
func TestAPreparingStateSendsNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var acme string
	_, err := Create(dir, "default", func(s *State) error {
		var err error
		if acme, err = s.Select(team("acme", "acme-platform", "github.com/acme/*")); err != nil {
			return err
		}
		must(t, s.Bind("github.com/acme", "acme", false))
		s.Migration = &Migration{Phase: Preparing, Legacy: map[string]string{"acme-platform": acme}}
		return nil
	})
	must(t, err)

	raw, ok, err := Load(dir, "default")
	if err != nil || !ok || raw.Ready() {
		t.Fatalf("Load = ready %v, %v, %v", raw.Ready(), ok, err)
	}
	if _, ok, err := LoadReady(dir, "default"); ok || err != nil {
		t.Fatalf("LoadReady read a Preparing state: %v, %v", ok, err)
	}
	r := NewRouter(raw)
	for _, loc := range []Location{repo("github.com/acme/api"), {Kind: OutsideGit}, {Kind: Unknown}} {
		if d := r.Route(loc, now); d.Outcome != Drop || d.Reason != ReasonNotReady || d.EnrollmentID != "" {
			t.Errorf("Route(%+v) on a Preparing state = %+v", loc, d)
		}
	}
	if v, _ := r.Recheck(Destination{EnrollmentID: acme, Location: repo("github.com/acme/api")}, now); v != Wait {
		t.Errorf("Recheck on a Preparing state = %s, want it kept waiting", v)
	}
	solo := fixture(t, personal())
	solo.Migration = &Migration{Phase: Preparing}
	if UnknownToPersonal(*solo, now) {
		t.Error("a Preparing state takes unknown records for personal")
	}

	_, err = Update(dir, "default", func(s *State) error {
		s.Migration.Phase = Committed
		return nil
	})
	must(t, err)
	st, ok, err := LoadReady(dir, "default")
	if err != nil || !ok || !st.Ready() {
		t.Fatalf("LoadReady after the commit = %v, %v", ok, err)
	}
	if d := NewRouter(st).Route(repo("github.com/acme/api"), now); d.Outcome != Send || d.EnrollmentID != acme {
		t.Fatalf("Route after the commit = %+v", d)
	}
	if fresh := fixture(t); !fresh.Ready() {
		t.Fatal("a state set up afresh is not ready")
	}
}
