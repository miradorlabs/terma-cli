package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay"
)

// now fires at once, recording each wait it was asked for.
func now(waits *[]time.Duration) func(time.Duration) <-chan time.Time {
	return func(d time.Duration) <-chan time.Time {
		*waits = append(*waits, d)
		c := make(chan time.Time, 1)
		c <- time.Now()
		return c
	}
}

// The updater asks after a jittered wait, again every Every plus jitter after a failure or
// nothing new, and stops once it has installed a release, saying which.
func TestTheUpdaterAsksUntilItInstallsARelease(t *testing.T) {
	t.Parallel()
	var waits []time.Duration
	var logged []string
	answers := []struct {
		version string
		err     error
	}{{"", errors.New("offline")}, {"", nil}, {"1.3.0", nil}}
	calls := 0
	u := Updater{
		Every:  time.Hour,
		After:  now(&waits),
		Jitter: func(d time.Duration) time.Duration { return d / 4 },
		Logf:   func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
		Update: func(context.Context) (string, error) {
			a := answers[calls]
			calls++
			return a.version, a.err
		},
	}
	installed := make(chan string, 1)
	u.Run(t.Context(), installed)
	if got := <-installed; got != "1.3.0" || calls != 3 {
		t.Fatalf("installed %q after %d attempts, want 1.3.0 after 3", got, calls)
	}
	if want := []time.Duration{15 * time.Minute, 75 * time.Minute, 75 * time.Minute}; !slices.Equal(waits, want) {
		t.Fatalf("waited %v, want %v", waits, want)
	}
	if len(logged) != 2 || logged[0] != "automatic update failed: offline" {
		t.Fatalf("logged %q", logged)
	}
}

// A relay stopping before the updater's first wait is over makes no attempt.
func TestTheUpdaterStopsWithTheRelay(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	u := Updater{Every: time.Hour, Update: func(context.Context) (string, error) {
		t.Error("asked for an update after the relay stopped")
		return "", nil
	}}
	u.Run(ctx, make(chan string, 1))
}

// installsAtOnce is an updater that installs 1.3.0 on its first attempt.
func installsAtOnce() *Updater {
	var waits []time.Duration
	return &Updater{Every: time.Hour, After: now(&waits), Jitter: func(time.Duration) time.Duration { return 0 },
		Update: func(context.Context) (string, error) { return "1.3.0", nil }}
}

// An idle relay that installed a newer terma stops as soon as no agent has exported for
// updateQuiet, and asks to be started again, as the new binary.
func TestAnIdleRelayRestartsOnTheReleaseItInstalled(t *testing.T) {
	stateDir, _, _ := setUpRelay(t)
	c := runConfig(stateDir, 0, nil)
	c.Version, c.Service, c.Updater = "1.2.0", true, installsAtOnce()
	var ahead atomic.Int64
	c.Engine.Now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	r := startRun(t, c)
	r.await(t, "the relay")
	ahead.Store(int64(updateQuiet)) // no agent has exported since it started
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the idle relay kept running the release it replaced")
	}
	if !r.res.Updated || !r.res.Restart() {
		t.Fatalf("Run = %+v, want Updated and a restart", r.res)
	}
}

// A relay that installed a newer terma while it holds records waits for its hold to empty
// and its agents to fall quiet, however long that takes — even when a hook of the new
// release asks it to make way — so a restart drops nothing it held.
func TestAnUpdatedRelayDropsNothingItHolds(t *testing.T) {
	was := replacedMaxWait
	replacedMaxWait = time.Second
	t.Cleanup(func() { replacedMaxWait = was })
	stateDir, dir, token := setUpRelay(t)
	c := runConfig(stateDir, 0, nil)
	c.Addr, c.Version, c.Service, c.Updater = freeAddr(t), "1.2.0", true, installsAtOnce()
	var ahead atomic.Int64
	c.Engine.Now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	r := startRun(t, c)
	r.await(t, "the relay")
	postSessionlessSpans(t, c.Addr, token, 5)
	askToMakeWay(t, dir)
	select {
	case <-r.done:
		t.Fatalf("the relay restarted while it held: %+v", r.res)
	case <-time.After(3 * time.Second):
	}
	ahead.Store(int64(relay.DefaultTraceHold + time.Minute)) // the held spans age out
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay kept running once its hold was empty and its agents quiet")
	}
	if !r.res.Updated || !r.res.Restart() {
		t.Fatalf("Run = %+v, want Updated and a restart", r.res)
	}
	if got := counters(t, filepath.Join(dir, StatsFile))["dropped.no_session_trace_at_exit.traces"]; got != 0 {
		t.Fatalf("the restart dropped %d held spans", got)
	}
}
