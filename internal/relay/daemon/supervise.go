package daemon

import (
	"context"
	"os/exec"
	"time"
)

// Windows has no per-user service manager that restarts a process: the Run key starts
// a program once, at logon. `terma relay supervise` is what it starts, and it does what
// launchd's KeepAlive and systemd's Restart=on-failure do elsewhere: it runs the relay
// and starts it again when it exits asking to be (nonzero: a crash, or the step aside
// after its binary was replaced), for as long as the service stays installed. A relay
// that exits 0 is done for good — its token is gone, terma was uninstalled — and the
// supervisor ends with it.

// Supervisor is what Supervise needs, so a test can drive it without a relay.
type Supervisor struct {
	// Start returns the relay's command, afresh each time: a binary an update replaced
	// is the one started next.
	Start func() *exec.Cmd
	// Installed reports whether the service is still wanted; once it is not, the relay
	// is stopped and the supervisor returns.
	Installed func() bool
	// Stop asks the running relay to stop, and waits for it.
	Stop func()
	Logf func(string, ...any)
	// MinPause doubles to MaxPause while the relay keeps dying, and resets once it has
	// run for Healthy; Poll is how often Installed is asked.
	MinPause, MaxPause, Healthy, Poll time.Duration
}

// Supervise runs the relay until ctx ends, the service is removed, or the relay exits
// 0.
func Supervise(ctx context.Context, sv Supervisor) {
	pause := sv.MinPause
	for ctx.Err() == nil && sv.Installed() {
		cmd := sv.Start()
		began := time.Now()
		if err := cmd.Start(); err != nil {
			sv.Logf("start the relay: %v", err)
		} else {
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			exited, err := sv.watch(ctx, cmd, done)
			if !exited || err == nil {
				return
			}
		}
		if time.Since(began) >= sv.Healthy {
			pause = sv.MinPause
		}
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		pause = min(pause*2, sv.MaxPause)
	}
}

// watch waits for the relay to exit, true (with how it exited) when it did on its own;
// when ctx ends or the service is removed it stops the relay — asked, so it delivers
// what it accepted, and killed only if it will not go — and reports false.
func (sv Supervisor) watch(ctx context.Context, cmd *exec.Cmd, done <-chan error) (bool, error) {
	tick := time.NewTicker(sv.Poll)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			sv.Logf("relay exited (%v)", err)
			return true, err
		case <-ctx.Done():
		case <-tick.C:
			if sv.Installed() {
				continue
			}
		}
		sv.Stop()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		return false, nil
	}
}
