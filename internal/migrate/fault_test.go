package migrate

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

var errCrash = errors.New("crashed")

// several are the profiles the fault tests migrate: two teams of two organizations, and a
// profile with no team.
var several = []legacy{acme, clientco, bare}

// severalMachine is a machine with several's profiles on it.
func severalMachine(t *testing.T) Machine {
	t.Helper()
	m := newMachine(t)
	for _, l := range several {
		m.setUp(t, l)
	}
	return m
}

// steps are the steps a migration of several's machine runs, in order.
func steps(t *testing.T) []string {
	t.Helper()
	m := severalMachine(t)
	var names []string
	m.fault = func(step string) error {
		names = append(names, step)
		return nil
	}
	m.commit(t)
	want := []string{"prepare/bare", "prepare/default", "prepare/work", "copy-sign-ins", "copy-keys/default", "copy-keys/work",
		"retire-keys", "retire-sign-ins", "retire-teams", "commit/bare", "commit/default", "commit/work"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("the steps are %v, want %v", names, want)
	}
	return names
}

// crashAt fails the first time the migration reaches step.
func crashAt(step string) func(string) error {
	return func(s string) error {
		if s == step {
			return errCrash
		}
		return nil
	}
}

// legacyIntact reports whether every legacy entry several put on m is still where today's
// rules read it: each team in config.json, each key in keys.json, each sign-in in
// credentials.json.
func (m Machine) legacyIntact(t *testing.T) bool {
	t.Helper()
	file, err := config.LoadFile(m.ConfigDir)
	must(t, err)
	creds, err := os.ReadFile(config.CredentialsPath(m.ConfigDir))
	must(t, err)
	for _, l := range several {
		if file.Profiles[l.profile].Team != l.team {
			return false
		}
		if l.key != "" {
			if key, err := keystore.Get(m.ConfigDir, l.team); err != nil || key != l.key {
				return false
			}
		}
		if !strings.Contains(string(creds), `"`+l.org+`"`) {
			return false
		}
	}
	return true
}

// phases are the migration phases of m's routing states, by profile.
func (m Machine) phases(t *testing.T) map[string]routing.MigrationPhase {
	t.Helper()
	out := map[string]routing.MigrationPhase{}
	for _, profile := range sortedProfiles(t, m) {
		if s := m.state(t, profile); s.Migration != nil {
			out[profile] = s.Migration.Phase
		}
	}
	return out
}

// A crash at any step, and the restart after it. A crash before step 2 changes nothing.
// A crash in steps 2 to 4 leaves either every legacy entry in place and the machine
// unmigrated, so every reader keeps today's rules, or a migrated machine whose states
// are Preparing: never a machine that has lost legacy entries and is not marked migrated.
// The restart finishes from where the crash left, with the enrollments it had already
// chosen, and commits.
func TestACrashAtEveryStepIsFinishedByTheNextStart(t *testing.T) {
	t.Parallel()
	for _, step := range steps(t) {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			m := severalMachine(t)
			before := snapshot(t, m)
			m.fault = crashAt(step)
			out, err := Run(context.Background(), m)
			if !errors.Is(err, errCrash) {
				t.Fatalf("Run = %s, %v", out, err)
			}
			b, ok := ReadBlock(m.StateDir)
			if !ok || b.Reason != Failed || !strings.Contains(b.Detail, "crashed") {
				t.Fatalf("doctor's record = %+v, %v", b, ok)
			}
			migrated := config.Migrated(m.ConfigDir)
			switch {
			case step == "prepare/bare":
				if after := snapshot(t, m); !mapsEqual(before, after) || out != NotMigrated {
					t.Fatalf("a crash before step 2 changed the machine (%s)", out)
				}
			case !migrated:
				if !m.legacyIntact(t) || out != NotMigrated || b.Unfinished {
					t.Fatalf("an unmigrated machine lost legacy entries (%s, %+v)", out, b)
				}
			default:
				if out != Unfinished || !b.Unfinished || !Needed(m.ConfigDir, m.StateDir) {
					t.Fatalf("a migrated machine with Preparing states: %s, %+v", out, b)
				}
			}
			chosen := map[string]string{}
			for profile, phase := range m.phases(t) {
				if migrated && strings.HasPrefix(step, "commit/") && profile < strings.TrimPrefix(step, "commit/") {
					if phase != routing.Committed {
						t.Fatalf("%s committed before the crash is %s", profile, phase)
					}
				} else if phase != routing.Preparing {
					t.Fatalf("%s is %s before the migration finished", profile, phase)
				}
				if s := m.state(t, profile); len(s.Teams) == 1 {
					chosen[profile] = s.Teams[0].EnrollmentID
				}
			}

			m.fault = nil
			m.commit(t)
			for _, l := range several {
				id := m.migrated(t, l)
				if was, ok := chosen[l.profile]; ok && was != id {
					t.Fatalf("%s's enrollment moved from %s to %s across the restart", l.profile, was, id)
				}
			}
			m.legacyRetired(t)
			if _, ok := ReadBlock(m.StateDir); ok {
				t.Fatal("the finished migration left doctor's record")
			}
		})
	}
}

// A crash at every step in turn, each restart crashing at the next step it reaches, still
// ends migrated with the enrollments first chosen.
func TestACrashOnEveryRestartStillFinishes(t *testing.T) {
	t.Parallel()
	m := severalMachine(t)
	chosen := map[string]string{}
	for _, step := range steps(t) {
		m.fault = crashAt(step)
		out, err := Run(context.Background(), m)
		if err == nil {
			// Already past it: the machine is migrated, and prepares nothing again.
			if !strings.HasPrefix(step, "prepare/") || out != Done && out != Committed {
				t.Fatalf("Run past %s = %s", step, out)
			}
			continue
		}
		for _, l := range several {
			if s, ok, _ := routing.Load(m.StateDir, l.profile); ok && len(s.Teams) == 1 {
				if was, seen := chosen[l.profile]; seen && was != s.Teams[0].EnrollmentID {
					t.Fatalf("crashing at %s moved %s's enrollment", step, l.profile)
				}
				chosen[l.profile] = s.Teams[0].EnrollmentID
			}
		}
	}
	m.fault = nil
	if out, err := Run(context.Background(), m); err != nil || out != Committed && out != Done {
		t.Fatalf("the last restart = %s, %v", out, err)
	}
	for _, l := range several {
		if id := m.migrated(t, l); l.team != "" && chosen[l.profile] != id {
			t.Fatalf("%s ended on enrollment %s, not %s", l.profile, id, chosen[l.profile])
		}
	}
}

// A step that fails for real, not a crash, behaves the same: a keychain that will not
// give up the legacy sign-ins before they are copied leaves every legacy entry in place,
// and the start after it is unlocked migrates.
func TestALockedKeychainLeavesTheMachineAsItWas(t *testing.T) {
	t.Parallel()
	m := severalMachine(t)
	before := snapshot(t, m)
	unlock := secret.FailForTest(t, m.ConfigDir)
	out, err := Run(context.Background(), m)
	unlock()
	if err == nil || out != NotMigrated || config.Migrated(m.ConfigDir) || !m.legacyIntact(t) {
		t.Fatalf("Run with the keychain locked = %s, %v", out, err)
	}
	after := snapshot(t, m)
	for path, data := range before {
		if after[path] != data {
			t.Fatalf("a failed migration changed %s", path)
		}
	}
	m.commit(t)
	for _, l := range several {
		m.migrated(t, l)
	}
}

// A profile config.json no longer has, whose state an earlier start prepared, is not
// committed with a team nobody selects: its state goes.
func TestPreparingAgainDropsAProfileThatWentAway(t *testing.T) {
	t.Parallel()
	m := severalMachine(t)
	m.fault = crashAt("copy-sign-ins")
	if _, err := Run(context.Background(), m); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	must(t, config.UpdateFile(m.ConfigDir, func(f *config.File) { delete(f.Profiles, "work") }))
	m.fault = nil
	m.commit(t)
	if _, ok, err := routing.Load(m.StateDir, "work"); ok || err != nil {
		t.Fatalf("the state of a profile that went away is still there: %v", err)
	}
	m.migrated(t, acme)
}

// A team changed by an older terma between a failed start and the next is prepared again:
// the next start enrolls the team the profile has now.
func TestPreparingAgainFollowsATeamChangedMeanwhile(t *testing.T) {
	t.Parallel()
	m := severalMachine(t)
	m.fault = crashAt("copy-sign-ins")
	if _, err := Run(context.Background(), m); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	first := m.state(t, "default").Teams[0].EnrollmentID
	moved := legacy{profile: "default", org: "acme", team: "acme-mobile", entries: []string{"github.com/acme/ios"}, key: "ter_srv_mobile"}
	m.setUp(t, moved)
	m.fault = nil
	m.commit(t)
	if id := m.migrated(t, moved); id == first {
		t.Fatal("a different team kept the earlier team's enrollment")
	}
}
