package daemon

import (
	"context"
	"math/rand/v2"
	"time"
)

// UpdateEvery is how often the relay asks for an update; the update's own records keep the
// release lookup daily, so this only bounds how late after that day it comes.
const UpdateEvery = time.Hour

// updateQuiet is how long the relay must have had no export before it restarts on the
// release it installed, so the restart falls in a pause rather than refusing an export: an
// agent at work exports every few seconds, an idle one about once a minute at most.
const updateQuiet = 30 * time.Second

// updateMaxWait bounds how long a relay that installed a release waits for its hold to
// empty and its agents to pause: an agent that never pauses for a working day cannot keep
// the earlier release running past it.
var updateMaxWait = 12 * time.Hour

// Updater keeps the relay's own terma up to date, so an install nobody runs a command on
// still takes each release: the relay is the one process that runs all day.
type Updater struct {
	// Update makes one attempt, returning the release it installed, "" when none.
	Update func(ctx context.Context) (string, error)
	// Every is how often Update is asked, each wait jittered by up to as long again so
	// machines that started together spread out.
	Every time.Duration
	Logf  func(format string, args ...any)
	// After and Jitter are the clock and the spread; time.After and a random fraction of
	// Every when nil.
	After  func(time.Duration) <-chan time.Time
	Jitter func(time.Duration) time.Duration
}

// Run asks for an update after a jittered wait and then every Every, until ctx ends or a
// release is installed, which it sends on installed.
func (u Updater) Run(ctx context.Context, installed chan<- string) {
	after, jitter := u.After, u.Jitter
	if after == nil {
		after = time.After
	}
	if jitter == nil {
		jitter = func(d time.Duration) time.Duration { return rand.N(max(d, 1)) }
	}
	wait := jitter(u.Every)
	for {
		select {
		case <-ctx.Done():
			return
		case <-after(wait):
		}
		version, err := u.Update(ctx)
		switch {
		case err != nil:
			u.Logf("automatic update failed: %v", err)
		case version != "":
			u.Logf("installed terma %s; restarting on it once no agent is exporting", version)
			installed <- version
			return
		}
		wait = u.Every + jitter(u.Every)
	}
}
