package routing

import (
	"fmt"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// A binding or an origin in any form a remote takes lands on the same key: a binding
// made with an scp remote, a port, credentials or `.git` keeps its repository from
// personal exactly as `host/owner/name` does.
func TestOneNormaliserForEveryForm(t *testing.T) {
	t.Parallel()
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
		s := fixture(t, team("acme", "acme-platform", "github.com/acme/api"), personal())
		must(t, s.Unbind("github.com/acme"))
		if err := s.Bind(form, "acme", false); err != nil {
			t.Errorf("Bind(%q): %v", form, err)
			continue
		}
		if _, ok := s.Bindings["github.com/acme/secret"]; !ok || len(s.Bindings) != 2 {
			t.Errorf("Bind(%q) stored %v", form, s.Bindings)
		}
		expect(t, s, repo("github.com/acme/secret"), Drop, "")
		expect(t, s, repo(form), Drop, "")
		if err := s.check(); err != nil {
			t.Errorf("after Bind(%q): %v", form, err)
		}
	}
	bad := fixture(t)
	for _, key := range []string{"git@github.com:acme/*", "github.com/acme//secret"} {
		if err := bad.Bind(key, "acme", false); err == nil {
			t.Errorf("bound %q", key)
		}
	}
	bad.Bindings = map[string]Binding{"git@github.com:acme": {Organization: "acme"}}
	if bad.check() == nil {
		t.Error("check accepted a binding not in normalised form")
	}
	bad.Bindings = map[string]Binding{"github.com/acme": {Organization: "acme", Refused: true}}
	if bad.check() == nil {
		t.Error("check accepted a binding both bound and refused")
	}
}

// What the reviewer found unpinned: blocked teams hold rather than pass on, ties keep
// the developer's order, and nothing malformed or personal-bound reaches personal.
func TestRouteEdges(t *testing.T) {
	t.Parallel()
	t.Run("a blocked personal team holds what it collects", func(t *testing.T) {
		s := fixture(t)
		s.Teams[2].SignInNeeded = true
		expect(t, s, repo("github.com/dawson/side"), Hold, "personal")
		expect(t, s, Location{Kind: OutsideGit}, Hold, "personal")
		if d := Route(Location{Kind: OutsideGit}, *s, now); d.Reason != ReasonBlocked || d.Blocked != SignInNeeded {
			t.Errorf("a blocked hold said %s/%s", d.Reason, d.Blocked)
		}
	})
	t.Run("a blocked, more specific team holds instead of passing to a wildcard", func(t *testing.T) {
		s := fixture(t, team("acme", "acme-platform", "github.com/acme/*"), team("acme", "acme-mobile", "github.com/acme/api"), personal())
		s.Teams[1].Policy.FetchedAt = now.Add(-config.MaxPolicyAge - time.Hour)
		expect(t, s, repo("github.com/acme/api"), Hold, "acme-mobile")
	})
	t.Run("equally specific teams keep the order they were ticked in", func(t *testing.T) {
		s := fixture(t, team("acme", "acme-first", "github.com/acme/*"), team("acme", "acme-second", "github.com/acme/*"), personal())
		expect(t, s, repo("github.com/acme/api"), Send, "acme-first")
	})
	t.Run("a repository that names no repository is held, never personal's", func(t *testing.T) {
		alone := &State{Profile: "default"}
		if _, err := alone.Select(personal()); err != nil {
			t.Fatal(err)
		}
		for _, r := range []string{"", "github.com", "github.com/acme", "/home/me/repo"} {
			expect(t, alone, repo(r), Hold, "")
		}
	})
	t.Run("nothing is bound to the personal organization", func(t *testing.T) {
		if err := fixture(t).Bind("github.com/dawson", "me", false); err == nil {
			t.Fatal("bound an owner to the personal organization")
		}
	})
	t.Run("a pending hold names the binding to decide", func(t *testing.T) {
		s := fixture(t)
		s.Teams[1].Policy.Repositories = append(s.Teams[1].Policy.Repositories, "github.com/dawson/*", "github.com/torvalds/linux")
		for r, key := range map[string]string{"github.com/dawson/side": "github.com/dawson", "github.com/torvalds/linux": "github.com/torvalds/linux"} {
			if d := Route(repo(r), *s, now); d.Reason != ReasonPending || d.Authority.Key != key {
				t.Errorf("Route(%s) held for %q (%s), want %q", r, d.Authority.Key, d.Reason, key)
			}
		}
	})
}

// Route compiles each team's entries once per state: a relay that routes many records
// keeps one Router per state it loads.
func BenchmarkRoute(b *testing.B) {
	for _, n := range []int{10, 100, 500} {
		s := State{Profile: "default"}
		for i := range 5 {
			entries := make([]string, n)
			for j := range entries {
				entries[j] = fmt.Sprintf("github.com/org%d/repo%d", i, j)
			}
			entries[0] = fmt.Sprintf("github.com/org%d/*", i)
			if _, err := s.Select(team(fmt.Sprintf("org%d", i), fmt.Sprintf("team%d", i), entries...)); err != nil {
				b.Fatal(err)
			}
			if err := s.Bind(fmt.Sprintf("github.com/org%d", i), fmt.Sprintf("org%d", i), false); err != nil {
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
