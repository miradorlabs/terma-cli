package daemon

import (
	"context"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/relay"
)

// One relay runs per state directory: the one holding LockFile. The service's relay and a
// follower wait for it; any other relay that finds it held gives way.

// replacedMaxWait bounds how long a relay asked to make way waits for its hold to empty:
// the default hold, so an agent exporting without pause cannot keep the old terma running.
var replacedMaxWait = relay.DefaultHold

// followSlack is how long a following relay waits beyond replacedMaxWait, for the relay
// making way to deliver what it accepted and let go of its lock.
const followSlack = 30 * time.Second

// followWaiting runs once a following relay holds FollowLockFile and starts to wait; tests
// learn of it here, since probing the lock themselves would make the follower give way.
var followWaiting = func() {}

// lockPoll is how often the service's relay retries a lock another relay holds, and
// followPoll a following relay's: nothing listens between that relay's exit and the retry,
// and a follower is there for that moment alone, so its wait is shorter still.
const (
	lockPoll   = 250 * time.Millisecond
	followPoll = 25 * time.Millisecond
)

// lock takes the single-instance lock, retrying every poll while another relay holds it
// (never, with poll 0) and holding marker, when one is named, for as long as it waits; a nil
// unlock means this relay must not run.
func lock(ctx context.Context, dir string, poll time.Duration, marker string) (unlock func(), busy bool, err error) {
	path := filepath.Join(dir, LockFile)
	unlock, err = flock.TryLock(path)
	var unmark func()
	defer func() {
		if unmark != nil {
			unmark()
		}
	}()
	for poll > 0 && flock.IsBusy(err) {
		// Retried each round: a probe that held the marker a moment only delays it.
		if marker != "" && unmark == nil {
			if u, err := flock.TryLock(filepath.Join(dir, marker)); err == nil {
				unmark = u
			}
		}
		select {
		case <-ctx.Done():
			return nil, false, nil
		case <-time.After(poll):
		}
		unlock, err = flock.TryLock(path)
	}
	if flock.IsBusy(err) {
		return nil, true, nil
	}
	return unlock, false, err
}
