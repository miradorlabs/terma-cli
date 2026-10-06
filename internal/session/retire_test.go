package session

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

// retireStore is a store with one manifest touched at touched and a lock file created at
// locked, its root returned too.
func retireStore(t *testing.T, touched, locked time.Time) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	s := Open(filepath.Join(root, "store"))
	if err := s.Touch(Session{ID: "s1", Tool: "codex"}, []string{"a.go"}, touched); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(s.dir, lockFile)
	if err := os.Chtimes(lock, locked, locked); err != nil {
		t.Fatal(err)
	}
	return s, root
}

func storeGone(t *testing.T, s *Store) bool {
	t.Helper()
	_, err := os.Stat(s.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return errors.Is(err, fs.ErrNotExist)
}

// A store whose every record is past the cutoff, created before it and held by no one goes.
func TestRetireRemovesAnEmptyOldUnheldStore(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	s, root := retireStore(t, old, old)
	s.Retire(now.Add(-24 * time.Hour))
	if !storeGone(t, s) {
		left, _ := os.ReadDir(s.dir)
		t.Fatalf("the store survived: %v", left)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("Retire removed more than its store: %v", err)
	}
}

// A record touched after the cutoff keeps the store and itself.
func TestRetireKeepsAStoreTouchedAfterTheCutoff(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	s, _ := retireStore(t, old, old)
	if err := s.Touch(Session{ID: "s2", Tool: "codex"}, []string{"b.go"}, now); err != nil {
		t.Fatal(err)
	}
	s.Retire(now.Add(-24 * time.Hour))
	if storeGone(t, s) {
		t.Fatal("a store with a fresh record went")
	}
	ms, err := s.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if ids := sessionIDs(ms); !slices.Equal(ids, []string{"s2"}) {
		t.Fatalf("manifests after Retire = %v, want only the fresh s2", ids)
	}
}

// A store created after the cutoff stays even once empty: a hook may have just made it.
func TestRetireKeepsANewEmptyStore(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	s, _ := retireStore(t, old, now)
	s.Retire(now.Add(-24 * time.Hour))
	if storeGone(t, s) {
		t.Fatal("a store created after the cutoff went")
	}
	if ms, _ := s.Manifests(); len(ms) != 0 {
		t.Fatalf("the old record survived: %v", sessionIDs(ms))
	}
}

// With another holder on the lock, Retire changes nothing, not even an old record.
func TestRetireGivesUpWhileTheStoreIsHeld(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	s, _ := retireStore(t, old, old)
	s.lockWait = 20 * time.Millisecond
	unlock, err := flock.Lock(context.Background(), filepath.Join(s.dir, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	s.Retire(now.Add(-24 * time.Hour))
	unlock()
	if storeGone(t, s) {
		t.Fatal("a held store went")
	}
	if ms, _ := s.Manifests(); len(ms) != 1 {
		t.Fatalf("a held store was pruned: %v", sessionIDs(ms))
	}
}

// A store nothing created is left uncreated.
func TestRetireCreatesNothing(t *testing.T) {
	t.Parallel()
	s := Open(filepath.Join(t.TempDir(), "store"))
	s.Retire(time.Now())
	if !storeGone(t, s) {
		t.Fatal("Retire created the store")
	}
}

func sessionIDs(ms []Manifest) []string {
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.SessionID)
	}
	slices.Sort(ids)
	return ids
}

// Writers recording sessions and a loop retiring the store whenever it empties never lose
// a record the cutoff did not reach, and never hold the store lock twice at once. Each
// writer takes the lock through its own open file, which is how separate processes
// contend: flock and LockFileEx exclude per open file. TERMA_RETIRE_STRESS sets the
// duration (default 300ms).
func TestRetireRacingWritersLosesNothing(t *testing.T) {
	t.Parallel()
	run := 300 * time.Millisecond
	if v := os.Getenv("TERMA_RETIRE_STRESS"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatal(err)
		}
		run = d
	}
	// A record is old to Retire once lag has passed; a writer reads its record back well
	// within that, so a read that misses it is a loss, not an aging. Writers work in
	// bursts with quiet gaps longer than lag, so the store empties and goes between them,
	// and each burst races a removal.
	const lag = 20 * time.Millisecond
	const burst, quiet = 40 * time.Millisecond, 30 * time.Millisecond
	const writers = 8
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "holder")
	newStore := func() *Store { s := Open(root); s.lockWait = time.Minute; return s }

	type record struct {
		id string
		at time.Time
	}
	var (
		recordsMu sync.Mutex
		records   []record
		doubles   atomic.Int32
		holds     atomic.Int32
		retires   atomic.Int32
		removed   atomic.Int32
		cutoffMax atomic.Int64
	)
	// hold takes the store lock as a writer would and proves no one else has it.
	hold := func(s *Store) error {
		unlock, held := s.acquire()
		defer unlock()
		if !held {
			return nil // the store was being removed; the next writer recreates it
		}
		f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				doubles.Add(1)
				return nil
			}
			return err
		}
		holds.Add(1)
		time.Sleep(50 * time.Microsecond)
		f.Close()
		return os.Remove(marker)
	}

	start := time.Now()
	deadline := start.Add(run)
	busy := func() bool { return time.Since(start)%(burst+quiet) < burst }
	var wg sync.WaitGroup
	errs := make(chan error, writers+1)
	for w := range writers {
		wg.Go(func() {
			for n := 0; time.Now().Before(deadline); n++ {
				if !busy() {
					time.Sleep(time.Millisecond)
					continue
				}
				s := newStore()
				id := fmt.Sprintf("w%d-%d", w, n)
				at := time.Now()
				if err := s.Touch(Session{ID: id, Tool: "codex"}, []string{"f.go"}, at); err != nil {
					errs <- fmt.Errorf("touch %s: %w", id, err)
					return
				}
				recordsMu.Lock()
				records = append(records, record{id, at})
				recordsMu.Unlock()
				ms, err := newStore().Manifests()
				if err != nil {
					errs <- fmt.Errorf("read back %s: %w", id, err)
					return
				}
				if !slices.ContainsFunc(ms, func(m Manifest) bool { return m.SessionID == id }) && time.Since(at) < lag {
					errs <- fmt.Errorf("%s was lost %s after it was written", id, time.Since(at))
					return
				}
				if err := hold(s); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Go(func() {
		for time.Now().Before(deadline) {
			cutoff := time.Now().Add(-lag)
			cutoffMax.Store(cutoff.UnixNano())
			s := newStore()
			if s.missing() {
				continue
			}
			s.Retire(cutoff)
			retires.Add(1)
			if s.missing() {
				removed.Add(1)
			}
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := doubles.Load(); n > 0 {
		t.Errorf("the store lock had two holders %d times", n)
	}

	ms, err := newStore().Manifests()
	if err != nil {
		t.Fatal(err)
	}
	last := time.Unix(0, cutoffMax.Load())
	kept := map[string]bool{}
	for _, m := range ms {
		kept[m.SessionID] = true
	}
	lost := 0
	for _, r := range records {
		if !r.at.Before(last) && !kept[r.id] {
			lost++
		}
	}
	if lost > 0 {
		t.Errorf("%d records written after the last cutoff are gone", lost)
	}
	t.Logf("%d records, %d holds proven exclusive, %d retires of an existing store, %d removals", len(records), holds.Load(), retires.Load(), removed.Load())
	if removed.Load() == 0 {
		t.Log("the store was never removed: raise TERMA_RETIRE_STRESS for a run that exercises removal")
	}
}
