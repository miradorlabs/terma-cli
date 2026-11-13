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
// one profile signed in with its team's server key, whose key is in keys.json.
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
	return configDir, stateDir
}

// A relay asked to migrate does so at start, under its lock, before it listens.
func TestARelayMigratesBeforeItListens(t *testing.T) {
	t.Parallel()
	configDir, stateDir := legacyMachine(t)
	c := runConfig(stateDir, time.Millisecond, func(net.Addr, time.Duration) {
		s, ok, err := routing.Load(stateDir, "default")
		if !config.Migrated(configDir) || !ok || err != nil || s.Migration.Phase != routing.Committed {
			t.Errorf("listening on a machine not migrated: %+v, %v", s, err)
		}
	})
	c.Migration = &migrate.Machine{ConfigDir: configDir, StateDir: stateDir, FlushWait: time.Second}
	run(t, c)
	if migrate.Needed(configDir, stateDir) {
		t.Fatal("the relay left the migration to do")
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

// A migration that stops part way, once the machine is marked migrated, keeps the relay
// from running: the machine collects nothing until a start finishes it, and the next
// start does.
func TestARelayDoesNotRunOnAMigrationLeftPartWay(t *testing.T) {
	t.Parallel()
	configDir, stateDir := legacyMachine(t)
	listened := false
	c := runConfig(stateDir, time.Millisecond, func(net.Addr, time.Duration) { listened = true })
	c.Migration = &migrate.Machine{ConfigDir: configDir, StateDir: stateDir, FlushWait: time.Second}
	// The profile has no sign-in to copy, so the copy marks the machine migrated; the
	// locked keychain then keeps its team's key from being copied.
	unlock := secret.FailForTest(t, configDir)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := Run(ctx, c); err == nil || listened {
		t.Fatalf("Run = %v, listened %v", err, listened)
	}
	if !config.Migrated(configDir) || !migrate.Needed(configDir, stateDir) {
		t.Fatal("the test did not leave the migration part way")
	}
	unlock()
	run(t, c)
	if !listened || migrate.Needed(configDir, stateDir) {
		t.Fatalf("the next start did not finish: listened %v", listened)
	}
}
