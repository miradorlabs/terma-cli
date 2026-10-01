// Package flock is terma's advisory file lock, serializing a read-modify-rename of a
// state file between concurrent hooks. Lock a sidecar, never the file the rename
// replaces: a lock on the old inode guards nothing.
package flock

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// fileMode keeps lock files private: they sit beside credentials and keys.
const fileMode = 0o600

// A contended lock is retried after pollMin, doubling up to pollMax, because holders
// finish in well under a millisecond: with 200 µs holders, twenty-four take 22 ms
// (118 ms at a fixed 5 ms poll).
const (
	pollMin = 100 * time.Microsecond
	pollMax = time.Millisecond
)

// Lock takes an exclusive lock on path, creating it if needed, waiting until it is free
// or ctx is done; without platform locks it succeeds without excluding anyone.
func Lock(ctx context.Context, path string) (unlock func(), err error) {
	return lock(ctx, path)
}

// TryLock takes the lock only if it is free now, never following a symlink at path; a
// held lock is reported by IsBusy.
func TryLock(path string) (unlock func(), err error) {
	return tryLock(path)
}

// IsBusy reports whether err is TryLock finding the lock held, as opposed to failing.
func IsBusy(err error) bool {
	return isBusy(err)
}

// Locked runs fn holding path's sidecar lock, waiting at most wait. path's directory is
// created first: the lock may be the first thing written there.
func Locked(path string, wait time.Duration, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	unlock, err := Lock(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}
