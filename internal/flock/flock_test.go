package flock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Every platform's lock: one holder at a time, TryLock reports busy, Lock waits and
// can be cancelled, and a released lock can be taken again.
func TestLockExcludesAndReleases(t *testing.T) {
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
