package routing

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// An origin in any form a remote takes lands on the same key as the policy's entry.
func TestOneNormaliserForEveryForm(t *testing.T) {
	t.Parallel()
	s := fixture(t, team("acme", "acme-platform", "github.com/acme/secret"), personal())
	for _, form := range []string{
		"git@github.com:acme/secret",
		"git@github.com:Acme/Secret.git",
		"https://github.com/acme/secret.git",
		"https://user:token@github.com:443/acme/secret",
		"ssh://git@github.com/acme/secret",
		"github.com:443/acme/secret",
		"user@github.com/acme/secret",
		"GitHub.com/ACME/secret/",
	} {
		expect(t, s, repo(form), Send, "acme-platform")
	}
	for _, key := range []string{
		"github.com/acme//secret", "github.com/acme/../secret", "github.com/./acme/x",
		"github.com/acme/*?note", "github.com/acme/*#note", "github.com/acme/api?x", "https://github.com/acme/api#main",
	} {
		if _, ok := ParseEntry(key, true); ok {
			t.Errorf("parsed %q as an entry", key)
		}
	}
}

// Normalize is idempotent, so a key stored normalised compares equal to every reading of
// it: `.GIT` strips like `.git`, and percent-encoding names no repository.
func TestNormalizeIsIdempotent(t *testing.T) {
	t.Parallel()
	for _, form := range []string{
		"git@github.com:acme/secret", "git@github.com:Acme/Secret.git", "https://github.com/acme/api.GIT",
		"git@github.com:acme/api.Git", "https://user:token@github.com:443/acme/secret", "ssh://git@github.com/acme/secret",
		"github.com:443/acme/secret", "user@github.com/acme/secret", "GitHub.com/ACME/secret/", "github.com/acme/*",
		"github.com/a%2541/b", "gitlab.com/acme/platform/api",
	} {
		once := Normalize(form)
		if twice := Normalize(once); twice != once {
			t.Errorf("Normalize(%q) = %q, but again %q", form, once, twice)
		}
	}
	if Normalize("github.com/a%2541/b") != "" {
		t.Error("a percent-encoded path named a repository")
	}
	s := fixture(t, team("acme", "acme-platform", "github.com/acme/api.GIT"), personal())
	for _, origin := range []string{"https://github.com/acme/api.GIT", "git@github.com:acme/api.Git"} {
		expect(t, s, repo(origin), Send, "acme-platform")
	}
	if err := s.check(); err != nil {
		t.Fatal(err)
	}
}

// Blocked teams hold rather than pass on, and nothing malformed reaches personal.
func TestRouteEdges(t *testing.T) {
	t.Parallel()
	t.Run("a blocked personal team holds what it collects", func(t *testing.T) {
		s := fixture(t)
		s.Teams[2].SignInNeeded = true
		expect(t, s, repo("github.com/dawson/side"), Hold, "personal")
		expect(t, s, Location{Kind: OutsideGit}, Hold, "personal")
		if d := NewRouter(*s).Route(Location{Kind: OutsideGit}, now); d.Reason != ReasonBlocked || d.Blocked != SignInNeeded {
			t.Errorf("a blocked hold said %s/%s", d.Reason, d.Blocked)
		}
	})
	t.Run("a repository that names no repository is held, never personal's", func(t *testing.T) {
		alone := &State{Profile: "default", Account: testAccount}
		if _, err := alone.Select(personal()); err != nil {
			t.Fatal(err)
		}
		for _, r := range []string{"", "github.com", "github.com/acme", "/home/me/repo"} {
			expect(t, alone, repo(r), Hold, "")
		}
	})
	t.Run("a router keeps its own copy of the state", func(t *testing.T) {
		s := fixture(t)
		r := NewRouter(*s)
		s.Teams[0].Refusals = 2
		if d := r.Route(repo("github.com/acme/api"), now); d.Outcome != Send {
			t.Fatalf("changing the state after compiling changed the route: %+v", d)
		}
	})
}

// Route compiles each team's entries once per state: a relay that routes many records
// keeps one Router per state it loads.
func BenchmarkRoute(b *testing.B) {
	for _, n := range []int{10, 100, 500} {
		s := State{Profile: "default", Account: testAccount}
		for i := range 5 {
			entries := make([]string, n)
			for j := range entries {
				entries[j] = fmt.Sprintf("github.com/org%d/repo%d", i, j)
			}
			entries[0] = fmt.Sprintf("github.com/org%d/*", i)
			if _, err := s.Select(team(fmt.Sprintf("org%d", i), fmt.Sprintf("team%d", i), entries...)); err != nil {
				b.Fatal(err)
			}
		}
		r := NewRouter(s)
		loc := repo(fmt.Sprintf("github.com/org4/repo%d", n-1))
		b.Run(fmt.Sprintf("%d entries per team", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if r.Route(loc, now).Outcome != Send {
					b.Fatal("not sent")
				}
			}
		})
	}
}

// Normalize is idempotent.
func FuzzNormalize(f *testing.F) {
	for _, seed := range []string{
		"git@github.com:acme/api.Git", "https://user:tok@github.com:443/acme/api.GIT/", "github.com/acme/a%3fb",
		"github.com/acme/a%2541", "github.com/acme/a%23b", "gitlab.com/acme/platform/*", "github.com//acme/api",
		"github.com/acme/../api", "ssh://git@github.com/acme/api", "HTTPS://GitHub.com/Acme/API.GIT/",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		once := Normalize(raw)
		if twice := Normalize(once); twice != once {
			t.Fatalf("Normalize(%q) = %q, but again %q", raw, once, twice)
		}
	})
}

// A team is selected only with its organization.
func TestSelectNeedsAnOrganization(t *testing.T) {
	t.Parallel()
	s := &State{Profile: "default", Account: testAccount}
	if _, err := s.Select(team("", "orphan", "github.com/acme/*")); err == nil {
		t.Fatal("selected a team with no organization")
	}
}

// An encoded wildcard is refused, a policy fetched for another
// organization or environment never becomes a team's grant, and a 401 shows over a
// suspension.
func TestNoGrantFromEncodedEntriesOrForeignPolicies(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"github.com/acme/%2A", "github.com/acme%2F%2A", "github.com/acme/%61pi"} {
		if _, ok := ParseEntry(raw, true); ok {
			t.Errorf("parsed %q as an entry", raw)
		}
	}

	s := fixture(t)
	id := enrollment(t, s, "acme-platform")
	for _, p := range []config.Policy{
		{Mode: config.ModeRepo, TeamID: "acme-platform", OrganizationID: "clientco", AuthURL: authURL, Revision: 5, FetchedAt: now},
		{Mode: config.ModeRepo, TeamID: "acme-platform", OrganizationID: "acme", AuthURL: "https://auth.other", Revision: 5, FetchedAt: now},
	} {
		if _, err := s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Policy: &p}); err == nil {
			t.Errorf("a policy fetched under %s/%s became the team's grant", p.OrganizationID, p.AuthURL)
		}
	}
	other := team("acme", "acme-mobile", "github.com/acme/api")
	other.Policy.OrganizationID = "clientco"
	if _, err := s.Select(other); err == nil {
		t.Error("selected a team with another organization's policy")
	}

	_, err := s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Failure: Terminal})
	must(t, err)
	_, err = s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Failure: SignIn})
	must(t, err)
	if st := s.team(id).StateAt(now); st != SignInNeeded {
		t.Fatalf("a 401 after a suspension shows %s", st)
	}
}

// Ownership is strict, and holds for a loaded state as for Select and ApplyRefresh: a
// policy without its organization and environment is no team's, since every fetch stamps
// both (internal/policy). A wildcard is only an entry typed ending in `/*`.
func TestOwnershipIsStrictEverywhere(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"github.com/acme/*.git", "github.com/acme/*/", "github.com/acme/*.GIT"} {
		if _, ok := ParseEntry(raw, true); ok {
			t.Errorf("parsed %q as a wildcard", raw)
		}
	}

	for name, blank := range map[string]func(*Team){
		"its policy names no organization":      func(tm *Team) { tm.Policy.OrganizationID = "" },
		"its policy names no environment":       func(tm *Team) { tm.Policy.AuthURL = "" },
		"it and its policy name no environment": func(tm *Team) { tm.AuthURL, tm.Policy.AuthURL = "", "" },
		"it and its policy name no team":        func(tm *Team) { tm.TeamID, tm.Policy.TeamID = "", "" },
	} {
		unstamped := team("acme", "acme-platform", "github.com/acme/*")
		blank(&unstamped)
		if _, err := (&State{Profile: "default", Account: testAccount}).Select(unstamped); err == nil {
			t.Errorf("selected a team where %s", name)
		}
	}
	s := fixture(t)
	id := enrollment(t, s, "acme-platform")
	if _, err := s.ApplyRefresh(Refresh{EnrollmentID: id, Epoch: s.team(id).Epoch, Policy: &config.Policy{
		Mode: config.ModeRepo, TeamID: "acme-platform", Revision: 5, FetchedAt: now,
	}}); err == nil {
		t.Error("a refresh with no organization or environment became the team's grant")
	}

	dir := t.TempDir()
	_, err := Create(dir, "default", testAccount, func(st *State) error {
		st.Teams = s.Teams
		return nil
	})
	must(t, err)
	path, _ := StatePath(dir, "default")
	b, err := os.ReadFile(path)
	must(t, err)
	var stale State
	must(t, json.Unmarshal(b, &stale))
	stale.Teams[1].Policy.OrganizationID = "other"
	b, err = json.Marshal(stale)
	must(t, err)
	must(t, os.WriteFile(path, b, 0o600))
	if _, _, err := Load(dir, "default"); err == nil || !strings.Contains(err.Error(), "fetched for another") {
		t.Fatalf("loaded a state whose team holds another organization's policy: %v", err)
	}
}
