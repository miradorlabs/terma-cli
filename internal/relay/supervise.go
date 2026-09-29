package relay

import (
	"context"
	"os/exec"
	"time"
)

// Supervise keeps a relay running where no service manager restarts one (Windows):
// it starts the relay, and whenever the relay exits — a crash, or the exit that follows
// a self-update — starts it again, after a pause that grows from one second to a minute
// while the relay keeps dying quickly and resets once it has run for a minute. start is
// called afresh each time, so a binary replaced by an update is the one started next.
// It returns when ctx ends, stopping the relay it started.
func Supervise(ctx context.Context, start func(context.Context) *exec.Cmd, logf func(string, ...any)) {
	const minPause, maxPause, healthy = time.Second, time.Minute, time.Minute
	pause := minPause
	for ctx.Err() == nil {
		cmd := start(ctx)
		began := time.Now()
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		if time.Since(began) >= healthy {
			pause = minPause
		}
		logf("relay exited (%v); starting it again in %s", err, pause)
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		pause = min(pause*2, maxPause)
	}
}
