//go:build windows

package flock

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Windows locks the first byte with LockFileEx, polled because a blocked lock cannot be cancelled.

func lockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
}

func unlockFile(f *os.File) {
	var ol windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
	_ = f.Close()
}

func lock(ctx context.Context, path string) (func(), error) {
	f, err := openLockFile(ctx, path)
	if err != nil {
		return nil, err
	}
	wait := pollMin
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err := lockFile(f)
		if err == nil {
			break
		}
		if !isBusy(err) {
			_ = f.Close()
			return nil, err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
		wait = min(wait*2, pollMax)
	}
	return func() { unlockFile(f) }, nil
}

// openLockFile opens the lock file at path, waiting out one that Remove is deleting:
// Windows refuses to open a file pending deletion, with ERROR_ACCESS_DENIED, until its last
// handle closes. A denial that outlasts ctx is returned as it is.
func openLockFile(ctx context.Context, path string) (*os.File, error) {
	wait := pollMin
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, fileMode)
		if err == nil || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return f, err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, err
		case <-timer.C:
		}
		wait = min(wait*2, pollMax)
	}
}

func tryLock(path string) (func(), error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("refusing to lock through a symbolic link: " + path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { unlockFile(f) }, nil
}

func isBusy(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING)
}
