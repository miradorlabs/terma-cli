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

// Refreshes change a team's grant only from the current epoch: a success already in
// flight never undoes a refusal, and a refusal suspends at once.
func TestApplyRefreshKeepsRefusalsInOrder(t *testing.T) {
	t.Parallel()
	s := fixture(t)
	id := enrollment(t, s, "acme-platform")
	ok := func(rev int64) *config.Policy {
		return &config.Policy{Mode: config.ModeRepo, TeamID: "acme-platform", OrganizationID: "acme", Repositories: []string{"github.com/acme/*"}, Wildcards: true, Revision: rev, FetchedAt: now}
	}
	apply := func(r Refresh) bool {
		t.Helper()
		changed, err := s.ApplyRefresh(r)
		must(t, err)
		return changed
	}
	state := func() TeamState { return s.team(id).StateAt(now) }

	inFlight := s.team(id).Epoch
	apply(Refresh{EnrollmentID: id, Epoch: inFlight, Failure: Terminal})
	if state() != Suspended {
		t.Fatalf("one terminal refusal left the team %s", state())
	}
	if apply(Refresh{EnrollmentID: id, Epoch: inFlight, Policy: ok(2)}) || state() != Suspended {
		t.Fatal("a success started before the refusal undid it")
	}
	apply(Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Failure: Transient})
	if state() != Suspended {
		t.Fatal("a network failure cleared a suspension")
	}
	apply(Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Failure: Terminal})
	if state() != Refused {
		t.Fatalf("two terminal refusals in a row left the team %s", state())
	}
	apply(Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Policy: ok(2)})
	if state() != Active {
		t.Fatal("the daily recheck's success did not restore the team")
	}
	if _, err := s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Policy: ok(1)}); err == nil {
		t.Fatal("accepted an older revision")
	}
	beforeSignIn := s.team(id).Epoch
	apply(Refresh{EnrollmentID: id, Epoch: beforeSignIn, Failure: SignIn})
	if state() != SignInNeeded {
		t.Fatal("a refused sign-in did not block the team")
	}
	if apply(Refresh{EnrollmentID: id, Epoch: beforeSignIn, Policy: ok(3)}) || state() != SignInNeeded {
		t.Fatal("a success started under the refused sign-in cleared it")
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

// Personal stays last, and a team joins only with its own validated policy.
func TestSelect(t *testing.T) {
	t.Parallel()
	s := &State{Profile: "default"}
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
	if _, err := Update(dir, "default", func(*State) error { return nil }); !errors.Is(err, ErrNoState) {
		t.Fatalf("Update made a routing state that did not exist: %v", err)
	}
	if _, err := Create(dir, "default", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(dir, "default", func(*State) error { return nil }); err == nil {
		t.Fatal("Create overwrote an existing routing state")
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Update(dir, "default", func(s *State) error {
				return s.Bind(fmt.Sprintf("github.com/owner%d", i), "acme", false)
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	s, ok, err := Load(dir, "default")
	if err != nil || !ok || len(s.Bindings) != 20 || s.Generation != 21 {
		t.Fatalf("after 20 concurrent writes: %d bindings, generation %d, %v", len(s.Bindings), s.Generation, err)
	}
	for _, noop := range []func(*State) error{
		func(s *State) error { return s.Bind("github.com/owner0", "acme", false) },
		func(s *State) error { return s.Unbind("github.com/never-bound") },
	} {
		if again, err := Update(dir, "default", noop); err != nil || again.Generation != s.Generation {
			t.Fatalf("a change that changed nothing was written: generation %d, %v", again.Generation, err)
		}
	}

	if _, err := Update(dir, "default", func(s *State) error {
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
