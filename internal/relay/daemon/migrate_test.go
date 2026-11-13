package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// legacyMachine is a relay's state directory on a machine before multi-org routing, with
// one profile signed in with its team's server key, whose key is in keys.json, and whose
// developer opted in to routing by teams (migrate.OptIn).
func legacyMachine(t *testing.T) (configDir, stateDir string) {
	t.Helper()
	stateDir, _, _ = setUpRelay(t)
	configDir = t.TempDir()
	if err := config.UpdateProfile(configDir, "default", func(p *config.Profile) {
		p.OrganizationID, p.Team, p.ServerKeySignIn = "acme", "acme-platform", true
	}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.Set(configDir, "acme-platform", "ter_srv_acme", keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	if err := migrate.OptIn(stateDir); err != nil {
		t.Fatal(err)
	}
	return configDir, stateDir
}

// A relay start on a machine whose developer has not opted in leaves it as it was, and
// does not wait on it: it asks nothing, records nothing, and is idle at once.
func TestARelayStartLeavesAMachineNotOptedIn(t *testing.T) {
	t.Parallel()
	configDir, stateDir := legacyMachine(t)
	if err := os.Remove(filepath.Join(stateDir, migrate.OptInFile)); err != nil {
		t.Fatal(err)
	}
	c := runConfig(stateDir, time.Millisecond, nil)
	c.Migration = &migrate.Machine{ConfigDir: configDir, StateDir: stateDir, FlushWait: time.Second,
		OrgType: func(context.Context, string, string) (string, error) {
			t.Error("asked an organization type on a machine not opted in")
			return migrate.OrgShared, nil
		}}
	run(t, c)
	if config.Migrated(configDir) {
		t.Fatal("a relay migrated a machine not opted in")
	}
	if _, err := os.Stat(filepath.Join(stateDir, routing.StatesDir)); !os.IsNotExist(err) {
		t.Fatalf("a relay prepared routing states on a machine not opted in: %v", err)
	}
	if _, ok := migrate.ReadBlock(stateDir); ok {
		t.Fatal("a relay recorded why it did not migrate a machine not opted in")
	}
}

// With migration automatic (the command line's migrateAutomatically), a relay start
// migrates a machine whose developer has not opted in.
func TestARelayStartMigratesAutomatically(t *testing.T) {
	t.Parallel()
	configDir, stateDir := legacyMachine(t)
	if err := os.Remove(filepath.Join(stateDir, migrate.OptInFile)); err != nil {
		t.Fatal(err)
	}
	c := runConfig(stateDir, time.Millisecond, nil)
	c.Migration = &migrate.Machine{ConfigDir: configDir, StateDir: stateDir, FlushWait: time.Second, Automatic: true}
	run(t, c)
	if migrate.Needed(configDir, stateDir) {
		t.Fatal("an automatic relay start did not migrate")
	}
}

// A relay asked to migrate listens first, under today's rules, then migrates beside
// itself, under its lock, and is not idle until it has.
func TestARelayListensThenMigrates(t *testing.T) {
	t.Parallel()
	configDir, stateDir := legacyMachine(t)
	c := runConfig(stateDir, time.Millisecond, func(net.Addr, time.Duration) {
		if config.Migrated(configDir) {
			t.Error("the relay migrated before it listened")
		}
	})
	c.Migration = &migrate.Machine{ConfigDir: configDir, StateDir: stateDir, FlushWait: time.Second}
	run(t, c)
	s, ok, err := routing.Load(stateDir, "default")
	if migrate.Needed(configDir, stateDir) || !ok || err != nil || s.Migration.Phase != routing.Committed {
		t.Fatalf("the relay left the migration to do: %+v, %v", s, err)
	}
	if _, migrating := migrate.Migrating(stateDir); migrating {
		t.Fatal("the relay left its record that it is migrating")
	}
}

// Asking each team's organization type never delays the relay's start: it listens while
// it asks, records that it is migrating meanwhile, and migrates once it knows.
func TestARelayListensWhileItAsksTheOrganizationTypes(t *testing.T) {
	t.Parallel()
	configDir, stateDir := legacyMachine(t)
	if err := config.UpdateProfile(configDir, "default", func(p *config.Profile) { p.ServerKeySignIn = false }); err != nil {
		t.Fatal(err)
	}
	listening, answer := make(chan struct{}), make(chan struct{})
	c := runConfig(stateDir, time.Millisecond, func(net.Addr, time.Duration) { close(listening) })
	c.Migration = &migrate.Machine{ConfigDir: configDir, StateDir: stateDir, FlushWait: time.Second,
		OrgType: func(ctx context.Context, _, _ string) (string, error) {
			select {
			case <-answer:
				return migrate.OrgShared, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}}
	done := make(chan Result, 1)
	go func() { done <- run(t, c) }()
	select {
	case <-listening:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay did not listen while the organization types were asked")
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, migrating := migrate.Migrating(stateDir); migrating {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the relay did not record that it is migrating")
		}
	}
	if config.Migrated(configDir) {
		t.Fatal("migrated before the organization types were known")
	}
	close(answer)
	<-done
	if migrate.Needed(configDir, stateDir) {
		t.Fatal("the relay did not migrate once it knew the organization types")
	}
}

// A relay not asked to migrate leaves the machine as it was.
func TestARelayNotAskedToMigrateDoesNot(t *testing.T) {
	t.Parallel()
	configDir, stateDir := legacyMachine(t)
	run(t, runConfig(stateDir, time.Millisecond, nil))
	if config.Migrated(configDir) {
		t.Fatal("a relay migrated the machine unasked")
	}
	if _, err := os.Stat(filepath.Join(stateDir, routing.StatesDir)); !os.IsNotExist(err) {
		t.Fatalf("a relay prepared routing states unasked: %v", err)
	}
}

// A relay that may not migrate, here because the agents' hooks run another terma, runs
// as before on a machine left as it was.
func TestARelayThatMayNotMigrateRunsAsBefore(t *testing.T) {
	t.Parallel()
	configDir, stateDir := legacyMachine(t)
	exe, other := filepath.Join(t.TempDir(), "terma"), filepath.Join(t.TempDir(), "terma")
	for path, build := range map[string]string{exe: "new", other: "old"} {
		if err := os.WriteFile(path, []byte(build), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var listened bool
	c := runConfig(stateDir, time.Millisecond, func(net.Addr, time.Duration) { listened = true })
	c.Migration = &migrate.Machine{ConfigDir: configDir, StateDir: stateDir, Executable: exe, FlushWait: time.Second,
		Hooked: func() ([]string, error) { return []string{other}, nil }}
	run(t, c)
	if !listened || config.Migrated(configDir) {
		t.Fatalf("listened %v, migrated %v", listened, config.Migrated(configDir))
	}
	if b, ok := migrate.ReadBlock(stateDir); !ok || b.Reason != migrate.TwoInstalls {
		t.Fatalf("doctor's record = %+v, %v", b, ok)
	}
}

// A migration that stops part way, once the machine is marked migrated, stops the relay
// with its error: the machine collects nothing until a start finishes it, and the next
// start does.
func TestARelayStopsOnAMigrationLeftPartWay(t *testing.T) {
	t.Parallel()
	configDir, stateDir := legacyMachine(t)
	listened := false
	c := runConfig(stateDir, 0, func(net.Addr, time.Duration) { listened = true })
	c.Migration = &migrate.Machine{ConfigDir: configDir, StateDir: stateDir, FlushWait: time.Second}
	// The profile has no sign-in to copy, so the copy marks the machine migrated; the
	// locked keychain then keeps its team's key from being copied.
	unlock := secret.FailForTest(t, configDir)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := Run(ctx, c); err == nil || ctx.Err() != nil {
		t.Fatalf("Run = %v, the test's deadline %v", err, ctx.Err())
	}
	listened = false
	c.Idle = time.Millisecond
	if !config.Migrated(configDir) || !migrate.Needed(configDir, stateDir) {
		t.Fatal("the test did not leave the migration part way")
	}
	// Finished whatever was chosen: the machine collects nothing until it is.
	if err := os.Remove(filepath.Join(stateDir, migrate.OptInFile)); err != nil {
		t.Fatal(err)
	}
	unlock()
	run(t, c)
	if !listened || migrate.Needed(configDir, stateDir) {
		t.Fatalf("the next start did not finish: listened %v", listened)
	}
}
