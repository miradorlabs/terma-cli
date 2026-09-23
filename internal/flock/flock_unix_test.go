//go:build unix

package flock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockExcludesASecondHolderUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	unlock, err := Lock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := Lock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held lock was taken again: err = %v", err)
	}

	unlock()
	again, err := Lock(context.Background(), path)
	if err != nil {
		t.Fatalf("a released lock could not be taken: %v", err)
	}
	again()
}

func TestLockGivesUpWhenTheContextIsAlreadyDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Lock(ctx, filepath.Join(t.TempDir(), "state.lock")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestTryLockReportsAHeldLockAsBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	unlock, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(path); !IsBusy(err) {
		t.Fatalf("a held lock should be busy, got %v", err)
	}
	// The two entry points exclude each other: they are one lock.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := Lock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock took what TryLock holds: %v", err)
	}
	unlock()
	again, err := TryLock(path)
	if err != nil {
		t.Fatalf("a released lock could not be taken: %v", err)
	}
	again()
}

// A lock file lives in a directory other processes can write to; TryLock must not be
// walked through a link into locking, or creating, a file somewhere else.
func TestTryLockRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	link := filepath.Join(dir, "state.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(link); err == nil || IsBusy(err) {
		t.Fatalf("a symlinked lock path must be an error that is not \"busy\": %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("the link's target was created")
	}
}
