package hookrun

import "github.com/miradorlabs/terma-cli/internal/flock"

// lockEvidence takes a state file's lock if it is free, and gives up at once if not:
// another invocation holding it is already capturing the same session, so there is
// nothing to wait for. On a platform without flock it excludes nobody — atomic writes
// still keep each record whole, and simultaneous invocations may emit a duplicate.
func lockEvidence(path string) (func(), error) { return flock.TryLock(path) }

// isLockBusy tells a lock someone else holds from one that could not be taken at all.
func isLockBusy(err error) bool { return flock.IsBusy(err) }
