package runs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
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

	Prune(context.Background(), dir, time.Now(), nil)
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
	must(t, MarkMixed(dir, "young", t0, crossing))
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
	Prune(context.Background(), dir, time.Now(), nil)
	for _, p := range []string{"aliases/agent-1", "lost.json.lock", ".tmp-123"} {
		if !gone(filepath.Join(runs, p)) {
			t.Errorf("%s survived the prune", p)
		}
	}
	Prune(context.Background(), t.TempDir(), time.Now(), nil)
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
	must(t, MarkMixed(dir, "queued", long, crossing))
	must(t, MarkMixed(dir, "crossed-lately", now.Add(-time.Hour), crossing))
	touch(t, dir, "touched-lately", "", []string{"github.com/acme/api"})
	// Every file looks long untouched: what keeps one is what it records.
	for _, s := range []string{"idle", "queued", "queued-by-alias", "crossed-lately", "touched-lately"} {
		age(t, filepath.Join(runs, s+".json"), Retention+time.Hour)
	}

	Prune(context.Background(), dir, now, map[string]bool{"queued": true, "agent-q": true})
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
	Prune(context.Background(), dir, now, nil)
	if !gone(filepath.Join(runs, "queued-by-alias.json")) {
		t.Error("queued-by-alias survived once nothing queued named it")
	}
	if gone(filepath.Join(runs, "queued.json")) {
		t.Error("a Mixed run's ledger went once nothing queued named it")
	}
	Prune(context.Background(), dir, long.Add(MixedRetention+time.Hour), nil)
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
		must(t, MarkMixed(dir, s, long.Add(-time.Hour), crossing))
	}
	start(t, dir, "mixed-before", long)
	for _, s := range []string{"mixed", "mixed-before"} {
		age(t, filepath.Join(runs, s+".json"), Retention+24*time.Hour)
	}
	Prune(context.Background(), dir, now, nil)
	if gone(filepath.Join(runs, "mixed.json")) {
		t.Fatal("a Mixed run's ledger went after Retention")
	}
	if _, mixed := MixedAt(dir, "mixed", time.Time{}); !mixed {
		t.Fatal("the kept ledger does not read Mixed")
	}
	if !gone(filepath.Join(runs, "mixed-before.json")) {
		t.Error("a ledger whose current run is not Mixed survived Retention")
	}
	Prune(context.Background(), dir, long.Add(MixedRetention+time.Hour), nil)
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
	Prune(context.Background(), dir, start.Add(Retention+time.Hour), nil)
	if gone(filepath.Join(dir, Dir, "sess.json")) {
		t.Fatal("pruned a renewed ledger")
	}
	must(t, Refresh(dir, "none", later))
	if !gone(filepath.Join(dir, Dir, "none.json")) {
		t.Fatal("Refresh made a ledger")
	}
}

// A prune stops once its context ends, as the relay stops: it removes nothing more.
func TestPruneStopsWithItsContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	start(t, dir, "old", time.Now().Add(-Retention-24*time.Hour))
	age(t, filepath.Join(dir, Dir, "old.json"), Retention+time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	Prune(ctx, dir, time.Now(), nil)
	if gone(filepath.Join(dir, Dir, "old.json")) {
		t.Fatal("a prune whose context had ended removed a ledger")
	}
}

// An unreadable ledger or corrupt sidecar fails closed: Prune keeps it while a queued
// record names the session and for MixedRetention after its file time, because it may
// record a crossing.
func TestPruneFailsClosedOnUnreadableLedgerOrSidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runsDir := filepath.Join(dir, Dir)
	must(t, os.MkdirAll(runsDir, 0o700))

	// Corrupt ledger JSON with a subagent alias pointing at it.
	corruptLedger := filepath.Join(runsDir, "corrupt.json")
	must(t, os.WriteFile(corruptLedger, []byte("{not-json"), 0o600))
	age(t, corruptLedger, Retention+24*time.Hour)
	must(t, os.MkdirAll(filepath.Dir(aliasPath(dir, "sub-corrupt")), 0o700))
	must(t, os.WriteFile(aliasPath(dir, "sub-corrupt"), []byte("corrupt"), 0o600))
	age(t, aliasPath(dir, "sub-corrupt"), Retention+24*time.Hour)

	// Valid ledger with a subagent alias and a corrupt mark file in its sidecar.
	long := time.Now().Add(-Retention - 24*time.Hour)
	start(t, dir, "badside", long)
	must(t, Alias(dir, "badside", "sub-badside"))
	badSideDir := filepath.Join(runsDir, "badside"+asideExt)
	must(t, os.MkdirAll(badSideDir, 0o700))
	must(t, os.WriteFile(filepath.Join(badSideDir, "m1"), []byte("{not-json"), 0o600))
	age(t, filepath.Join(runsDir, "badside.json"), Retention+24*time.Hour)

	// Past Retention but within MixedRetention: both stay even when not queued.
	Prune(context.Background(), dir, time.Now(), nil)
	if gone(corruptLedger) {
		t.Fatal("corrupt ledger was pruned before MixedRetention")
	}
	if gone(filepath.Join(runsDir, "badside.json")) {
		t.Fatal("ledger with corrupt sidecar was pruned before MixedRetention")
	}

	// Past MixedRetention but queued via subagent alias: both ledgers and aliases stay.
	future := time.Now().Add(MixedRetention + 24*time.Hour)
	Prune(context.Background(), dir, future, map[string]bool{"sub-corrupt": true, "sub-badside": true})
	if gone(corruptLedger) || gone(aliasPath(dir, "sub-corrupt")) {
		t.Fatal("alias-queued corrupt ledger or its alias was pruned")
	}
	if gone(filepath.Join(runsDir, "badside.json")) {
		t.Fatal("alias-queued ledger with corrupt sidecar was pruned")
	}

	// Past MixedRetention and no longer queued: both (and their aliases) are pruned.
	Prune(context.Background(), dir, future, nil)
	if !gone(corruptLedger) || !gone(aliasPath(dir, "sub-corrupt")) {
		t.Fatal("unreferenced corrupt ledger or its alias survived MixedRetention")
	}
	if !gone(filepath.Join(runsDir, "badside.json")) {
		t.Fatal("unreferenced ledger with corrupt sidecar survived MixedRetention")
	}
	if !gone(aliasPath(dir, "sub-badside")) {
		t.Fatal("alias of pruned corrupt-sidecar ledger survived")
	}
}

// Alias deletion in Prune must serialize with Alias reassignment under alias.json.lock so
// pruning an expired parent never deletes a replacement pointer to a new Mixed parent.
func TestPruneAliasDeletionSerializedWithReassignment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runsDir := filepath.Join(dir, Dir)
	long := time.Now().Add(-Retention - 24*time.Hour)

	// 1. Deterministic lock hold: hold agent-lock.json.lock (as Alias(new, agent-lock) does)
	//    while Prune removes expired old.json; the alias pointer must survive while locked.
	start(t, dir, "old-det", long)
	must(t, Alias(dir, "old-det", "agent-lock"))
	age(t, filepath.Join(runsDir, "old-det.json"), Retention+2*time.Hour)
	age(t, aliasPath(dir, "agent-lock"), Retention+2*time.Hour)
	unlock, err := flock.TryLock(filepath.Join(runsDir, "agent-lock.json.lock"))
	must(t, err)
	Prune(context.Background(), dir, time.Now(), nil)
	if !gone(filepath.Join(runsDir, "old-det.json")) {
		unlock()
		t.Fatal("old-det.json was not pruned")
	}
	if gone(aliasPath(dir, "agent-lock")) {
		unlock()
		t.Fatal("Prune deleted aliases/agent-lock while agent-lock.json.lock was held")
	}
	unlock()

	// 2. Concurrent stress: Prune expired old-i while Alias(new-i, agent-i) reassigns and marks new-i Mixed.
	cross := time.Now().UTC()
	for i := range 16 {
		oldID, newID, agID := fmt.Sprintf("pold-%d", i), fmt.Sprintf("pnew-%d", i), fmt.Sprintf("pag-%d", i)
		start(t, dir, oldID, long)
		start(t, dir, newID, cross)
		must(t, Alias(dir, oldID, agID))
		must(t, MarkMixed(dir, newID, cross, Why{Reason: Crossed, Where: "new"}))
		age(t, filepath.Join(runsDir, oldID+".json"), Retention+2*time.Hour)
		age(t, aliasPath(dir, agID), Retention+2*time.Hour)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			Prune(context.Background(), dir, time.Now(), nil)
		}()
		go func() {
			defer wg.Done()
			if err := Alias(dir, newID, agID); err != nil {
				t.Errorf("Alias(%s, %s): %v", newID, agID, err)
			}
		}()
		wg.Wait()
		if gone(aliasPath(dir, agID)) {
			t.Fatalf("stress %d: Prune deleted reassigned alias %s", i, agID)
		}
		if _, mixed := MixedAt(dir, agID, cross.Add(time.Second)); !mixed {
			t.Fatalf("stress %d: reassigned alias %s lost new parent's Mixed state", i, agID)
		}
	}
}
