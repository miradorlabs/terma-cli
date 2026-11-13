package migrate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// A machine whose developer has not opted in is left as it was: Run asks no organization
// type, takes no lock and records nothing, and every reader goes on with today's rules.
func TestRunLeavesAMachineNotOptedIn(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	must(t, os.Remove(filepath.Join(m.StateDir, OptInFile)))
	m.setUp(t, acme)
	m.OrgType = func(context.Context, string, string) (string, error) {
		t.Error("asked an organization type on a machine not opted in")
		return OrgShared, nil
	}
	before := snapshot(t, m)
	if out, err := Run(context.Background(), m); err != nil || out != NotOptedIn {
		t.Fatalf("Run = %s, %v", out, err)
	}
	if after := snapshot(t, m); !mapsEqual(before, after) || config.Migrated(m.ConfigDir) {
		t.Fatal("Run changed a machine not opted in")
	}
	if _, ok := ReadBlock(m.StateDir); ok {
		t.Fatal("Run recorded a block on a machine not opted in")
	}
	if _, migrating := Migrating(m.StateDir); migrating {
		t.Fatal("Run recorded that it migrates a machine not opted in")
	}

	// Opting in moves it at the next Run.
	must(t, OptIn(m.StateDir))
	m.OrgType = orgsOfType(nil)
	m.commit(t)
	m.migrated(t, acme)
}

// Machine.Automatic moves a machine whose developer has not opted in.
func TestRunMigratesAutomaticallyWithoutOptIn(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	must(t, os.Remove(filepath.Join(m.StateDir, OptInFile)))
	m.setUp(t, acme)
	m.Automatic = true
	m.commit(t)
	m.migrated(t, acme)
}

// A migration left part way is finished whatever was chosen: until it is, the machine
// collects nothing.
func TestRunFinishesAMigrationLeftPartWayWithoutOptIn(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.setUp(t, acme)
	m.fault = crashAt("commit/default")
	if out, _ := Run(context.Background(), m); out != Unfinished {
		t.Fatalf("Run = %s, want it left part way", out)
	}
	must(t, os.Remove(filepath.Join(m.StateDir, OptInFile)))
	m.fault = nil
	m.commit(t)
	m.migrated(t, acme)
}
