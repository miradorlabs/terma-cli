package daemon

import (
	"context"
	"os/exec"
	"time"
)

// Windows' Run key starts a program once, at logon, so `terma relay supervise` restarts the
// relay on a nonzero exit, as launchd's KeepAlive and systemd's Restart=on-failure do.

// Supervisor is what Supervise needs, so a test can drive it without a relay.
type Supervisor struct {
	// Start returns the relay's command afresh each time, so a replaced binary is the one started next.
	Start func() *exec.Cmd
	// Installed reports whether the service is still wanted.
	Installed func() bool
	Stop      func()
	Logf      func(string, ...any)
	// MinPause doubles to MaxPause while the relay keeps dying, and resets once it has
	// run for Healthy; Poll is how often Installed is asked.
	MinPause, MaxPause, Healthy, Poll time.Duration
}

// Supervise runs the relay until ctx ends, the service is removed, or the relay exits 0.
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

// watch reports true when the relay exited on its own; otherwise it asks the relay to stop,
// so it delivers what it accepted, and kills it only if it will not go.
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
