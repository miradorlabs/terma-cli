package routing

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// The wildcard grammar, and an older policy reading `/*` literally.
func TestParseEntry(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		raw       string
		wildcards bool
		ok        bool
		key       string
		matches   []string
		misses    []string
	}{
		{"github.com/acme/api", true, true, "github.com/acme/api", []string{"github.com/acme/api", "GitHub.com/ACME/api"}, []string{"github.com/acme/api2", "github.com/acme"}},
		{"github.com/acme/*", true, true, "github.com/acme", []string{"github.com/acme/api", "github.com/acme/a/b"}, []string{"github.com/acme", "github.com/acmex/api"}},
		{"gitlab.com/acme/platform/*", true, true, "gitlab.com/acme", []string{"gitlab.com/acme/platform/api"}, []string{"gitlab.com/acme/other"}},
		{"github.com/acme/*", false, true, "github.com/acme/*", nil, []string{"github.com/acme/api"}},
		{"github.com/*", true, false, "", nil, nil},
		{"*", true, false, "", nil, nil},
		{"github.com/acme/terma-*", true, false, "", nil, nil},
		{"github.com/*/api", true, false, "", nil, nil},
		{"github.com/acme", true, false, "", nil, nil},
		{"github.com//api", false, false, "", nil, nil},
	} {
		e, ok := ParseEntry(c.raw, c.wildcards)
		if ok != c.ok {
			t.Errorf("ParseEntry(%q, %v) ok = %v", c.raw, c.wildcards, ok)
			continue
		}
		if !ok {
			continue
		}
		if e.Key() != c.key {
			t.Errorf("%q needs binding %q, want %q", c.raw, e.Key(), c.key)
		}
		for _, r := range c.matches {
			if !e.Matches(r) {
				t.Errorf("%q does not match %q", c.raw, r)
			}
		}
		for _, r := range c.misses {
			if e.Matches(r) {
				t.Errorf("%q matches %q", c.raw, r)
			}
		}
	}
	if Owner("gitlab.com/acme/platform/api") != "gitlab.com/acme" || Owner("github.com/acme") != "" {
		t.Error("an owner is a host and the first path segment, of a repository only")
	}
	for _, key := range []string{"github.com", "github.com/*", "github.com/acme/*", "/acme", "github.com//api"} {
		if _, err := bindingKey(key); err == nil {
			t.Errorf("%q was accepted as a binding key", key)
		}
	}
}

// Refreshes change a team's grant only from the current epoch: every state-changing
// outcome (including success) advances the epoch so neither a delayed success nor a
// delayed refusal/transient can overwrite a newer transition.
func TestApplyRefreshKeepsRefusalsInOrder(t *testing.T) {
	t.Parallel()
	s := fixture(t)
	id := enrollment(t, s, "acme-platform")
	ok := func(rev int64) *config.Policy {
		return &config.Policy{Mode: config.ModeRepo, TeamID: "acme-platform", OrganizationID: "acme", AuthURL: authURL, Repositories: []string{"github.com/acme/*"}, Wildcards: true, Revision: rev, FetchedAt: now}
	}
	apply := func(r Refresh) bool {
		t.Helper()
		changed, err := s.ApplyRefresh(r)
		must(t, err)
		return changed
	}
	state := func() TeamState { return s.Team(id).StateAt(now) }

	// A delayed refusal or transient started before a newer success cannot undo the success.
	beforeSuccess := s.Team(id).Epoch
	if !apply(Refresh{EnrollmentID: id, Epoch: beforeSuccess, Policy: ok(2), At: now}) || s.Team(id).Epoch != beforeSuccess+1 {
		t.Fatalf("success did not advance epoch: %+v", s.Team(id))
	}
	for _, delayed := range []ErrorClass{Terminal, OrgAccessLost, SignIn, Transient} {
		if apply(Refresh{EnrollmentID: id, Epoch: beforeSuccess, Failure: delayed, At: now}) || state() != Active || s.Team(id).LastError != "" {
			t.Fatalf("delayed %s from stale epoch overwrote newer success: %+v", delayed, s.Team(id))
		}
	}

	inFlight := s.Team(id).Epoch
	apply(Refresh{EnrollmentID: id, Epoch: inFlight, Failure: Terminal})
	if state() != Suspended {
		t.Fatalf("one terminal refusal left the team %s", state())
	}
	if apply(Refresh{EnrollmentID: id, Epoch: inFlight, Policy: ok(3)}) || state() != Suspended {
		t.Fatal("a success started before the refusal undid it")
	}
	apply(Refresh{EnrollmentID: id, Epoch: s.Team(id).Epoch, Failure: Transient})
	if state() != Suspended {
		t.Fatal("a network failure cleared a suspension")
	}
	refusedAt := now.Add(time.Minute)
	apply(Refresh{EnrollmentID: id, Epoch: s.Team(id).Epoch, Failure: Terminal, At: refusedAt})
	if state() != Refused || !s.Team(id).RefusedAt.Equal(refusedAt) {
		t.Fatalf("two terminal refusals left the team %s, refusedAt=%v", state(), s.Team(id).RefusedAt)
	}
	apply(Refresh{EnrollmentID: id, Epoch: s.Team(id).Epoch, Policy: ok(3)})
	if state() != Active || !s.Team(id).RefusedAt.IsZero() {
		t.Fatal("the daily recheck's success did not restore the team and clear RefusedAt")
	}
	if _, err := s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: s.Team(id).Epoch, Policy: ok(2)}); err == nil {
		t.Fatal("accepted an older revision")
	}
	beforeSignIn := s.Team(id).Epoch
	apply(Refresh{EnrollmentID: id, Epoch: beforeSignIn, Failure: SignIn})
	if state() != SignInNeeded {
		t.Fatal("a refused sign-in did not block the team")
	}
	if apply(Refresh{EnrollmentID: id, Epoch: beforeSignIn, Policy: ok(4)}) || state() != SignInNeeded {
		t.Fatal("a success started under the refused sign-in cleared it")
	}
	// Single OrgAccessLost transitions immediately to Refused and stamps RefusedAt.
	orgLostAt := now.Add(2 * time.Minute)
	apply(Refresh{EnrollmentID: id, Epoch: s.Team(id).Epoch, Failure: OrgAccessLost, At: orgLostAt})
	if state() != Refused || s.Team(id).LastError != OrgAccessLost || !s.Team(id).RefusedAt.Equal(orgLostAt) {
		t.Fatalf("OrgAccessLost left %+v", s.Team(id))
	}
	for _, bad := range []Refresh{
		{EnrollmentID: id, Epoch: s.Team(id).Epoch, Policy: &config.Policy{TeamID: "clientco-contractors", Revision: 9, FetchedAt: now}},
		{EnrollmentID: id, Epoch: s.Team(id).Epoch},
	} {
		if _, err := s.ApplyRefresh(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}

	s.Deselect(id)
	again, err := s.Select(team("acme", "acme-platform", "github.com/acme/*"))
	must(t, err)
	if apply(Refresh{EnrollmentID: id, Epoch: 0, Failure: Terminal}) || s.Team(again).StateAt(now) != Active {
		t.Fatal("an ended enrollment's refresh reached the team ticked again")
	}
}

// Org-wide ORG_NOT_ACCESSIBLE refuses all matching-epoch enrollments in that organization,
// spares any sibling whose epoch already advanced via a newer success, and leaves other
// organizations and Personal untouched while keeping bindings intact.
func TestApplyOrgAccessLostRefusesMatchingEpochSiblingsOnly(t *testing.T) {
	t.Parallel()
	s := fixture(t,
		team("acme", "acme-platform", "github.com/acme/*"),
		team("acme", "acme-mobile", "github.com/acme/mobile"),
		team("acme", "acme-fresh", "github.com/acme/fresh"),
		team("clientco", "clientco-contractors", "github.com/clientco/app"),
		personal(),
	)
	ePlat := enrollment(t, s, "acme-platform")
	eMob := enrollment(t, s, "acme-mobile")
	eFresh := enrollment(t, s, "acme-fresh")
	eClient := enrollment(t, s, "clientco-contractors")
	ePers := enrollment(t, s, "personal")

	// An enrollment in the same organization ID on a different auth environment must be untouched.
	otherEnv := team("acme", "acme-staging", "github.com/acme/staging")
	otherEnv.AuthURL = "https://auth.staging.example"
	otherEnv.Policy.AuthURL = otherEnv.AuthURL
	eOtherEnv, err := s.Select(otherEnv)
	must(t, err)

	// Pre-request epoch snapshot across acme (including the other-env enrollment to verify authURL filtering).
	snapshot := map[string]int64{
		ePlat:     s.Team(ePlat).Epoch,
		eMob:      s.Team(eMob).Epoch,
		eFresh:    s.Team(eFresh).Epoch,
		eOtherEnv: s.Team(eOtherEnv).Epoch,
	}
	// acme-fresh completes a newer successful refresh before the 403 arrives.
	freshPol := config.Policy{
		Mode: config.ModeRepo, TeamID: "acme-fresh", OrganizationID: "acme", AuthURL: authURL,
		Repositories: []string{"github.com/acme/fresh"}, Wildcards: true, Revision: 2, FetchedAt: now,
	}
	if changed, err := s.ApplyRefresh(Refresh{EnrollmentID: eFresh, Epoch: snapshot[eFresh], Policy: &freshPol}); err != nil || !changed {
		t.Fatalf("fresh refresh: changed=%v err=%v", changed, err)
	}

	at := now.Add(5 * time.Minute)
	// Empty org, empty authURL, or empty snapshot are no-ops.
	if s.ApplyOrgAccessLost("", authURL, snapshot, at) || s.ApplyOrgAccessLost("acme", "", snapshot, at) || s.ApplyOrgAccessLost("acme", authURL, nil, at) {
		t.Fatal("ApplyOrgAccessLost accepted empty org, authURL, or snapshot")
	}
	if !s.ApplyOrgAccessLost("acme", authURL, snapshot, at) {
		t.Fatal("ApplyOrgAccessLost reported no change")
	}
	for _, refusedID := range []string{ePlat, eMob} {
		tm := s.Team(refusedID)
		if tm.StateAt(now) != Refused || tm.LastError != OrgAccessLost || !tm.RefusedAt.Equal(at) || tm.Epoch != snapshot[refusedID]+1 {
			t.Errorf("enrollment %s = %+v, want Refused/OrgAccessLost at %v", refusedID, tm, at)
		}
	}
	// Sibling with newer epoch, other auth environment, other org, and Personal are untouched.
	for _, activeID := range []string{eFresh, eOtherEnv, eClient, ePers} {
		if st := s.Team(activeID).StateAt(now); st != Active {
			t.Errorf("enrollment %s became %s, want Active", activeID, st)
		}
	}
	// Re-applying the stale snapshot is a no-op and preserves RefusedAt.
	if s.ApplyOrgAccessLost("acme", authURL, snapshot, at.Add(time.Hour)) {
		t.Fatal("stale snapshot modified already-advanced enrollments")
	}
	if !s.Team(ePlat).RefusedAt.Equal(at) {
		t.Fatal("RefusedAt changed on stale re-apply")
	}
	// Bindings remain intact: acme repos hold as Refused, never spilling into Personal.
	d := NewRouter(*s).Route(repo("github.com/acme/api"), now)
	if d.Outcome != Hold || d.Blocked != Refused || d.EnrollmentID != ePlat {
		t.Fatalf("Route after org access lost = %+v, want Hold/Refused on %s", d, ePlat)
	}
	expect(t, s, repo("github.com/clientco/app"), Send, "clientco-contractors")
	expect(t, s, repo("github.com/dawson/side"), Send, "personal")
}

// Personal stays last, and a team joins only with its own validated policy.
func TestSelect(t *testing.T) {
	t.Parallel()
	s := &State{Profile: "default", Account: testAccount}
	for _, tm := range []Team{personal(), team("acme", "acme-platform", "github.com/acme/*"), team("clientco", "clientco-contractors")} {
		if _, err := s.Select(tm); err != nil {
			t.Fatal(err)
		}
	}
	if !s.Teams[len(s.Teams)-1].Personal || s.Teams[0].TeamID != "acme-platform" || s.Teams[1].TeamID != "clientco-contractors" {
		t.Fatalf("teams out of order: %+v", s.Teams)
	}
	unfetched := team("acme", "acme-mobile")
	unfetched.Policy.FetchedAt = time.Time{}
	global := team("acme", "acme-all")
	global.Policy.Mode = config.ModeGlobal
	for _, tm := range []Team{unfetched, global, personal(), team("acme", "acme-platform")} {
		if _, err := s.Select(tm); err == nil {
			t.Errorf("selected %s", tm.TeamID)
		}
	}
	if err := s.check(); err != nil {
		t.Fatal(err)
	}
}

// The state is per profile, written under one lock: concurrent writers lose nothing, a
// broken change writes nothing, and another profile's file is never read.
func TestUpdate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exp := Expect{Account: testAccount}
	if _, err := Update(dir, "default", exp, func(*State) error { return nil }); !errors.Is(err, ErrNoState) {
		t.Fatalf("Update made a routing state that did not exist: %v", err)
	}
	if _, err := Create(dir, "default", testAccount, func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(dir, "default", testAccount, func(*State) error { return nil }); !errors.Is(err, ErrStateExists) {
		t.Fatalf("Create overwrote an existing routing state: %v", err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Update(dir, "default", exp, func(s *State) error {
				return s.Bind(fmt.Sprintf("github.com/owner%d", i), "acme", false)
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	s, ok, err := Load(dir, "default")
	if err != nil || !ok || s.Account != testAccount || len(s.Bindings) != 20 || s.Generation != 21 {
		t.Fatalf("after 20 concurrent writes: account=%q, %d bindings, generation %d, %v", s.Account, len(s.Bindings), s.Generation, err)
	}
	for _, noop := range []func(*State) error{
		func(s *State) error { return s.Bind("github.com/owner0", "acme", false) },
		func(s *State) error { return s.Unbind("github.com/never-bound") },
	} {
		if again, err := Update(dir, "default", exp, noop); err != nil || again.Generation != s.Generation {
			t.Fatalf("a change that changed nothing was written: generation %d, %v", again.Generation, err)
		}
	}

	if _, err := Update(dir, "default", exp, func(s *State) error {
		s.Teams = append(s.Teams, personal(), team("acme", "acme-platform"))
		s.Teams[0].EnrollmentID, s.Teams[1].EnrollmentID = "a", "b"
		return nil
	}); err == nil {
		t.Fatal("wrote a state with personal before an organization")
	}
	if again, _, _ := Load(dir, "default"); again.Generation != s.Generation {
		t.Fatal("a refused change was written")
	}

	path, _ := StatePath(dir, "default")
	other, _ := StatePath(dir, "work")
	b, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(other, b, 0o600))
	if _, _, err := Load(dir, "work"); err == nil {
		t.Fatal("read another profile's routing state")
	}
	must(t, os.WriteFile(path, []byte("{"), 0o600))
	if _, _, err := Load(dir, "default"); err == nil {
		t.Fatal("an unreadable routing state read as none")
	}
	if _, err := StatePath(dir, "../escape"); err == nil || filepath.Base(other) != "work.json" {
		t.Fatal("a profile name stepped out of the routing folder")
	}
}

// CAS writes guard against stale interactive sessions, Delete+Create ABA races, and
// cross-account writers (Alice vs Bob).
func TestStoreCASRejectsWrongAccountAndStaleGeneration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Invalid accounts are refused by Create, Update, Replace, and Delete.
	for _, bad := range []string{"", "unreadable", "short", "0123456789ABCDEF", "zzzzzzzzzzzzzzzz"} {
		if _, err := Create(dir, "default", bad, func(*State) error { return nil }); err == nil {
			t.Errorf("Create accepted invalid account %q", bad)
		}
		if _, err := Update(dir, "default", Expect{Account: bad}, func(*State) error { return nil }); err == nil {
			t.Errorf("Update accepted invalid account %q", bad)
		}
		if _, err := Replace(dir, "default", nil, bad, func(*State) error { return nil }); err == nil {
			t.Errorf("Replace accepted invalid newAccount %q", bad)
		}
		if _, err := Replace(dir, "default", &Expect{Account: bad}, testAccount, func(*State) error { return nil }); err == nil {
			t.Errorf("Replace accepted invalid expectPrev account %q", bad)
		}
		if err := Delete(dir, "default", Expect{Account: bad}); err == nil {
			t.Errorf("Delete accepted invalid account %q", bad)
		}
	}

	// Server-key profile lane ("no_person") is a valid account owner.
	if _, err := Create(dir, "srv", "no_person", func(*State) error { return nil }); err != nil {
		t.Fatalf("Create with no_person: %v", err)
	}

	// Callback tampering with Profile, Account, StateID, or Generation is refused in Create.
	for name, tamper := range map[string]func(*State) error{
		"profile":    func(s *State) error { s.Profile = "other"; return nil },
		"account":    func(s *State) error { s.Account = bobAccount; return nil },
		"state_id":   func(s *State) error { s.StateID = "tampered"; return nil },
		"generation": func(s *State) error { s.Generation = 99; return nil },
	} {
		if _, err := Create(dir, "tamper-create", testAccount, tamper); err == nil {
			t.Errorf("Create allowed callback to mutate %s", name)
		}
	}

	// Alice creates her routing state at generation 1.
	alice1, err := Create(dir, "default", testAccount, func(s *State) error {
		return s.Bind("github.com/acme", "acme", false)
	})
	must(t, err)
	if alice1.Generation != 1 || alice1.Account != testAccount || alice1.StateID == "" {
		t.Fatalf("alice1 = %+v", alice1)
	}

	// Bob cannot update or delete Alice's state.
	if _, err := Update(dir, "default", Expect{Account: bobAccount}, func(s *State) error {
		return s.Bind("github.com/bob", "bob", false)
	}); !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("Bob Update on Alice state: got %v, want ErrAccountMismatch", err)
	}
	if err := Delete(dir, "default", Expect{Account: bobAccount}); !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("Bob Delete on Alice state: got %v, want ErrAccountMismatch", err)
	}

	// Concurrent writer advances Alice's state to generation 2 using State.Expect().
	alice2, err := Update(dir, "default", alice1.Expect(), func(s *State) error {
		return s.Bind("github.com/clientco", "clientco", false)
	})
	must(t, err)
	if alice2.Generation != 2 || alice2.StateID != alice1.StateID {
		t.Fatalf("alice2 = %+v, want gen=2 same StateID", alice2)
	}

	// A stale interactive session holding alice1.Expect() is rejected and writes nothing.
	if _, err := Update(dir, "default", alice1.Expect(), func(s *State) error {
		return s.Bind("github.com/stale", "acme", false)
	}); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale generation Update: got %v, want ErrStaleGeneration", err)
	}
	if err := Delete(dir, "default", alice1.Expect()); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale generation Delete: got %v, want ErrStaleGeneration", err)
	}

	// Callback tampering with Profile, Account, StateID, or Generation is refused in Update and Replace.
	for name, tamper := range map[string]func(*State) error{
		"profile":    func(s *State) error { s.Profile = "other"; return nil },
		"account":    func(s *State) error { s.Account = bobAccount; return nil },
		"state_id":   func(s *State) error { s.StateID = "tampered"; return nil },
		"generation": func(s *State) error { s.Generation = 99; return nil },
	} {
		if _, err := Update(dir, "default", Expect{Account: testAccount}, tamper); err == nil {
			t.Errorf("Update allowed callback to mutate %s", name)
		}
		if _, err := Replace(dir, "default", &Expect{Account: testAccount}, testAccount, tamper); err == nil {
			t.Errorf("Replace allowed callback to mutate %s", name)
		}
	}

	// Conditional Replace rejects wrong previous account or stale generation.
	if _, err := Replace(dir, "default", &Expect{Account: bobAccount}, bobAccount, func(*State) error { return nil }); !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("Replace with wrong expectPrev account: %v", err)
	}
	if _, err := Replace(dir, "default", func() *Expect { e := alice1.Expect(); return &e }(), bobAccount, func(*State) error { return nil }); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("Replace with stale expectPrev generation: %v", err)
	}

	// Bob signs in via setup and atomically replaces Alice's state; generation advances,
	// a new StateID is minted, and Alice's teams/bindings do not carry over.
	alice2Exp := alice2.Expect()
	bob1, err := Replace(dir, "default", &alice2Exp, bobAccount, func(s *State) error {
		return s.Bind("github.com/bobco", "bobco", false)
	})
	must(t, err)
	if bob1.Account != bobAccount || bob1.Generation != 3 || bob1.StateID == alice2.StateID || len(bob1.Bindings) != 1 || bob1.Bindings["github.com/bobco"].Organization != "bobco" {
		t.Fatalf("bob1 = %+v", bob1)
	}
	if SameGeneration(State{StateID: bob1.StateID, Generation: 3, Account: testAccount}, bob1) {
		t.Fatal("SameGeneration matched states of different accounts")
	}

	// Alice's in-flight refresher or open `terma teams` session is now rejected.
	if _, err := Update(dir, "default", Expect{Account: testAccount}, func(s *State) error {
		return s.Bind("github.com/alice-late", "acme", false)
	}); !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("Alice late Update after Bob Replace: got %v, want ErrAccountMismatch", err)
	}

	// Unconditional Replace (expectPrev == nil) recovers even from a corrupt file on disk,
	// minting a fresh StateID so stale Expect snapshots never ABA-match.
	path, _ := StatePath(dir, "default")
	must(t, os.WriteFile(path, []byte("{corrupt"), 0o600))
	if _, _, err := Load(dir, "default"); err == nil {
		t.Fatal("Load accepted corrupt file")
	}
	if _, err := Replace(dir, "default", &Expect{Account: bobAccount}, bobAccount, func(*State) error { return nil }); err == nil {
		t.Fatal("conditional Replace accepted corrupt file")
	}
	recovered, err := Replace(dir, "default", nil, bobAccount, func(*State) error { return nil })
	must(t, err)
	if recovered.Account != bobAccount || recovered.Generation != 1 || recovered.StateID == bob1.StateID {
		t.Fatalf("recovered = %+v", recovered)
	}

	// ABA guard: Delete followed by Create for the same account starts at generation 1 with
	// a new StateID, rejecting any writer holding recovered.Expect() from before Delete.
	must(t, Delete(dir, "default", recovered.Expect()))
	if _, ok, err := Load(dir, "default"); err != nil || ok {
		t.Fatalf("after Delete: ok=%v err=%v", ok, err)
	}
	if err := Delete(dir, "default", Expect{Account: bobAccount}); !errors.Is(err, ErrNoState) {
		t.Fatalf("Delete absent state: %v", err)
	}
	recreated, err := Create(dir, "default", bobAccount, func(*State) error { return nil })
	must(t, err)
	if recreated.Generation != 1 || recreated.StateID == recovered.StateID {
		t.Fatalf("recreated = %+v", recreated)
	}
	if SameGeneration(recovered, recreated) {
		t.Fatal("SameGeneration matched states across Delete+Create")
	}
	if _, err := Update(dir, "default", recovered.Expect(), func(s *State) error {
		return s.Bind("github.com/aba", "bobco", false)
	}); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale Expect after Delete+Create: got %v, want ErrStaleGeneration", err)
	}

	// Hand-built Expect with Generation set but empty StateID is refused to prevent ABA bugs.
	bareGen := int64(1)
	if _, err := Update(dir, "default", Expect{Account: bobAccount, Generation: &bareGen}, func(*State) error { return nil }); err == nil {
		t.Fatal("Update accepted Generation without StateID")
	}

	// Unconditional Replace (expectPrev == nil) over a readable state advances Generation
	// past the existing state's generation and mints a fresh StateID.
	recreated2, err := Replace(dir, "default", nil, testAccount, func(*State) error { return nil })
	must(t, err)
	if recreated2.Generation != recreated.Generation+1 || recreated2.StateID == recreated.StateID || recreated2.Account != testAccount {
		t.Fatalf("unconditional Replace over readable state = %+v", recreated2)
	}
}

// StoreTeamPolicy requires team, organization, and auth environment, and refuses a
// backward revision without altering the stored policy.
func TestStoreTeamPolicyValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := config.Policy{Mode: config.ModeRepo, TeamID: "team-a", OrganizationID: "acme", AuthURL: authURL, Revision: 3, FetchedAt: now}
	for _, bad := range []config.Policy{
		{Mode: config.ModeRepo, OrganizationID: "acme", AuthURL: authURL, FetchedAt: now},
		{Mode: config.ModeRepo, TeamID: "team-a", AuthURL: authURL, FetchedAt: now},
		{Mode: config.ModeRepo, TeamID: "team-a", OrganizationID: "acme", FetchedAt: now},
	} {
		if err := StoreTeamPolicy(dir, &bad); err == nil {
			t.Errorf("StoreTeamPolicy accepted missing scope %+v", bad)
		}
	}
	must(t, StoreTeamPolicy(dir, &base))
	older := base
	older.Revision = 2
	if err := StoreTeamPolicy(dir, &older); err == nil {
		t.Fatal("StoreTeamPolicy accepted backward revision")
	}
	got, ok, err := config.ReadPolicy(dir, "team-a")
	if err != nil || !ok || got.Revision != 3 {
		t.Fatalf("after rejected backward write: %+v, ok=%v, err=%v", got, ok, err)
	}
}

// Re-refusing an already-Refused team across Terminal and OrgAccessLost keeps the
// original RefusedAt timestamp while advancing Epoch and updating LastError.
func TestRefusedAtPreservedAcrossRefusalReasons(t *testing.T) {
	t.Parallel()
	s := fixture(t, team("acme", "acme-platform", "github.com/acme/*"))
	id := enrollment(t, s, "acme-platform")
	t1 := now.Add(time.Minute)
	t2 := now.Add(2 * time.Hour)
	t3 := now.Add(3 * time.Hour)

	if _, err := s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: 0, Failure: Terminal, At: t1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: 1, Failure: Terminal, At: t1}); err != nil {
		t.Fatal(err)
	}
	if !s.Team(id).RefusedAt.Equal(t1) {
		t.Fatalf("initial RefusedAt = %v, want %v", s.Team(id).RefusedAt, t1)
	}
	if changed, err := s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: 2, Failure: OrgAccessLost, At: t2}); err != nil || !changed {
		t.Fatalf("OrgAccessLost on Refused: changed=%v err=%v", changed, err)
	}
	if tm := s.Team(id); !tm.RefusedAt.Equal(t1) || tm.Epoch != 3 || tm.LastError != OrgAccessLost {
		t.Fatalf("after OrgAccessLost on Refused: %+v", tm)
	}
	if changed, err := s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: 3, Failure: Terminal, At: t3}); err != nil || !changed {
		t.Fatalf("Terminal on OrgAccessLost: changed=%v err=%v", changed, err)
	}
	if tm := s.Team(id); !tm.RefusedAt.Equal(t1) || tm.Epoch != 4 || tm.LastError != Terminal {
		t.Fatalf("after Terminal on OrgAccessLost: %+v", tm)
	}
}
