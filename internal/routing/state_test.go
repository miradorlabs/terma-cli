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
			t.Errorf("%q keys as %q, want %q", c.raw, e.Key(), c.key)
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
	state := func() TeamState { return s.team(id).StateAt(now) }

	// A delayed refusal or transient started before a newer success cannot undo the success.
	beforeSuccess := s.team(id).Epoch
	if !apply(Refresh{EnrollmentID: id, Epoch: beforeSuccess, Policy: ok(2), At: now}) || s.team(id).Epoch != beforeSuccess+1 {
		t.Fatalf("success did not advance epoch: %+v", s.team(id))
	}
	for _, delayed := range []ErrorClass{Terminal, OrgAccessLost, SignIn, Transient} {
		if apply(Refresh{EnrollmentID: id, Epoch: beforeSuccess, Failure: delayed, At: now}) || state() != Active || s.team(id).LastError != "" {
			t.Fatalf("delayed %s from stale epoch overwrote newer success: %+v", delayed, s.team(id))
		}
	}

	inFlight := s.team(id).Epoch
	apply(Refresh{EnrollmentID: id, Epoch: inFlight, Failure: Terminal})
	if state() != Suspended {
		t.Fatalf("one terminal refusal left the team %s", state())
	}
	if apply(Refresh{EnrollmentID: id, Epoch: inFlight, Policy: ok(3)}) || state() != Suspended {
		t.Fatal("a success started before the refusal undid it")
	}
	apply(Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Failure: Transient})
	if state() != Suspended {
		t.Fatal("a network failure cleared a suspension")
	}
	refusedAt := now.Add(time.Minute)
	apply(Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Failure: Terminal, At: refusedAt})
	if state() != Refused || !s.team(id).RefusedAt.Equal(refusedAt) {
		t.Fatalf("two terminal refusals left the team %s, refusedAt=%v", state(), s.team(id).RefusedAt)
	}
	apply(Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Policy: ok(3)})
	if state() != Active || !s.team(id).RefusedAt.IsZero() {
		t.Fatal("the daily recheck's success did not restore the team and clear RefusedAt")
	}
	if _, err := s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Policy: ok(2)}); err == nil {
		t.Fatal("accepted an older revision")
	}
	beforeSignIn := s.team(id).Epoch
	apply(Refresh{EnrollmentID: id, Epoch: beforeSignIn, Failure: SignIn})
	if state() != SignInNeeded {
		t.Fatal("a refused sign-in did not block the team")
	}
	if apply(Refresh{EnrollmentID: id, Epoch: beforeSignIn, Policy: ok(4)}) || state() != SignInNeeded {
		t.Fatal("a success started under the refused sign-in cleared it")
	}
	// Single OrgAccessLost transitions immediately to Refused and stamps RefusedAt.
	orgLostAt := now.Add(2 * time.Minute)
	apply(Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Failure: OrgAccessLost, At: orgLostAt})
	if state() != Refused || s.team(id).LastError != OrgAccessLost || !s.team(id).RefusedAt.Equal(orgLostAt) {
		t.Fatalf("OrgAccessLost left %+v", s.team(id))
	}
	for _, bad := range []Refresh{
		{EnrollmentID: id, Epoch: s.team(id).Epoch, Policy: &config.Policy{TeamID: "clientco-contractors", Revision: 9, FetchedAt: now}},
		{EnrollmentID: id, Epoch: s.team(id).Epoch},
	} {
		if _, err := s.ApplyRefresh(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}

	s.Deselect(id)
	again, err := s.Select(team("acme", "acme-platform", "github.com/acme/*"))
	must(t, err)
	if apply(Refresh{EnrollmentID: id, Epoch: 0, Failure: Terminal}) || s.team(again).StateAt(now) != Active {
		t.Fatal("an ended enrollment's refresh reached the team ticked again")
	}
}

// Org-wide ORG_NOT_ACCESSIBLE refuses all matching-epoch enrollments in that organization,
// spares any sibling whose epoch already advanced via a newer success, and leaves other
// organizations and Personal untouched.
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
		ePlat:     s.team(ePlat).Epoch,
		eMob:      s.team(eMob).Epoch,
		eFresh:    s.team(eFresh).Epoch,
		eOtherEnv: s.team(eOtherEnv).Epoch,
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
		tm := s.team(refusedID)
		if tm.StateAt(now) != Refused || tm.LastError != OrgAccessLost || !tm.RefusedAt.Equal(at) || tm.Epoch != snapshot[refusedID]+1 {
			t.Errorf("enrollment %s = %+v, want Refused/OrgAccessLost at %v", refusedID, tm, at)
		}
	}
	// Sibling with newer epoch, other auth environment, other org, and Personal are untouched.
	for _, activeID := range []string{eFresh, eOtherEnv, eClient, ePers} {
		if st := s.team(activeID).StateAt(now); st != Active {
			t.Errorf("enrollment %s became %s, want Active", activeID, st)
		}
	}
	// Re-applying the stale snapshot is a no-op and preserves RefusedAt.
	if s.ApplyOrgAccessLost("acme", authURL, snapshot, at.Add(time.Hour)) {
		t.Fatal("stale snapshot modified already-advanced enrollments")
	}
	if !s.team(ePlat).RefusedAt.Equal(at) {
		t.Fatal("RefusedAt changed on stale re-apply")
	}
	// A refused team is passed over: its repositories go down the stack.
	expect(t, s, repo("github.com/acme/api"), Send, "personal")
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
		wg.Go(func() {
			_, err := Update(dir, "default", exp, func(s *State) error {
				return tick(s, "acme", fmt.Sprintf("team%d", i))
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	s, ok, err := Load(dir, "default")
	if err != nil || !ok || s.Account != testAccount || len(s.Teams) != 20 || s.Generation != 21 {
		t.Fatalf("after 20 concurrent writes: account=%q, %d teams, generation %d, %v", s.Account, len(s.Teams), s.Generation, err)
	}
	for _, noop := range []func(*State) error{
		func(s *State) error { s.Deselect("never-selected"); return nil },
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
		return tick(s, "acme", "acme")
	})
	must(t, err)
	if alice1.Generation != 1 || alice1.Account != testAccount || alice1.StateID == "" {
		t.Fatalf("alice1 = %+v", alice1)
	}

	// Bob cannot update or delete Alice's state.
	if _, err := Update(dir, "default", Expect{Account: bobAccount}, func(s *State) error {
		return tick(s, "bob", "bob")
	}); !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("Bob Update on Alice state: got %v, want ErrAccountMismatch", err)
	}
	if err := Delete(dir, "default", Expect{Account: bobAccount}); !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("Bob Delete on Alice state: got %v, want ErrAccountMismatch", err)
	}

	// Concurrent writer advances Alice's state to generation 2 using State.Expect().
	alice2, err := Update(dir, "default", alice1.Expect(), func(s *State) error {
		return tick(s, "clientco", "clientco")
	})
	must(t, err)
	if alice2.Generation != 2 || alice2.StateID != alice1.StateID {
		t.Fatalf("alice2 = %+v, want gen=2 same StateID", alice2)
	}

	// A stale interactive session holding alice1.Expect() is rejected and writes nothing.
	if _, err := Update(dir, "default", alice1.Expect(), func(s *State) error {
		return tick(s, "acme", "stale")
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
	// a new StateID is minted, and Alice's teams do not carry over.
	alice2Exp := alice2.Expect()
	bob1, err := Replace(dir, "default", &alice2Exp, bobAccount, func(s *State) error {
		return tick(s, "bobco", "bobco")
	})
	must(t, err)
	if bob1.Account != bobAccount || bob1.Generation != 3 || bob1.StateID == alice2.StateID || len(bob1.Teams) != 1 || bob1.Teams[0].OrganizationID != "bobco" {
		t.Fatalf("bob1 = %+v", bob1)
	}
	if SameGeneration(State{StateID: bob1.StateID, Generation: 3, Account: testAccount}, bob1) {
		t.Fatal("SameGeneration matched states of different accounts")
	}

	// Alice's in-flight refresher or open `terma teams` session is now rejected.
	if _, err := Update(dir, "default", Expect{Account: testAccount}, func(s *State) error {
		return tick(s, "acme", "alice-late")
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
		return tick(s, "bobco", "aba")
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

// StoreTeamPolicy requires team, organization, and auth environment matching the Team,
// downgrades a non-personal team's global policy to repository mode on disk, preserves
// global mode for a personal team, and refuses a backward revision without altering the
// stored policy.
func TestStoreTeamPolicyValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	basePol := config.Policy{Mode: config.ModeRepo, TeamID: "team-a", OrganizationID: "acme", AuthURL: authURL, Revision: 3, FetchedAt: now}
	baseTeam := Team{TeamID: "team-a", OrganizationID: "acme", AuthURL: authURL, Policy: basePol}
	for _, bad := range []Team{
		{OrganizationID: "acme", AuthURL: authURL, Policy: config.Policy{Mode: config.ModeRepo, OrganizationID: "acme", AuthURL: authURL, FetchedAt: now}},
		{TeamID: "team-a", AuthURL: authURL, Policy: config.Policy{Mode: config.ModeRepo, TeamID: "team-a", AuthURL: authURL, FetchedAt: now}},
		{TeamID: "team-a", OrganizationID: "acme", Policy: config.Policy{Mode: config.ModeRepo, TeamID: "team-a", OrganizationID: "acme", FetchedAt: now}},
		{TeamID: "team-a", OrganizationID: "acme", AuthURL: authURL, Policy: config.Policy{Mode: config.ModeRepo, TeamID: "other-team", OrganizationID: "acme", AuthURL: authURL, FetchedAt: now}},
		{TeamID: "team-a", OrganizationID: "acme", AuthURL: authURL, Policy: config.Policy{Mode: config.ModeRepo, TeamID: "team-a", OrganizationID: "other-org", AuthURL: authURL, FetchedAt: now}},
		{TeamID: "team-a", OrganizationID: "acme", AuthURL: authURL, Policy: config.Policy{Mode: config.ModeRepo, TeamID: "team-a", OrganizationID: "acme", AuthURL: "https://other.example", FetchedAt: now}},
	} {
		if err := StoreTeamPolicy(dir, bad); err == nil {
			t.Errorf("StoreTeamPolicy accepted invalid/mismatched team %+v", bad)
		}
	}
	must(t, StoreTeamPolicy(dir, baseTeam))
	older := baseTeam
	older.Policy.Revision = 2
	if err := StoreTeamPolicy(dir, older); err == nil {
		t.Fatal("StoreTeamPolicy accepted backward revision")
	}
	got, ok, err := config.ReadPolicy(dir, "team-a")
	if err != nil || !ok || got.Revision != 3 {
		t.Fatalf("after rejected backward write: %+v, ok=%v, err=%v", got, ok, err)
	}

	// ApplyRefresh and StoreTeamPolicy persist the exact same downgraded policy when a
	// non-personal team returns ModeGlobal.
	globalPol := config.Policy{
		Mode: config.ModeGlobal, TeamID: "team-a", OrganizationID: "acme", AuthURL: authURL,
		Revision: 4, Repositories: []string{"github.com/acme/api"}, FetchedAt: now,
	}
	st, err := Create(dir, "default", testAccount, func(s *State) error {
		_, err := s.Select(baseTeam)
		return err
	})
	must(t, err)
	enrID := st.Teams[0].EnrollmentID
	committed, err := Update(dir, "default", st.Expect(), func(s *State) error {
		_, err := s.ApplyRefresh(Refresh{EnrollmentID: enrID, Epoch: 0, Policy: &globalPol, At: now})
		return err
	})
	must(t, err)
	must(t, StoreTeamPolicy(dir, *committed.team(enrID)))
	got, ok, err = config.ReadPolicy(dir, "team-a")
	if err != nil || !ok || got.Mode != config.ModeRepo || len(got.Repositories) != 0 || got.Revision != 4 || committed.team(enrID).Policy.Mode != got.Mode {
		t.Fatalf("non-personal global stored unsanitized: file=%+v state=%+v, ok=%v, err=%v", got, committed.team(enrID).Policy, ok, err)
	}

	// Direct StoreTeamPolicy on an unsanitized non-personal global Team also downgrades.
	unsanitized := baseTeam
	unsanitized.Policy = globalPol
	unsanitized.Policy.Revision = 5
	must(t, StoreTeamPolicy(dir, unsanitized))
	got, ok, err = config.ReadPolicy(dir, "team-a")
	if err != nil || !ok || got.Mode != config.ModeRepo || len(got.Repositories) != 0 || got.Revision != 5 {
		t.Fatalf("direct non-personal global stored unsanitized: %+v, ok=%v, err=%v", got, ok, err)
	}

	// A personal team keeps ModeGlobal when persisted.
	personalGlobal := Team{
		TeamID: "personal-team", OrganizationID: "personal-org", AuthURL: authURL, Personal: true,
		Policy: config.Policy{Mode: config.ModeGlobal, TeamID: "personal-team", OrganizationID: "personal-org", AuthURL: authURL, Revision: 1, FetchedAt: now},
	}
	must(t, StoreTeamPolicy(dir, personalGlobal))
	got, ok, err = config.ReadPolicy(dir, "personal-team")
	if err != nil || !ok || got.Mode != config.ModeGlobal {
		t.Fatalf("personal global stored wrong mode: %+v, ok=%v, err=%v", got, ok, err)
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
	if !s.team(id).RefusedAt.Equal(t1) {
		t.Fatalf("initial RefusedAt = %v, want %v", s.team(id).RefusedAt, t1)
	}
	if changed, err := s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: 2, Failure: OrgAccessLost, At: t2}); err != nil || !changed {
		t.Fatalf("OrgAccessLost on Refused: changed=%v err=%v", changed, err)
	}
	if tm := s.team(id); !tm.RefusedAt.Equal(t1) || tm.Epoch != 3 || tm.LastError != OrgAccessLost {
		t.Fatalf("after OrgAccessLost on Refused: %+v", tm)
	}
	if changed, err := s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: 3, Failure: Terminal, At: t3}); err != nil || !changed {
		t.Fatalf("Terminal on OrgAccessLost: changed=%v err=%v", changed, err)
	}
	if tm := s.team(id); !tm.RefusedAt.Equal(t1) || tm.Epoch != 4 || tm.LastError != Terminal {
		t.Fatalf("after Terminal on OrgAccessLost: %+v", tm)
	}
}

// Background policy refreshes advance Generation without advancing ConsentGeneration, so
// interactive editors using ExpectConsent() succeed across refreshes while concurrent
// team edits or account switches are rejected with ErrStaleGeneration /
// ErrAccountMismatch.
func TestConsentGenerationAndExpectConsent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pol := func(rev int64) config.Policy {
		return config.Policy{Mode: config.ModeRepo, TeamID: "acme-platform", OrganizationID: "acme", AuthURL: authURL, Revision: rev, FetchedAt: now}
	}
	var enrID string
	st, err := Create(dir, "default", testAccount, func(s *State) error {
		var selErr error
		enrID, selErr = s.Select(Team{OrganizationID: "acme", AuthURL: authURL, TeamID: "acme-platform", Policy: pol(1)})
		return selErr
	})
	must(t, err)
	if st.Generation != 1 || st.ConsentGeneration != 1 {
		t.Fatalf("after Create: gen=%d consent=%d", st.Generation, st.ConsentGeneration)
	}

	// Background refresh advances Generation (1->2) but NOT ConsentGeneration (stays 1).
	p2 := pol(2)
	refreshed, err := Update(dir, "default", Expect{Account: testAccount}, func(s *State) error {
		_, err := s.ApplyRefresh(Refresh{EnrollmentID: enrID, Epoch: 0, Policy: &p2, At: now})
		return err
	})
	must(t, err)
	if refreshed.Generation != 2 || refreshed.ConsentGeneration != 1 {
		t.Fatalf("after refresh: gen=%d consent=%d, want 2, 1", refreshed.Generation, refreshed.ConsentGeneration)
	}

	// Exact Expect() from st fails, but ExpectConsent() from st succeeds across the refresh!
	if _, err := Update(dir, "default", st.Expect(), func(s *State) error {
		return tick(s, "acme", "acme-api")
	}); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale exact Expect: got %v, want ErrStaleGeneration", err)
	}
	bound, err := Update(dir, "default", st.ExpectConsent(), func(s *State) error {
		return tick(s, "acme", "acme-api")
	})
	must(t, err)
	if bound.Generation != 3 || bound.ConsentGeneration != 2 {
		t.Fatalf("after Select: gen=%d consent=%d, want 3, 2", bound.Generation, bound.ConsentGeneration)
	}

	// A second consent writer using st.ExpectConsent() is now rejected with ErrStaleGeneration.
	if _, err := Update(dir, "default", st.ExpectConsent(), func(s *State) error {
		return tick(s, "acme", "acme-other")
	}); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale ExpectConsent after Select: got %v, want ErrStaleGeneration", err)
	}

	// ApplyOrgAccessLost advances Generation without advancing ConsentGeneration.
	orgLost, err := Update(dir, "default", Expect{Account: testAccount}, func(s *State) error {
		s.ApplyOrgAccessLost("acme", authURL, map[string]int64{enrID: 1}, now)
		return nil
	})
	must(t, err)
	if orgLost.Generation != 4 || orgLost.ConsentGeneration != 2 {
		t.Fatalf("after ApplyOrgAccessLost: gen=%d consent=%d, want 4, 2", orgLost.Generation, orgLost.ConsentGeneration)
	}

	// Deselect advances ConsentGeneration, and Replace advances it past the prior value.
	deselected, err := Update(dir, "default", bound.ExpectConsent(), func(s *State) error {
		s.Deselect(enrID)
		return nil
	})
	must(t, err)
	if deselected.ConsentGeneration != 3 {
		t.Fatalf("after Deselect: consent=%d, want 3", deselected.ConsentGeneration)
	}
	replaced, err := Replace(dir, "default", nil, testAccount, func(s *State) error { return nil })
	must(t, err)
	if replaced.ConsentGeneration != 4 {
		t.Fatalf("after Replace: consent=%d, want 4", replaced.ConsentGeneration)
	}

	// Expect with ConsentGeneration but empty StateID is refused.
	g := int64(2)
	if _, err := Update(dir, "default", Expect{Account: testAccount, ConsentGeneration: &g}, func(s *State) error {
		return nil
	}); err == nil {
		t.Fatal("Expect with ConsentGeneration and empty StateID succeeded")
	}
}

// State.Team returns a defensive copy whose slice mutations cannot alias State.
func TestTeamReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()
	s := fixture(t, team("acme", "acme-platform", "github.com/acme/api"))
	id := enrollment(t, s, "acme-platform")
	if _, ok := s.Team("missing"); ok {
		t.Fatal("Team(missing) returned ok=true")
	}
	cp, ok := s.Team(id)
	if !ok {
		t.Fatalf("Team(%s) returned ok=false", id)
	}
	cp.Policy.Repositories[0] = "github.com/mutated/repo"
	cp.Refusals = 99
	fresh, _ := s.Team(id)
	if fresh.Policy.Repositories[0] != "github.com/acme/api" || fresh.Refusals != 0 {
		t.Fatalf("State.Team aliased internal state: %+v", fresh)
	}
}

func TestDeleteCorruptAndReplaceCorruptUnderLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := DeleteCorrupt(dir, "default"); !errors.Is(err, ErrNoState) {
		t.Fatalf("DeleteCorrupt absent = %v, want ErrNoState", err)
	}
	if _, err := ReplaceCorrupt(dir, "default", testAccount, func(*State) error { return nil }); !errors.Is(err, ErrNoState) {
		t.Fatalf("ReplaceCorrupt absent = %v, want ErrNoState", err)
	}

	st, err := Create(dir, "default", testAccount, func(s *State) error {
		return tick(s, "acme", "acme-api")
	})
	must(t, err)
	if err := DeleteCorrupt(dir, "default"); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("DeleteCorrupt on valid state = %v, want ErrStaleGeneration", err)
	}
	if _, err := ReplaceCorrupt(dir, "default", bobAccount, func(*State) error { return nil }); !errors.Is(err, ErrStateExists) {
		t.Fatalf("ReplaceCorrupt on valid state = %v, want ErrStateExists", err)
	}
	still, ok, err := Load(dir, "default")
	if err != nil || !ok || still.StateID != st.StateID || still.Account != testAccount {
		t.Fatalf("valid state altered by corrupt helpers: %+v ok=%v err=%v", still, ok, err)
	}

	path, err := StatePath(dir, "default")
	must(t, err)
	must(t, os.WriteFile(path, []byte("{corrupt"), 0o600))
	repaired, err := ReplaceCorrupt(dir, "default", bobAccount, func(s *State) error {
		return tick(s, "bobco", "bob-app")
	})
	must(t, err)
	if repaired.Account != bobAccount || repaired.Generation != 1 || len(repaired.Teams) != 1 || repaired.Teams[0].OrganizationID != "bobco" {
		t.Fatalf("repaired = %+v", repaired)
	}

	must(t, os.WriteFile(path, []byte("{corrupt-again"), 0o600))
	must(t, DeleteCorrupt(dir, "default"))
	if _, ok, err := Load(dir, "default"); err != nil || ok {
		t.Fatalf("corrupt file survived DeleteCorrupt: ok=%v err=%v", ok, err)
	}
}

// tick selects a new team id in organization org, for a test that needs some write.
func tick(s *State, org, id string) error {
	_, err := s.Select(team(org, id))
	return err
}
