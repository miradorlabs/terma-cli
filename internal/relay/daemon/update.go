package daemon

import (
	"context"
	"math/rand/v2"
	"time"
)

// UpdateEvery is how often the relay asks for an update, jittered to every 5 to 10 minutes,
// each ask a fresh look (selfupdate.Client.Auto): a patch release, which installs without a
// soak, reaches a running relay within 10 minutes of its build finishing, also while the
// relay still waits to restart on a release it installed before.
const UpdateEvery = 5 * time.Minute

// updateQuiet is how long the relay must have had no export before it restarts on the
// release it installed, so the restart falls in a pause rather than refusing an export: an
// agent at work exports every few seconds, an idle one about once a minute at most.
const updateQuiet = 30 * time.Second

// updatePauseWait is how long a relay that installed a release waits for its agents to
// pause; after it, an agent that never pauses for a working day no longer keeps the earlier
// release running, and the relay restarts at the first moment it holds nothing. It never
// restarts while it holds anything: that lives in memory alone.
var updatePauseWait = 12 * time.Hour

// Pass is what one of the relay's update passes found.
type Pass struct {
	// Installed is the release now in place of the relay's terma, "" for none.
	Installed string
	// Replaced means another install put it there, not the relay.
	Replaced bool
}

// Updater keeps the relay's own terma up to date, so an install nobody runs a command on
// still takes each release: the relay is the one process that runs all day.
type Updater struct {
	// Update makes one attempt, returning the release it installed or found in place.
	Update func(ctx context.Context) (Pass, error)
	// Every is how often Update is asked, each wait jittered by up to as long again so
	// machines that started together spread out.
	Every time.Duration
	Logf  func(format string, args ...any)
	// After and Jitter are the clock and the spread; time.After and a random fraction of
	// Every when nil.
	After  func(time.Duration) <-chan time.Time
	Jitter func(time.Duration) time.Duration
}

// Run asks for an update after a jittered wait and then every Every, until ctx ends, sending
// each release it installs on installed. It goes on asking after an install: the relay may
// wait hours for its agents to pause before it restarts, and a release published meanwhile
// would otherwise reach the machine only once it had. Every must be positive: asking without
// pause would only hammer the release lookup, so an updater without one asks nothing.
func (u Updater) Run(ctx context.Context, installed chan<- string) {
	if u.Every <= 0 {
		return
	}
	after, jitter, logf := u.After, u.Jitter, u.Logf
	if after == nil {
		after = time.After
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if jitter == nil {
		jitter = func(d time.Duration) time.Duration { return rand.N(d) }
	}
	wait := jitter(u.Every)
	for {
		select {
		case <-ctx.Done():
			return
		case <-after(wait):
		}
		pass, err := u.Update(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			// Cut short by the relay stopping, which is no failure to report.
			return
		case err != nil:
			logf("automatic update failed: %v", err)
		case pass.Installed != "":
			if pass.Replaced {
				logf("replaced by terma %s, which another install put in place; restarting on it once no agent is exporting", pass.Installed)
			} else {
				logf("installed terma %s; restarting on it once no agent is exporting", pass.Installed)
			}
			select {
			case installed <- pass.Installed:
			case <-ctx.Done():
				return
			}
		}
		wait = u.Every + jitter(u.Every)
	}
}
