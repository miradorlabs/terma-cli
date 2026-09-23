// Package flock is terma's advisory file lock: what serializes a read-modify-write of a
// small state file between the processes that share it. Hooks are short-lived and run
// concurrently — one per tool call, several agents at once — and every state file is
// replaced by an atomic rename, so a write never tears; without a lock, though, two
// writers each read the old document and the second rename silently drops the first
// one's change.
//
// The lock belongs on a sidecar file, never on the file it protects: the rename swaps
// the inode, and a lock held on the old one guards nothing.
package flock

import (
	"context"
	"time"
)

// fileMode is what a lock file is created with. It holds nothing, but it sits beside
// credentials and keys and should not be the one world-readable thing there.
const fileMode = 0o600

// A contended lock is retried after pollMin, doubling up to pollMax. Holders are a
// read-modify-write of a few kilobytes — well under a millisecond — so the retries have
// to be on that scale: polled at a fixed five milliseconds, the lock changed hands at
// most two hundred times a second however short the work, three hooks behind one
// another took over ten milliseconds, and two dozen outlasted a 250 ms wait on a slow
// machine. Measured with 200 µs holders: three finish in 1.2 ms and twenty-four in 22 ms,
// against 118 ms before. An attempt is one non-blocking flock call, about a microsecond,
// so a thousand a second is nothing even across a flush's thirty-second wait.
const (
	pollMin = 100 * time.Microsecond
	pollMax = time.Millisecond
)

// Lock takes an exclusive lock on path, creating the file if needed, and returns the
// release. It waits until the lock is free or ctx is done, so a caller that must not
// hang — a hook — bounds the wait with a deadline.
//
// Where the platform has no flock, Lock succeeds without excluding anyone: concurrent
// hooks there are rare, and the atomic rename still keeps each file whole.
func Lock(ctx context.Context, path string) (unlock func(), err error) {
	return lock(ctx, path)
}

// TryLock takes the lock only if it is free right now, and never follows a symlink at
// path. It is for a hook that would rather skip than wait — another invocation holding
// the lock is already doing the same work — and whose lock file sits where a link could
// have been planted. A lock someone else holds is reported by IsBusy.
func TryLock(path string) (unlock func(), err error) {
	return tryLock(path)
}

// IsBusy reports whether err is TryLock finding the lock held, as opposed to failing.
func IsBusy(err error) bool {
	return isBusy(err)
}
