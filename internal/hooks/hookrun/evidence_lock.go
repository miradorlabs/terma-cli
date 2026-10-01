package hookrun

import "github.com/miradorlabs/terma-cli/internal/flock"

// LockEvidence takes a state file's lock or gives up at once: its holder is already capturing the session.
func LockEvidence(path string) (func(), error) { return flock.TryLock(path) }

func isLockBusy(err error) bool { return flock.IsBusy(err) }
