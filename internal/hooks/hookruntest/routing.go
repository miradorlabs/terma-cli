package hookruntest

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// A migrated machine routes by its profile's routing state: these build one for a hook's
// tests, with the organizations, teams and bindings each test names.

// AuthURL is the sign-in environment every routed fixture's teams were fetched in.
const AuthURL = "https://auth.example"

// RoutedProfile is the profile every routed fixture's state belongs to.
const RoutedProfile = "default"

// RoutedTeam is team, of organization org, with a policy validated an hour ago listing
// entries, wildcards allowed, and turning git_hooks on as gitHooks says.
func RoutedTeam(org, team string, gitHooks bool, entries ...string) routing.Team {
	return routing.Team{OrganizationID: org, AuthURL: AuthURL, TeamID: team, Policy: config.Policy{
		Mode: config.ModeRepo, TeamID: team, OrganizationID: org, AuthURL: AuthURL, Repositories: entries, Wildcards: true,
		GitHooks: gitHooks, IncludePrompts: true, IncludeToolContent: true, Revision: 1, FetchedAt: time.Now().Add(-time.Hour),
	}}
}

// RoutedPersonal is the developer's personal team, organization "me": collecting
// everything not bound to an organization, or only entries when given, commit trailers on.
func RoutedPersonal(entries ...string) routing.Team {
	t := RoutedTeam("me", "personal", true, entries...)
	t.Personal = true
	if len(entries) == 0 {
		t.Policy.Mode = config.ModeGlobal
	}
	return t
}

// RoutingState is RoutedProfile's routing state selecting teams in order, with binds, an
// owner or a repository each, bound to the organization it names.
func RoutingState(t testing.TB, teams []routing.Team, binds map[string]string) *routing.State {
	t.Helper()
	s := &routing.State{Profile: RoutedProfile}
	for _, tm := range teams {
		if _, err := s.Select(tm); err != nil {
			t.Fatal(err)
		}
	}
	for key, org := range binds {
		if err := s.Bind(key, org, true); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// TwoOrganizations is the plan's two-organization machine: Acme Platform ("acme-platform")
// collecting github.com/acme/* with git_hooks on, github.com/acme bound to Acme, then
// personal ("personal") collecting everything else.
func TwoOrganizations(t testing.TB) *routing.State {
	t.Helper()
	return RoutingState(t, []routing.Team{RoutedTeam("acme", "acme-platform", true, "github.com/acme/*"), RoutedPersonal()},
		map[string]string{"github.com/acme": "acme"})
}

// Enrollment is the enrollment of the selected team id in s.
func Enrollment(t testing.TB, s *routing.State, id string) string {
	t.Helper()
	for _, tm := range s.Teams {
		if tm.TeamID == id {
			return tm.EnrollmentID
		}
	}
	t.Fatalf("team %s is not selected", id)
	return ""
}

// WriteRoutingState writes s as its profile's routing state under stateDir.
func WriteRoutingState(t testing.TB, stateDir string, s *routing.State) {
	t.Helper()
	if _, err := routing.Create(stateDir, s.Profile, func(st *routing.State) error {
		*st = *s
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// RepoAt is a git repository with origin https://<origin>.git, in a folder named for the
// origin's last segment, and its resolved path: what a routed test places sessions in.
func RepoAt(t testing.TB, origin string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(parent, filepath.Base(origin))
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", root},
		{"-C", root, "config", "core.hooksPath", t.TempDir()},
		{"-C", root, "config", "user.email", "dev@example.com"},
		{"-C", root, "config", "user.name", "Dev"},
		{"-C", root, "config", "commit.gpgsign", "false"},
		{"-C", root, "remote", "add", "origin", "https://" + origin + ".git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return root
}
