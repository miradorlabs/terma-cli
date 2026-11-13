package migrate

import (
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// A scenario is where a record ran: a checkout's origin remote as git reports it, or ""
// outside git.
type scenario struct {
	name, remote string
	outsideGit   bool
	unknown      bool
}

var scenarios = []scenario{
	{name: "a listed repository over scp", remote: "git@github.com:acme/api.git"},
	{name: "a listed repository over https", remote: "https://github.com/acme/api"},
	{name: "a listed repository over ssh with a port", remote: "ssh://git@github.com:22/acme/api.git"},
	{name: "a listed repository in mixed case", remote: "https://GitHub.com/Acme/API"},
	{name: "a listed repository with credentials and a trailing slash", remote: "https://user:token@github.com/acme/api/"},
	{name: "a listed GitLab subgroup repository", remote: "https://gitlab.com/acme/platform/web.git"},
	{name: "a repository below a listed one", remote: "https://gitlab.com/acme/platform/web/extra"},
	{name: "a group above a listed repository", remote: "https://gitlab.com/acme/platform"},
	{name: "an unlisted repository of the same owner", remote: "git@github.com:acme/secret.git"},
	{name: "a repository of another owner", remote: "git@github.com:dawson/side.git"},
	{name: "a repository whose name extends a listed one", remote: "git@github.com:acme/api-v2.git"},
	{name: "a local path as origin", remote: "/srv/git/acme/api.git"},
	{name: "outside git", outsideGit: true},
	{name: "an unknown location", unknown: true},
}

// location is where s ran as a migrated hook or relay names it, and the repository as
// today's admission names it (gitx.RepositoryID).
func (s scenario) location() (routing.Location, config.Repository) {
	switch {
	case s.unknown:
		return routing.Location{Kind: routing.Unknown}, config.Repository{}
	case s.outsideGit:
		return routing.Location{Kind: routing.OutsideGit}, config.Repository{}
	}
	origin := gitx.RepositoryID(s.remote)
	if origin == "" {
		return routing.Location{Kind: routing.OutsideGit}, config.Repository{}
	}
	return routing.Location{Kind: routing.Repository, Repository: origin}, config.Repository{Origin: origin}
}

// The transition test: a single-team developer in repository mode routes every scenario
// exactly as before the migration. What today's rules collect (the team's validated,
// unexpired policy admits it) goes to the team's cutover enrollment and no other, and
// nothing else leaves; a record queued before the migration for the team is sent on the
// same terms, through the legacy map.
func TestAMigratedSingleTeamProfileRoutesAsBefore(t *testing.T) {
	t.Parallel()
	for _, age := range []time.Duration{time.Hour, config.MaxPolicyAge + time.Hour} {
		m := newMachine(t)
		m.setUp(t, acme)
		if age != time.Hour {
			cfg, err := config.Load(m.ConfigDir, m.StateDir, config.Overrides{})
			must(t, err)
			cfg.Policy.FetchedAt = time.Now().Add(-age)
			must(t, config.WritePolicy(m.StateDir, cfg.Policy))
		}
		// Today's rules, as the hooks, the relay and delivery read them before the migration.
		before, err := config.Load(m.ConfigDir, m.StateDir, config.Overrides{})
		must(t, err)
		today := func(repo config.Repository) bool {
			return before.Policy.Validated() && !before.Policy.Expired(time.Now()) && before.Policy.Admits(repo)
		}

		m.commit(t)
		s := m.state(t, "default")
		cutover := s.Migration.Legacy[acme.team]
		r := routing.NewRouter(s)
		now := time.Now()
		collected := 0
		for _, sc := range scenarios {
			loc, repo := sc.location()
			want := today(repo)
			if want {
				collected++
			}
			d := r.Route(loc, now)
			if got := d.Outcome == routing.Send; got != want || got && d.EnrollmentID != cutover {
				t.Errorf("policy %s old, %s: Route = %+v, today's rules collect it: %v", age, sc.name, d, want)
			}
			if d.Outcome == routing.Hold && d.Reason == routing.ReasonPending {
				t.Errorf("%s: a binding is pending after the upgrade", sc.name)
			}
			// A record spooled for the team before the migration.
			v, _ := r.Recheck(routing.Destination{EnrollmentID: s.Migration.Legacy[acme.team], Location: loc}, now)
			if got := v == routing.SendNow; got != want {
				t.Errorf("policy %s old, %s: a legacy record's recheck = %s, today's rules send it: %v", age, sc.name, v, want)
			}
		}
		if wantCollected := map[bool]int{true: 6, false: 0}[age == time.Hour]; collected != wantCollected {
			t.Fatalf("policy %s old: today's rules collect %d scenarios, want %d", age, collected, wantCollected)
		}
		// A record for any other project maps to nothing, and is dropped.
		if id := s.Migration.Legacy["acme-old-team"]; id != "" {
			t.Fatalf("another project maps to %s", id)
		}
	}
}
