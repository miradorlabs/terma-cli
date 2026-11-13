package runs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

func age(t *testing.T, path string, by time.Duration) {
	t.Helper()
	old := time.Now().Add(-by)
	must(t, os.Chtimes(path, old, old))
}

func gone(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// Prune drops a ledger untouched for longer than Retention, with its lock and the alias
// files still naming it, and keeps everything younger, an alias re-pointed elsewhere, and
// a ledger whose lock is held.
func TestPrune(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runs := filepath.Join(dir, Dir)
	start(t, dir, "old", t0)
	must(t, Alias(dir, "old", "agent-old"))
	must(t, Alias(dir, "old", "agent-moved"))
	start(t, dir, "young", t0)
	must(t, Alias(dir, "young", "agent-moved"))
	start(t, dir, "held", t0)
	for _, f := range []string{"old.json", "held.json", "old.json.lock", "held.json.lock"} {
		age(t, filepath.Join(runs, f), Retention+time.Hour)
	}
	age(t, filepath.Join(runs, "young.json"), Retention-time.Hour)
	unlock, err := flock.TryLock(filepath.Join(runs, "held.json.lock"))
	must(t, err)
	defer unlock()

	Prune(dir, time.Now())
	for _, p := range []string{"old.json", "old.json.lock", "aliases/agent-old"} {
		if !gone(filepath.Join(runs, p)) {
			t.Errorf("%s survived the prune", p)
		}
	}
	for _, p := range []string{"young.json", "held.json", "aliases/agent-moved"} {
		if gone(filepath.Join(runs, p)) {
			t.Errorf("%s was pruned", p)
		}
	}
	if _, mixed := MixedAt(dir, "agent-moved", t0); mixed {
		t.Fatal("the re-pointed alias reads as Mixed")
	}
	must(t, MarkMixed(dir, "young", t0))
	if _, mixed := MixedAt(dir, "agent-moved", t0); !mixed {
		t.Fatal("the re-pointed alias no longer reaches its session")
	}
}

// What a crash or a lost ledger left behind goes too: an orphaned alias, a lock without
// its ledger, a temporary file.
func TestPruneLeftovers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runs := filepath.Join(dir, Dir)
	start(t, dir, "lost", t0)
	must(t, Alias(dir, "lost", "agent-1"))
	must(t, os.Remove(filepath.Join(runs, "lost.json")))
	must(t, os.WriteFile(filepath.Join(runs, ".tmp-123"), nil, 0o600))
	for _, p := range []string{"aliases/agent-1", "lost.json.lock", ".tmp-123"} {
		age(t, filepath.Join(runs, p), Retention+time.Hour)
	}
	Prune(dir, time.Now())
	for _, p := range []string{"aliases/agent-1", "lost.json.lock", ".tmp-123"} {
		if !gone(filepath.Join(runs, p)) {
			t.Errorf("%s survived the prune", p)
		}
	}
	Prune(t.TempDir(), time.Now())
}
