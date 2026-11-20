package service

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A systemctl command that stops the relay waits as long as the unit lets it take to stop
// (TimeoutStopSec, StopWindow): cut short, a healthy restart would read as failed. One that
// returns at once keeps the short bound.
func TestSystemctlWaitsOutTheStopWindow(t *testing.T) {
	deadlines := map[string]time.Duration{}
	orig := command
	t.Cleanup(func() { command = orig })
	command = func(ctx context.Context, name string, args ...string) (string, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Fatalf("%s %v: no deadline", name, args)
		}
		deadlines[strings.Join(args[1:], " ")] = time.Until(d)
		return "", nil
	}
	ctx := context.Background()
	for _, args := range [][]string{
		{"restart", "x.service"}, {"stop", "x.service"}, {"disable", "--now", "x.service"},
		{"daemon-reload"}, {"enable", "x.service"}, {"is-active", "--quiet", "x.service"},
	} {
		if _, err := systemctl(ctx, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, stops := range []string{"restart x.service", "stop x.service", "disable --now x.service"} {
		if got := deadlines[stops]; got < StopWindow {
			t.Errorf("systemctl --user %s: deadline in %v; want at least StopWindow, %v", stops, got, StopWindow)
		}
	}
	for _, quick := range []string{"daemon-reload", "enable x.service", "is-active --quiet x.service"} {
		if got := deadlines[quick]; got > commandTimeout {
			t.Errorf("systemctl --user %s: deadline in %v; want at most %v", quick, got, commandTimeout)
		}
	}
}

// launchd's bootout and the bootstrap retries after it wait no shorter than the relay may
// take to stop, and no longer than RestartWait. (The Windows supervisor's wait,
// supervisorWait, is RestartWait too, in a Windows-only file.)
func TestEveryStopWaitCoversTheStopWindow(t *testing.T) {
	for name, wait := range map[string]time.Duration{"stopTimeout": stopTimeout, "bootoutWait": bootoutWait} {
		if wait < StopWindow {
			t.Errorf("%s = %v; want at least StopWindow, %v", name, wait, StopWindow)
		}
	}
	if stopTimeout > RestartWait || bootoutWait > RestartWait {
		t.Errorf("a stop wait exceeds RestartWait, %v, which refreshTimeout in internal/cli is sized by", RestartWait)
	}
}
