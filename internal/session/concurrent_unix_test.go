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

// patient gives a store a wait no test will outlast, so exclusion tests do not measure
// the machine (hookLockWait kept 15 of 24 files on a loaded CI runner).
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
