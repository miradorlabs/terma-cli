package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// The registry is append-only and ordered: an ID a machine has recorded must always
// mean the same migration, and Run relies on the order.
func TestRegistryIsOrderedAndNamed(t *testing.T) {
	last := 0
	for _, m := range migrations {
		if m.ID <= last || m.Name == "" || m.Run == nil {
			t.Fatalf("migration %d (%q) out of order, unnamed or empty after %d", m.ID, m.Name, last)
		}
		last = m.ID
	}
}

// with replaces the registry for one test.
func with(t *testing.T, ms ...Migration) {
	t.Helper()
	original := migrations
	migrations = ms
	t.Cleanup(func() { migrations = original })
}

func TestNothingIsPendingWithoutAConfigDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created")
	if Pending(missing) {
		t.Fatal("a machine with no config directory has nothing to migrate")
	}
	if applied, err := Run(context.Background(), missing, true); err != nil || applied != nil {
		t.Fatalf("Run = %v, %v", applied, err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("Run created the config directory")
	}
}

func TestRunAppliesInOrderAndRecordsEach(t *testing.T) {
	dir := t.TempDir()
	var ran []int
	step := func(id int) Migration {
		return Migration{ID: id, Name: "step", Run: func() error { ran = append(ran, id); return nil }}
	}
	with(t, step(1), step(2), step(5))
	if !Pending(dir) {
		t.Fatal("a machine that has run nothing is pending")
	}
	applied, err := Run(context.Background(), dir, false)
	if err != nil || len(applied) != 3 || !slices.Equal(ran, []int{1, 2, 5}) {
		t.Fatalf("applied %v, ran %v, err %v", applied, ran, err)
	}
	if s, _ := Load(dir); s.Applied != 5 || s.Failed != nil || Pending(dir) {
		t.Fatalf("state %+v", s)
	}
	if applied, err := Run(context.Background(), dir, false); err != nil || len(applied) != 0 || len(ran) != 3 {
		t.Fatalf("second run applied %v, ran %v, err %v", applied, ran, err)
	}
	// A newer build's migration arrives: only it runs.
	with(t, step(1), step(2), step(5), step(6))
	if _, err := Run(context.Background(), dir, false); err != nil || !slices.Equal(ran, []int{1, 2, 5, 6}) {
		t.Fatalf("ran %v, err %v", ran, err)
	}
	// An older build sees a record past its own registry: nothing for it to do.
	with(t, step(1))
	if Pending(dir) {
		t.Fatal("an older build treats a newer record as pending")
	}
}

// A failure stops the run and is recorded. A start that is not asked to retry leaves it
// alone for a while — a hook fires on every tool call — and one that is asked runs it
// again from there, clearing the record when it gets through.
func TestAFailedMigrationStopsTheRunAndIsRetried(t *testing.T) {
	dir := t.TempDir()
	broken, ran := true, 0
	with(t,
		Migration{ID: 1, Name: "first", Run: func() error { return nil }},
		Migration{ID: 2, Name: "second", Run: func() error {
			ran++
			if broken {
				return errors.New("disk full")
			}
			return nil
		}},
		Migration{ID: 3, Name: "third", Run: func() error { return nil }},
	)
	applied, err := Run(context.Background(), dir, false)
	if err == nil || !slices.Equal(applied, []string{"first"}) || ran != 1 {
		t.Fatalf("applied %v, ran %d, err %v", applied, ran, err)
	}
	s, _ := Load(dir)
	if s.Applied != 1 || s.Failed == nil || s.Failed.ID != 2 || s.Failed.Error != "disk full" || !Pending(dir) {
		t.Fatalf("state %+v", s)
	}
	if _, err := Run(context.Background(), dir, false); err == nil || ran != 1 {
		t.Fatalf("a recent failure was attempted again without retry (ran %d, err %v)", ran, err)
	}
	broken = false
	applied, err = Run(context.Background(), dir, true)
	if err != nil || !slices.Equal(applied, []string{"second", "third"}) {
		t.Fatalf("retry applied %v, err %v", applied, err)
	}
	if s, _ := Load(dir); s.Applied != 3 || s.Failed != nil {
		t.Fatalf("state after retry %+v", s)
	}

	// Past RetryAfter, an ordinary start tries again too.
	with(t, Migration{ID: 4, Name: "fourth", Run: func() error { return nil }})
	stale := State{Applied: 3, Failed: &Failure{ID: 4, Name: "fourth", At: time.Now().Add(-RetryAfter - time.Minute), Error: "x"}}
	if err := config.WriteJSON(filepath.Join(dir, stateFile), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if applied, err := Run(context.Background(), dir, false); err != nil || len(applied) != 1 {
		t.Fatalf("stale failure: applied %v, err %v", applied, err)
	}
}

// An unreadable record is not trusted as "all applied": every migration is idempotent,
// so running them again is safe, and the rewrite repairs it.
func TestAnUnreadableRecordIsRepaired(t *testing.T) {
	dir := t.TempDir()
	ran := 0
	with(t, Migration{ID: 1, Name: "only", Run: func() error { ran++; return nil }})
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !Pending(dir) {
		t.Fatal("an unreadable record must be pending")
	}
	if _, err := Run(context.Background(), dir, false); err != nil || ran != 1 || Pending(dir) {
		t.Fatalf("ran %d, err %v, pending %v", ran, err, Pending(dir))
	}
}

// Concurrent starts share one lock; one that cannot get it within its bound changes
// nothing and says so.
func TestRunWaitsForAnotherMigratingProcess(t *testing.T) {
	dir := t.TempDir()
	ran := 0
	with(t, Migration{ID: 1, Name: "only", Run: func() error { ran++; return nil }})
	unlock, err := flock.Lock(context.Background(), filepath.Join(dir, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Run(ctx, dir, false); err == nil || ran != 0 {
		t.Fatalf("ran %d while another process held the lock (err %v)", ran, err)
	}
	unlock()
	if _, err := Run(context.Background(), dir, false); err != nil || ran != 1 {
		t.Fatalf("after release: ran %d, err %v", ran, err)
	}
}
