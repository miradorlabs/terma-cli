//go:build unix

package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

// Concurrent touches of one manifest keep every file; flock excludes per open file, so
// goroutines contend exactly as processes do.
func TestConcurrentTouchesKeepEveryFile(t *testing.T) {
	store := patient(Open(t.TempDir()))
	sess := Session{ID: "sess-1", Tool: "codex"}
	const writers = 24

	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			if err := store.Touch(sess, []string{fmt.Sprintf("file-%02d.go", i)}, time.Now()); err != nil {
				t.Errorf("touch %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	manifests, err := store.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 {
		t.Fatalf("manifests = %d, want 1", len(manifests))
	}
	if got := len(manifests[0].Files); got != writers {
		t.Fatalf("the manifest kept %d of %d files", got, writers)
	}
}

// Reading a repository's state does not create it, lock file included.
func TestConsumeAndPruneCreateNothing(t *testing.T) {
	gitDir := t.TempDir()
	store := Open(gitDir)
	if err := store.Consume("sess-1", []string{"a.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, stateDirName)); !os.IsNotExist(err) {
		t.Fatalf("the state directory was created by a read path: %v", err)
	}
}

// A SetActive racing Touch's refresh of the active session is never overwritten.
func TestANewSessionSurvivesTouchesOfTheOldOne(t *testing.T) {
	for round := range 40 {
		store := patient(Open(t.TempDir()))
		older := Session{ID: "sess-old", Tool: "codex"}
		if err := store.SetActive(older); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := range 12 {
			wg.Go(func() {
				_ = store.Touch(older, []string{fmt.Sprintf("f-%d.go", i)}, time.Now())
			})
		}
		wg.Go(func() {
			if err := store.SetActive(Session{ID: "sess-new", Tool: "claude-code"}); err != nil {
				t.Errorf("set active: %v", err)
			}
		})
		wg.Wait()
		// A Touch of the old session never brings it back.
		if active, _ := store.Active(time.Now(), 0); active == nil || active.ID != "sess-new" {
			t.Fatalf("round %d: the active session is %+v, want the newly announced one", round, active)
		}
	}
}

// Prune retires a stale active session without waiting on its own lock.
func TestPruneDoesNotWaitOnItsOwnLock(t *testing.T) {
	store := Open(t.TempDir())
	long := time.Now().Add(-30 * 24 * time.Hour)
	if err := store.SetActive(Session{ID: "sess-stale", Tool: "codex", StartedAt: long, UpdatedAt: long}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := store.Prune(time.Now().Add(-24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > hookLockWait/2 {
		t.Fatalf("prune took %s: it waited on a lock it already held", elapsed)
	}
	if active, _ := store.Active(time.Now(), 0); active != nil {
		t.Fatalf("the stale active session survived the prune: %+v", active)
	}
}

// patient gives a store a wait no test will outlast, so tests of the lock's exclusion do
// not measure the machine.
func patient(s *Store) *Store {
	s.lockWait = time.Minute
	return s
}

// A lock that cannot be had in time costs the hook its exclusivity, never its write.
func TestTouchGoesAheadWhenTheLockCannotBeHad(t *testing.T) {
	store := Open(t.TempDir())
	store.lockWait = 40 * time.Millisecond
	if err := os.MkdirAll(store.dir, dirMode); err != nil {
		t.Fatal(err)
	}
	release, err := flock.Lock(context.Background(), filepath.Join(store.dir, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	start := time.Now()
	if err := store.Touch(Session{ID: "sess-1", Tool: "codex"}, []string{"a.go"}, time.Now()); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("touch waited %s on a lock it was never going to get", elapsed)
	}
	manifests, err := store.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 || len(manifests[0].Files) != 1 {
		t.Fatalf("the write was lost with the lock: %+v", manifests)
	}
}

// Merges racing the parent's own touches keep every file in the parent's manifest and
// leave no child's.
func TestConcurrentMergesAndTouchesLoseNothing(t *testing.T) {
	store := patient(Open(t.TempDir()))
	parent := Session{ID: "conv-parent", Tool: "cursor"}
	const own, children = 12, 8
	now := time.Now()

	for j := range children {
		if err := store.Touch(Session{ID: fmt.Sprintf("conv-child-%d", j), Tool: "cursor"}, []string{fmt.Sprintf("child-%d.go", j)}, now); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := range own {
		wg.Go(func() {
			if err := store.Touch(parent, []string{fmt.Sprintf("own-%02d.go", i)}, now); err != nil {
				t.Errorf("touch %d: %v", i, err)
			}
		})
	}
	for j := range children {
		wg.Go(func() {
			if _, err := store.Merge(fmt.Sprintf("conv-child-%d", j), parent, now); err != nil {
				t.Errorf("merge %d: %v", j, err)
			}
		})
	}
	wg.Wait()

	manifests, err := store.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 || manifests[0].SessionID != parent.ID {
		t.Fatalf("manifests = %d, want only the parent's", len(manifests))
	}
	if got := len(manifests[0].Files); got != own+children {
		t.Fatalf("the parent's manifest kept %d of %d files", got, own+children)
	}
}

// Touches that all miss the lock keep every file, and the next locked writer folds them
// into the manifest and leaves no delta behind.
func TestContendedTouchesLoseNothing(t *testing.T) {
	store := Open(t.TempDir())
	store.lockWait = time.Millisecond
	if err := os.MkdirAll(store.dir, dirMode); err != nil {
		t.Fatal(err)
	}
	release, err := flock.Lock(context.Background(), filepath.Join(store.dir, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	sess := Session{ID: "sess-1", Tool: "codex"}
	const writers = 24
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			if err := store.Touch(sess, []string{fmt.Sprintf("file-%02d.go", i)}, time.Now()); err != nil {
				t.Errorf("touch %d: %v", i, err)
			}
		})
	}
	wg.Wait()
	if got := fileCount(t, store); got != writers {
		t.Fatalf("contended touches kept %d of %d files", got, writers)
	}

	release()
	if err := store.Touch(sess, []string{"last.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := fileCount(t, store); got != writers+1 {
		t.Fatalf("the fold kept %d of %d files", got, writers+1)
	}
	if deltas, _ := filepath.Glob(filepath.Join(store.dir, manifestsDir, "*"+deltaExt)); len(deltas) != 0 {
		t.Fatalf("the locked touch left %d deltas", len(deltas))
	}
}

// Consume and Prune see a session's deltas: a committed file is retired wherever it was
// recorded, and a stale session leaves nothing behind to revive it.
func TestConsumeAndPruneFoldDeltas(t *testing.T) {
	store := Open(t.TempDir())
	long := time.Now().Add(-30 * 24 * time.Hour)
	sess := Session{ID: "sess-1", Tool: "codex"}
	if err := store.Touch(sess, []string{"a.go"}, long); err != nil {
		t.Fatal(err)
	}
	if err := store.writeDelta(&Manifest{SessionID: sess.ID, Tool: "codex", StartedAt: long, UpdatedAt: long, Files: map[string]time.Time{"b.go": long, "c.go": long}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Consume(sess.ID, []string{"b.go"}); err != nil {
		t.Fatal(err)
	}
	manifests, err := store.Manifests()
	if err != nil || len(manifests) != 1 || len(manifests[0].Files) != 2 || !manifests[0].Files["c.go"].Equal(long) {
		t.Fatalf("after consume: %+v, %v", manifests, err)
	}
	if err := store.writeDelta(&Manifest{SessionID: sess.ID, StartedAt: long, UpdatedAt: long, Files: map[string]time.Time{"d.go": long}}); err != nil {
		t.Fatal(err)
	}
	if n, err := store.Prune(time.Now().Add(-24 * time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	if manifests, _ := store.Manifests(); len(manifests) != 0 {
		t.Fatalf("a pruned session came back: %+v", manifests)
	}
}

func fileCount(t *testing.T, store *Store) int {
	t.Helper()
	manifests, err := store.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 {
		t.Fatalf("manifests = %d, want 1", len(manifests))
	}
	return len(manifests[0].Files)
}

// Writers that get the lock and writers that miss it, racing a Consume that folds deltas
// mid-flight, keep every file: a fold removes only the deltas it read.
func TestMixedLockedAndContendedWritersLoseNothing(t *testing.T) {
	for round := range 20 {
		dir := t.TempDir()
		steady := patient(Open(dir))
		hasty := Open(dir)
		hasty.lockWait = time.Microsecond
		sess := Session{ID: "sess-1", Tool: "codex"}
		const writers = 32
		var wg sync.WaitGroup
		for i := range writers {
			store := steady
			if i%2 == 1 {
				store = hasty
			}
			wg.Go(func() {
				if err := store.Touch(sess, []string{fmt.Sprintf("file-%02d.go", i)}, time.Now()); err != nil {
					t.Errorf("touch %d: %v", i, err)
				}
			})
		}
		for range 4 {
			wg.Go(func() {
				if err := steady.Consume(sess.ID, []string{"never-touched.go"}); err != nil {
					t.Errorf("consume: %v", err)
				}
			})
		}
		wg.Wait()
		if got := fileCount(t, steady); got != writers {
			t.Fatalf("round %d: kept %d of %d files", round, got, writers)
		}
	}
}

// A merge folds both sessions' deltas into the target and leaves none of either behind.
func TestMergeFoldsDeltasOfBothSessions(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Now()
	child, parent := Session{ID: "conv-child", Tool: "cursor"}, Session{ID: "conv-parent", Tool: "cursor"}
	if err := store.Touch(child, []string{"child.go"}, now); err != nil {
		t.Fatal(err)
	}
	for _, d := range []*Manifest{
		{SessionID: child.ID, Tool: "cursor", StartedAt: now, UpdatedAt: now, Files: map[string]time.Time{"child-late.go": now}},
		{SessionID: parent.ID, Tool: "cursor", StartedAt: now, UpdatedAt: now, Files: map[string]time.Time{"parent.go": now}},
	} {
		if err := store.writeDelta(d); err != nil {
			t.Fatal(err)
		}
	}
	moved, err := store.Merge(child.ID, parent, now)
	if err != nil || len(moved) != 2 {
		t.Fatalf("merge moved %v, %v", moved, err)
	}
	manifests, err := store.Manifests()
	if err != nil || len(manifests) != 1 || manifests[0].SessionID != parent.ID {
		t.Fatalf("manifests = %+v, %v", manifests, err)
	}
	for _, f := range []string{"child.go", "child-late.go", "parent.go"} {
		if _, ok := manifests[0].Files[f]; !ok {
			t.Errorf("the merged manifest lost %s: %v", f, manifests[0].Files)
		}
	}
	if deltas, _ := filepath.Glob(filepath.Join(store.dir, manifestsDir, "*"+deltaExt)); len(deltas) != 0 {
		t.Fatalf("the merge left %d deltas", len(deltas))
	}
}

// A session whose every touch missed the lock has no manifest file, yet it claims the
// commit that carries its files, as a manifest's session would.
func TestASessionKnownOnlyByDeltasIsAttributed(t *testing.T) {
	store := Open(t.TempDir())
	store.lockWait = time.Millisecond
	if err := os.MkdirAll(store.dir, dirMode); err != nil {
		t.Fatal(err)
	}
	release, err := flock.Lock(context.Background(), filepath.Join(store.dir, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := store.Touch(Session{ID: "sess-1", Tool: "codex", ToolVersion: "0.158.0"}, []string{"a.go"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.manifestPath("sess-1")); !os.IsNotExist(err) {
		t.Fatalf("a contended touch wrote the manifest itself: %v", err)
	}
	manifests, err := store.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	got := Attribute([]string{"a.go", "b.go"}, manifests, nil, false)
	if len(got) != 1 || got[0].SessionID != "sess-1" || got[0].Tool != "codex/0.158.0" || len(got[0].Files) != 1 {
		t.Fatalf("attribution = %+v", got)
	}
}
