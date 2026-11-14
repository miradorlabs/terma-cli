package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/migrate"
)

// The relay's migration migrates a machine whose developer has not opted in exactly when
// migrateAutomatically says to; the tests below take each value in turn.
func TestTheRelaysMigrationIsAutomaticOnlyByTheConstant(t *testing.T) {
	useConfigDir(t, t.TempDir())
	if m := testApp.relayMigration(""); m.Automatic != migrateAutomatically {
		t.Fatalf("Automatic = %v, migrateAutomatically = %v", m.Automatic, migrateAutomatically)
	}
	if testApp.optedIn() != migrateAutomatically {
		t.Fatalf("a machine with no opt-in counts as opted in: %v", testApp.optedIn())
	}
}

// notOptedIn is legacyPersonalMachine before its developer opted in, and the relay's
// migration of it with migrateAutomatically off; the relay runs exe.
func notOptedIn(t *testing.T) (*fakeAuth, *migrate.Machine) {
	t.Helper()
	f := legacyPersonalMachine(t)
	if err := os.Remove(filepath.Join(testApp.stateDir, migrate.OptInFile)); err != nil {
		t.Fatal(err)
	}
	exe := fakeInstall(t, "build 2")
	hookWith(t, exe)
	m := testApp.relayMigration(exe)
	m.FlushWait, m.Automatic = time.Second, false
	return f, m
}

// With migrateAutomatically off, a relay start leaves a machine whose developer has not
// opted in as it was, and doctor says, as context, that `terma teams` turns routing by
// teams on.
func TestARelayStartLeavesAMachineNotOptedIn(t *testing.T) {
	_, m := notOptedIn(t)
	if out, err := migrate.Run(context.Background(), *m); err != nil || out != migrate.NotOptedIn || config.Migrated(testApp.dir) {
		t.Fatalf("migrate = %s, %v; migrated %v", out, err, config.Migrated(testApp.dir))
	}
	routingRow := func() (doctor.Row, bool) {
		env := testApp.doctorEnv(t.Context())
		for _, r := range doctor.Context(env) {
			if r.Label == "Routing" {
				return r, env.MigrationBlocked == nil
			}
		}
		return doctor.Row{}, false
	}
	if r, ok := routingRow(); !ok || !strings.Contains(r.Value, "`terma teams` turns on routing by teams") {
		t.Fatalf("doctor's routing line = %+v, %v", r, ok)
	}
	if err := migrate.OptIn(testApp.stateDir); err != nil {
		t.Fatal(err)
	}
	if r, found := routingRow(); found || r.Label != "" {
		t.Fatalf("doctor still offers routing by teams once opted in: %+v", r)
	}
}

// With migrateAutomatically on, the same relay start migrates the machine.
func TestARelayStartMigratesAutomaticallyWhenTheConstantSaysSo(t *testing.T) {
	_, m := notOptedIn(t)
	m.Automatic = true
	if out, err := migrate.Run(context.Background(), *m); err != nil || out != migrate.Committed {
		t.Fatalf("migrate = %s, %v", out, err)
	}
}

// `terma teams` opts the machine in, so the relay it starts migrates it.
func TestTermaTeamsOptsTheMachineIn(t *testing.T) {
	_, m := notOptedIn(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	writeRelayToken(t)
	prevSpawn := testApp.spawnRelay
	t.Cleanup(func() { testApp.spawnRelay = prevSpawn })
	started := 0
	testApp.spawnRelay = func(string, string, string) {
		started++
		if out, err := migrate.Run(context.Background(), *m); err != nil || out != migrate.Committed {
			t.Errorf("the relay `terma teams` started: migrate = %s, %v", out, err)
		}
	}
	if _, out, err := teamsJSON(t); err != nil {
		t.Fatalf("teams: %v\n%s", err, out)
	}
	if started != 1 || !config.Migrated(testApp.dir) || !migrate.OptedIn(testApp.stateDir) {
		t.Fatalf("relay starts %d, migrated %v", started, config.Migrated(testApp.dir))
	}
}

// `terma setup` on a machine never set up before opts it in, before it starts the relay,
// so the relay migrates it; setup on a machine set up before leaves that to `terma teams`.
func TestOnlyAFreshSetupOptsTheMachineIn(t *testing.T) {
	for _, c := range []struct {
		name  string
		fresh bool
	}{{"fresh", true}, {"set up before", false}} {
		t.Run(c.name, func(t *testing.T) {
			gateway := newFakeAuth(t)
			authSandbox(t, gateway)
			sandboxMachine(t)
			t.Setenv("CODEX_HOME", t.TempDir())
			gateway.orgs = []organization{{ID: orgA().ID, Name: orgA().Name, Type: "shared"}}
			if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
				t.Fatal(err)
			}
			if !c.fresh {
				if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
					p.OrganizationID, p.Harnesses = orgA().ID, []string{"codex"}
				}); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := runTerma(t, "setup", "--harness", "codex"); err != nil {
				t.Fatalf("setup: %v\n%s", err, out)
			}
			if migrate.OptedIn(testApp.stateDir) != c.fresh {
				t.Fatalf("opted in %v, want %v", migrate.OptedIn(testApp.stateDir), c.fresh)
			}
			m := testApp.relayMigration("")
			m.FlushWait, m.Automatic = time.Second, false
			want := migrate.Committed
			if !c.fresh {
				want = migrate.NotOptedIn
			}
			if out, err := migrate.Run(context.Background(), *m); err != nil || out != want {
				t.Fatalf("the relay's start after setup: migrate = %s, %v; want %s", out, err, want)
			}
		})
	}
}
