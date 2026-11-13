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

// Prune drops a ledger idle for longer than Retention, with its lock and the alias files
// still naming it, and keeps everything younger, an alias re-pointed elsewhere, and a
// ledger whose lock is held.
func TestPrune(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runs := filepath.Join(dir, Dir)
	long := time.Now().Add(-Retention - 24*time.Hour)
	start(t, dir, "old", long)
	must(t, Alias(dir, "old", "agent-old"))
	must(t, Alias(dir, "old", "agent-moved"))
	start(t, dir, "young", t0)
	must(t, Alias(dir, "young", "agent-moved"))
	start(t, dir, "held", long)
	for _, f := range []string{"old.json", "held.json", "old.json.lock", "held.json.lock"} {
		age(t, filepath.Join(runs, f), Retention+time.Hour)
	}
	age(t, filepath.Join(runs, "young.json"), Retention-time.Hour)
	unlock, err := flock.TryLock(filepath.Join(runs, "held.json.lock"))
	must(t, err)
	defer unlock()

	Prune(dir, time.Now(), nil)
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
	Prune(dir, time.Now(), nil)
	for _, p := range []string{"aliases/agent-1", "lost.json.lock", ".tmp-123"} {
		if !gone(filepath.Join(runs, p)) {
			t.Errorf("%s survived the prune", p)
		}
	}
	Prune(t.TempDir(), time.Now(), nil)
}

// A ledger goes by its last activity, a run starting, crossing or recording a location,
// never by its file's age alone, and never while a queued record names its session or a
// subagent sharing its runs: a Mixed run's records still queued would be sent without it.
func TestPruneKeepsWhatIsActiveOrQueued(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runs := filepath.Join(dir, Dir)
	now := time.Now()
	long := now.Add(-Retention - 24*time.Hour)
	for _, s := range []string{"idle", "queued", "queued-by-alias", "crossed-lately", "touched-lately"} {
		start(t, dir, s, long)
	}
	must(t, Alias(dir, "queued-by-alias", "agent-q"))
	must(t, MarkMixed(dir, "queued", long))
	must(t, MarkMixed(dir, "crossed-lately", now.Add(-time.Hour)))
	touch(t, dir, "touched-lately", "", []string{"github.com/acme/api"})
	// Every file looks long untouched: what keeps one is what it records.
	for _, s := range []string{"idle", "queued", "queued-by-alias", "crossed-lately", "touched-lately"} {
		age(t, filepath.Join(runs, s+".json"), Retention+time.Hour)
	}

	Prune(dir, now, map[string]bool{"queued": true, "agent-q": true})
	if !gone(filepath.Join(runs, "idle.json")) {
		t.Error("an idle ledger nothing queued names survived")
	}
	for _, s := range []string{"queued", "queued-by-alias", "crossed-lately", "touched-lately"} {
		if gone(filepath.Join(runs, s+".json")) {
			t.Errorf("%s was pruned", s)
		}
	}
	if _, mixed := MixedAt(dir, "queued", now); !mixed {
		t.Fatal("a queued record's run no longer reads as Mixed")
	}
	// Once its records have left the queues, it goes too, after MixedRetention if its run
	// is Mixed.
	Prune(dir, now, nil)
	if !gone(filepath.Join(runs, "queued-by-alias.json")) {
		t.Error("queued-by-alias survived once nothing queued named it")
	}
	if gone(filepath.Join(runs, "queued.json")) {
		t.Error("a Mixed run's ledger went once nothing queued named it")
	}
	Prune(dir, long.Add(MixedRetention+time.Hour), nil)
	if !gone(filepath.Join(runs, "queued.json")) {
		t.Error("a Mixed run's ledger survived MixedRetention")
	}
}

// A ledger whose current run is Mixed is kept for MixedRetention after its last activity,
// so a session that goes on after weeks idle without a new run still reads Mixed. One whose
// earlier run alone was Mixed goes after Retention, as any other.
func TestPruneKeepsAMixedRunLonger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runs := filepath.Join(dir, Dir)
	now := time.Now()
	long := now.Add(-Retention - 24*time.Hour)
	for _, s := range []string{"mixed", "mixed-before"} {
		start(t, dir, s, long.Add(-time.Hour))
		must(t, MarkMixed(dir, s, long.Add(-time.Hour)))
	}
	start(t, dir, "mixed-before", long)
	for _, s := range []string{"mixed", "mixed-before"} {
		age(t, filepath.Join(runs, s+".json"), Retention+24*time.Hour)
	}
	Prune(dir, now, nil)
	if gone(filepath.Join(runs, "mixed.json")) {
		t.Fatal("a Mixed run's ledger went after Retention")
	}
	if _, mixed := MixedAt(dir, "mixed", time.Time{}); !mixed {
		t.Fatal("the kept ledger does not read Mixed")
	}
	if !gone(filepath.Join(runs, "mixed-before.json")) {
		t.Error("a ledger whose current run is not Mixed survived Retention")
	}
	Prune(dir, long.Add(MixedRetention+time.Hour), nil)
	if !gone(filepath.Join(runs, "mixed.json")) {
		t.Error("a Mixed run's ledger survived MixedRetention")
	}
}

// Refresh renews a ledger's activity only once it is over RefreshEvery old, so Prune keeps
// a session that goes on recording nothing new; it makes no ledger for a session with none.
func TestRefreshRenewsAnOldLedgerOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	start := time.Now()
	if _, err := Start(dir, "sess", start); err != nil {
		t.Fatal(err)
	}
	must(t, Refresh(dir, "sess", start.Add(RefreshEvery)))
	if p, _ := Locate(dir, "sess"); !p.Active.Equal(start.UTC()) {
		t.Fatalf("renewed a ledger a day old or less: %v", p.Active)
	}
	later := start.Add(RefreshEvery + time.Minute)
	must(t, Refresh(dir, "sess", later))
	if p, _ := Locate(dir, "sess"); !p.Active.Equal(later.UTC()) {
		t.Fatalf("activity %v, want %v", p.Active, later.UTC())
	}
	Prune(dir, start.Add(Retention+time.Hour), nil)
	if gone(filepath.Join(dir, Dir, "sess.json")) {
		t.Fatal("pruned a renewed ledger")
	}
	must(t, Refresh(dir, "none", later))
	if !gone(filepath.Join(dir, Dir, "none.json")) {
		t.Fatal("Refresh made a ledger")
	}
}
