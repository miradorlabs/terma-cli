package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A personal organization's team migrates as personal: no bindings are seeded, and its
// global policy keeps collecting everything not bound to an organization, inside git and
// out, as today's global mode did.
func TestRunMigratesAPersonalTeamAsPersonal(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.OrgType = orgsOfType(map[string]string{"me": OrgPersonal})
	mine := legacy{profile: "default", org: "me", team: "me-personal", global: true, key: "ter_srv_me"}
	m.setUp(t, mine)
	m.commit(t)
	m.legacyRetired(t)

	s := m.state(t, "default")
	if len(s.Teams) != 1 || len(s.Bindings) != 0 {
		t.Fatalf("the personal profile migrated to %+v, bindings %v", s.Teams, s.Bindings)
	}
	tm := s.Teams[0]
	if !tm.Personal || !tm.Policy.Global() || tm.TeamID != mine.team || s.Migration.Legacy[mine.team] != tm.EnrollmentID {
		t.Fatalf("the personal team became %+v (legacy %v)", tm, s.Migration.Legacy)
	}
	r := routing.NewRouter(s)
	for _, loc := range []routing.Location{{Kind: routing.OutsideGit}, {Kind: routing.Repository, Repository: "github.com/dawson/side"}, {Kind: routing.Unknown}} {
		if d := r.Route(loc, time.Now()); d.Outcome != routing.Send || d.EnrollmentID != tm.EnrollmentID {
			t.Errorf("%+v: %+v, want it sent to personal as before", loc, d)
		}
	}
}

// A personal team with a repository list keeps it, and binds none of it.
func TestRunBindsNothingForAPersonalTeamsList(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.OrgType = orgsOfType(map[string]string{"me": OrgPersonal})
	mine := legacy{profile: "default", org: "me", team: "me-personal", entries: []string{"github.com/dawson/*", "github.com/dawson/terma"}, wildcards: true}
	m.setUp(t, mine)
	m.commit(t)
	s := m.state(t, "default")
	if tm := s.Teams[0]; !tm.Personal || tm.Policy.Global() || len(tm.Policy.Repositories) != 2 || len(s.Bindings) != 0 {
		t.Fatalf("the personal team became %+v, bindings %v", tm, s.Bindings)
	}
	if d := routing.NewRouter(s).Route(routing.Location{Kind: routing.Repository, Repository: "github.com/dawson/side"}, time.Now()); d.Outcome != routing.Send {
		t.Fatalf("a listed repository of personal: %+v", d)
	}
}

// Whether a team is personal is asked before the migration takes the spool flush's lock,
// with each signed-in profile's own sign-in to its team's organization. One that cannot
// be asked keeps the whole machine on today's rules, recorded for doctor; the next start
// that can ask migrates, and the record goes.
func TestAnUnknownOrgTypeDefersTheMigration(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func(context.Context, string, string) (string, error){
		"refused":     func(context.Context, string, string) (string, error) { return "", errors.New("401 sign in again") },
		"old server":  func(context.Context, string, string) (string, error) { return "", nil },
		"no question": nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := severalMachine(t)
			must(t, os.MkdirAll(filepath.Join(m.StateDir, spool.Dir), 0o700))
			var asked []string
			lockFree := true
			m.OrgType = func(ctx context.Context, profile, org string) (string, error) {
				asked = append(asked, profile+"/"+org)
				unlock, err := flock.TryLock(filepath.Join(m.StateDir, spool.Dir, spool.DeliveryLockFile))
				if err != nil {
					lockFree = false
				} else {
					unlock()
				}
				return answer(ctx, profile, org)
			}
			if answer == nil {
				m.OrgType = nil
			}
			before := snapshot(t, m)
			if out, err := Run(context.Background(), m); err != nil || out != NotMigrated {
				t.Fatalf("Run = %s, %v", out, err)
			}
			if after := snapshot(t, m); !mapsEqual(before, after) || config.Migrated(m.ConfigDir) {
				t.Fatal("a migration that could not ask an organization's type changed the machine")
			}
			b, ok := ReadBlock(m.StateDir)
			if !ok || b.Reason != OrgTypeUnknown || !strings.Contains(b.Detail, "default") || b.Unfinished {
				t.Fatalf("doctor's record = %+v, %v", b, ok)
			}
			if !lockFree {
				t.Fatal("the organization type was asked while the spool flush's lock was held")
			}
			if answer != nil && (len(asked) == 0 || asked[0] != "default/acme") {
				t.Fatalf("asked %v, want default's own sign-in to acme first", asked)
			}

			m.OrgType = orgsOfType(nil)
			m.commit(t)
			if _, ok := ReadBlock(m.StateDir); ok {
				t.Fatal("the record outlived the migration")
			}
			for _, l := range several {
				m.migrated(t, l)
			}
		})
	}
}

// A profile set up with a server key has no sign-in to ask with: its team migrates as an
// organization's, single-team as before, and asks nothing.
func TestRunAsksNoTypeForAServerKeyProfile(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	var asked atomic.Int32
	m.OrgType = func(context.Context, string, string) (string, error) {
		asked.Add(1)
		return "", errors.New("no sign-in")
	}
	ci := legacy{profile: "ci", org: "acme", team: "acme-platform", entries: []string{"github.com/acme/api"}, serverKey: true, key: "ter_srv_ci"}
	m.setUp(t, ci)
	m.commit(t)
	m.migrated(t, ci)
	if asked.Load() != 0 {
		t.Fatal("asked an organization type with no sign-in")
	}
}

// A migration a crash left part way, once the machine is marked migrated, finishes from
// what it prepared: it asks nothing more, so a sign-in refused meanwhile cannot hold it.
func TestFinishingAMigrationAsksNoTypes(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.OrgType = orgsOfType(map[string]string{"me": OrgPersonal})
	mine := legacy{profile: "default", org: "me", team: "me-personal", global: true, key: "ter_srv_me"}
	m.setUp(t, mine)
	m.fault = crashAt("retire-keys")
	if out, err := Run(context.Background(), m); !errors.Is(err, errCrash) || out != Unfinished {
		t.Fatalf("Run = %s, %v", out, err)
	}
	m.fault = nil
	m.OrgType = func(context.Context, string, string) (string, error) { return "", errors.New("asked again") }
	m.commit(t)
	if tm := m.state(t, "default").Teams[0]; !tm.Personal || !tm.Policy.Global() {
		t.Fatalf("the finished migration lost what the first start learned: %+v", tm)
	}
}

// Run records that it is migrating, and since when, from before it asks anything until
// it has an outcome: gone once it has committed or recorded why not, and gone when the
// relay stops it first, which changes nothing. `terma teams` waits on it.
func TestRunRecordsThatItIsMigrating(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func(ctx context.Context, stop func()) (string, error){
		"committed": func(context.Context, func()) (string, error) { return OrgShared, nil },
		"blocked":   func(context.Context, func()) (string, error) { return "", errors.New("401 sign in again") },
		"stopped": func(ctx context.Context, stop func()) (string, error) {
			stop()
			<-ctx.Done()
			return "", ctx.Err()
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := severalMachine(t)
			started := time.Now()
			ctx, cancel := context.WithCancel(context.Background())
			var recorded bool
			m.OrgType = func(context.Context, string, string) (string, error) {
				since, ok := Migrating(m.StateDir)
				recorded = recorded || ok && !since.Before(started.Truncate(time.Second))
				return answer(ctx, cancel)
			}
			out, _ := Run(ctx, m)
			cancel()
			if !recorded {
				t.Fatal("not recorded while asking the organization types")
			}
			if _, ok := Migrating(m.StateDir); ok {
				t.Fatalf("the record outlived the outcome (%s)", out)
			}
			_, blocked := ReadBlock(m.StateDir)
			switch {
			case name == "committed" && (out != Committed || blocked),
				name == "blocked" && (out != NotMigrated || !blocked),
				name == "stopped" && (out != NotMigrated || blocked || config.Migrated(m.ConfigDir)):
				t.Fatalf("Run = %s, block recorded %v", out, blocked)
			}
		})
	}
}
