//go:build unix

package hookrun

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Lock files exist only where lockEvidence takes a real flock, which is why this is a
// Unix test. Before the fix every finished session left its `.json.lock` behind: the
// prune matched `.json` alone.
func TestPruneRetiresOrphanedLocks(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-72 * time.Hour)
	cutoff := now.Add(-48 * time.Hour)

	touch := func(name string, at time.Time) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// A finished session: both halves are old, and both go in one pass.
	touch("finished.json", old)
	touch("finished.json.lock", old)
	// What earlier versions left behind: a lock whose data file is long gone.
	touch("orphan.json.lock", old)
	// A session older than the cutoff that is still writing: its lock is old by
	// creation, and its data file says it is alive.
	touch("alive.json", now)
	touch("alive.json.lock", old)
	// A session that has taken its lock and not written yet.
	touch("starting.json.lock", now)
	// An orphan another invocation holds right now.
	held := touch("held.json.lock", old)
	unlock, err := lockEvidence(held)
	if err != nil {
		t.Fatal(err)
	}
	// Taking the lock must not have refreshed it, or the case proves nothing.
	if err := os.Chtimes(held, old, old); err != nil {
		t.Fatal(err)
	}

	pruneQuotaState(dir, cutoff)
	unlock()

	for name, want := range map[string]bool{
		"finished.json":      false,
		"finished.json.lock": false,
		"orphan.json.lock":   false,
		"alive.json":         true,
		"alive.json.lock":    true,
		"starting.json.lock": true,
		"held.json.lock":     true,
	} {
		_, err := os.Lstat(filepath.Join(dir, name))
		if got := err == nil; got != want {
			t.Errorf("%s: present = %v, want %v", name, got, want)
		}
	}
}
