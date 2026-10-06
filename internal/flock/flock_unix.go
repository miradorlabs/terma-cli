//go:build unix

package flock

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// A lock file can be removed between its open and its flock (state prunes remove
// abandoned locks), and a flock on the removed file excludes no one who opens the path
// afterwards; so a lock counts only while the path still names the file locked.
func lock(ctx context.Context, path string) (func(), error) {
	for {
		l, err := lockOnce(ctx, path)
		if err != nil {
			return nil, err
		}
		if held(l.f, path) {
			return l.release, nil
		}
		l.release()
	}
}

type lockedFile struct{ f *os.File }

func (l *lockedFile) release() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

// held reports whether path still names f.
func held(f *os.File, path string) bool {
	a, errA := f.Stat()
	b, errB := os.Stat(path)
	return errA == nil && errB == nil && os.SameFile(a, b)
}

func lockOnce(ctx context.Context, path string) (*lockedFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return nil, err
	}
	wait := pollMin
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		// Polled rather than a blocking LOCK_EX, which cannot be cancelled.
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
		wait = min(wait*2, pollMax)
	}
	return &lockedFile{f}, nil
}

func tryLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, fileMode)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	l := &lockedFile{f}
	if !held(f, path) {
		l.release()
		return nil, syscall.EWOULDBLOCK // removed under us: someone else's turn
	}
	return l.release, nil
}

func isBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
