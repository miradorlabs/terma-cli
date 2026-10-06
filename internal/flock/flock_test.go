package flock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Every platform's lock: one holder at a time, TryLock reports busy, Lock waits and
// can be cancelled, and a released lock can be taken again.
func TestLockExcludesAndReleases(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(path); err == nil || !IsBusy(err) {
		t.Fatalf("a held lock was taken twice, or not reported busy: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Lock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock on a held lock returned %v, want the context's deadline", err)
	}
	unlock()
	again, err := Lock(context.Background(), path)
	if err != nil {
		t.Fatalf("a released lock could not be taken: %v", err)
	}
	again()
}

// A record fn deletes leaves no lock behind; one it keeps keeps its lock.
func TestLockedRemovesTheLockOfADeletedFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "record.json")
	write := func() error { return os.WriteFile(path, []byte("{}"), 0o600) }
	if err := Locked(path, time.Second, write); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("a kept record lost its lock: %v", err)
	}
	if err := Locked(path, time.Second, func() error { return os.Remove(path) }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("a deleted record left its lock: %v", err)
	}
}

// Remove deletes a held lock and frees it on every platform. Windows cannot delete an open
// file, so a Remove that deleted before it released would leave the file there.
func TestRemoveDeletesAHeldLockAndFreesIt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Lock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	Remove(path, unlock)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the lock file survived Remove: %v", err)
	}
	again, err := TryLock(path)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	again()
}
