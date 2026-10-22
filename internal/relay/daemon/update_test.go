package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
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

// once fires at once the first time it is asked to wait and never again: an updater's clock
// whose second ask is still to come.
func once() func(time.Duration) <-chan time.Time {
	asked := false
	return func(time.Duration) <-chan time.Time {
		if asked {
			return nil
		}
		asked = true
		c := make(chan time.Time, 1)
		c <- time.Now()
		return c
	}
}

// The updater asks after a jittered wait and again every Every plus jitter, whatever the
// answer was: a failure, nothing new, or a release it installed, each of which it sends. A
// release published while the relay waits to restart on the last is so installed too.
func TestTheUpdaterGoesOnAskingAfterItInstallsARelease(t *testing.T) {
	t.Parallel()
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	var waits []time.Duration
	var logged []string
	answers := []struct {
		version string
		err     error
	}{{"", errors.New("offline")}, {"", nil}, {"1.3.0", nil}, {"", nil}, {"1.3.1", nil}}
	calls := 0
	u := Updater{
		Every: time.Hour,
		After: func(d time.Duration) <-chan time.Time {
			if calls == len(answers) {
				stop() // the relay stops before the next ask
				return nil
			}
			return now(&waits)(d)
		},
		Jitter: func(d time.Duration) time.Duration { return d / 4 },
		Logf:   func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
		Update: func(context.Context) (Pass, error) {
			a := answers[calls]
			calls++
			return Pass{Installed: a.version}, a.err
		},
	}
	installed := make(chan string, len(answers))
	u.Run(ctx, installed)
	close(installed)
	var got []string
	for version := range installed {
		got = append(got, version)
	}
	if !slices.Equal(got, []string{"1.3.0", "1.3.1"}) || calls != 5 {
		t.Fatalf("installed %q after %d attempts, want 1.3.0 then 1.3.1 after 5", got, calls)
	}
	if want := []time.Duration{15 * time.Minute, 75 * time.Minute, 75 * time.Minute, 75 * time.Minute, 75 * time.Minute}; !slices.Equal(waits, want) {
		t.Fatalf("waited %v, want %v", waits, want)
	}
	if len(logged) != 3 || logged[0] != "automatic update failed: offline" {
		t.Fatalf("logged %q", logged)
	}
}

// An updater whose relay stops while nothing takes the release it installed ends with the
// relay, rather than keeping it from stopping.
func TestTheUpdaterNeverOutlivesTheRelayToSayWhatItInstalled(t *testing.T) {
	t.Parallel()
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	u := Updater{Every: time.Hour, After: once(), Jitter: func(time.Duration) time.Duration { return 0 },
		Update: func(context.Context) (Pass, error) {
			stop()
			return Pass{Installed: "1.3.0"}, nil
		}}
	done := make(chan struct{})
	go func() { u.Run(ctx, make(chan string)); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the updater still waits to send an install nothing will take")
	}
}

// A pass the relay cut short by stopping is no failed update to report in its log.
func TestAPassCutShortByTheRelayStoppingIsNoFailure(t *testing.T) {
	t.Parallel()
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	var logged []string
	u := Updater{Every: time.Hour, After: once(), Jitter: func(time.Duration) time.Duration { return 0 },
		Logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
		Update: func(ctx context.Context) (Pass, error) {
			stop()
			return Pass{}, ctx.Err()
		}}
	u.Run(ctx, make(chan string, 1))
	if len(logged) != 0 {
		t.Fatalf("logged %q for a pass the relay cut short", logged)
	}
}

// A relay stopping before the updater's first wait is over makes no attempt.
func TestTheUpdaterStopsWithTheRelay(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	u := Updater{Every: time.Hour, Update: func(context.Context) (Pass, error) {
		t.Error("asked for an update after the relay stopped")
		return Pass{}, nil
	}}
	u.Run(ctx, make(chan string, 1))
}

// An updater without a period asks nothing, rather than asking without pause.
func TestAnUpdaterWithoutAPeriodAsksNothing(t *testing.T) {
	t.Parallel()
	u := Updater{Update: func(context.Context) (Pass, error) {
		t.Error("asked for an update with no period")
		return Pass{}, nil
	}}
	u.Run(t.Context(), make(chan string, 1))
}

// installsAtOnce is an updater that installs 1.3.0 on its first attempt; its next is an
// hour away.
func installsAtOnce() *Updater {
	return &Updater{Every: time.Hour, After: once(), Jitter: func(time.Duration) time.Duration { return 0 },
		Update: func(context.Context) (Pass, error) { return Pass{Installed: "1.3.0"}, nil }}
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
	if !r.res.Updated || !r.res.Restart() || r.res.Installed != "1.3.0" {
		t.Fatalf("Run = %+v, want Updated to 1.3.0 and a restart", r.res)
	}
}

// A release another install put in place restarts the relay as one it installed would, and
// the log says whose it was.
func TestAReleaseAnotherInstallPutInPlaceIsLoggedAsSuch(t *testing.T) {
	t.Parallel()
	var logged []string
	u := Updater{Every: time.Hour, After: once(), Jitter: func(time.Duration) time.Duration { return 0 },
		Logf:   func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
		Update: func(context.Context) (Pass, error) { return Pass{Installed: "1.3.0", Replaced: true}, nil }}
	ctx, cancel := context.WithCancel(t.Context())
	installed := make(chan string, 1)
	go func() { <-installed; cancel() }()
	u.Run(ctx, installed)
	if len(logged) != 1 || !strings.HasPrefix(logged[0], "replaced by terma 1.3.0") {
		t.Fatalf("logged %q", logged)
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

// A relay that installed a newer terma restarts even while a route keeps failing, such as
// a revoked key answered 401: its queue is on disk, and the next relay delivers it.
func TestAnUpdatedRelayRestartsWhileARouteKeepsFailing(t *testing.T) {
	var refused atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refused.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(upstream.Close)
	stateDir, _, token := setUpRelay(t)
	c := runConfig(stateDir, 0, nil)
	c.Addr, c.Version, c.Service = freeAddr(t), "1.2.0", true
	var ahead atomic.Int64
	c.Engine.Now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	c.Engine.CatchAll = func() (claim.Claim, bool) { return claim.Claim{ProjectID: "team"}, true }
	c.Engine.Resolve = func(claim.Claim) (relay.Policy, error) {
		return relay.Policy{Endpoint: upstream.URL, Key: "revoked"}, nil
	}
	install := make(chan struct{})
	c.Updater = &Updater{Every: time.Hour, After: once(), Jitter: func(time.Duration) time.Duration { return 0 },
		Update: func(ctx context.Context) (Pass, error) {
			select {
			case <-install:
				return Pass{Installed: "1.3.0"}, nil
			case <-ctx.Done():
				return Pass{}, ctx.Err()
			}
		}}
	r := startRun(t, c)
	r.await(t, "the relay")
	postSessionlessSpans(t, c.Addr, token, 5)
	ahead.Store(int64(relay.DefaultTraceHold + time.Minute)) // the held spans go to the catch-all
	for deadline := time.Now().Add(10 * time.Second); refused.Load() == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the catch-all route never tried to deliver")
		}
	}
	close(install)
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay kept running the release it replaced while a route kept failing")
	}
	if !r.res.Updated || !r.res.Restart() || r.res.Installed != "1.3.0" {
		t.Fatalf("Run = %+v, want Updated to 1.3.0 and a restart", r.res)
	}
}

// A relay that installed a newer terma stops waiting for its agents to pause after
// updatePauseWait, but never restarts while it holds anything: it goes once its hold empties.
func TestAnUpdatedRelayNeverDropsWhatItHoldsAfterThePauseWait(t *testing.T) {
	was := updatePauseWait
	updatePauseWait = time.Second
	t.Cleanup(func() { updatePauseWait = was })
	stateDir, dir, token := setUpRelay(t)
	c := runConfig(stateDir, 0, nil)
	c.Addr, c.Version, c.Service, c.Updater = freeAddr(t), "1.2.0", true, installsAtOnce()
	var ahead atomic.Int64
	c.Engine.Now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	r := startRun(t, c)
	r.await(t, "the relay")
	postSessionlessSpans(t, c.Addr, token, 5)
	select {
	case <-r.done:
		t.Fatalf("the relay restarted while it held, past the pause wait: %+v", r.res)
	case <-time.After(updatePauseWait + 2*time.Second):
	}
	ahead.Store(int64(relay.DefaultTraceHold + time.Minute)) // the held spans age out
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay kept running once its hold was empty")
	}
	if !r.res.Updated {
		t.Fatalf("Run = %+v, want Updated", r.res)
	}
	if got := counters(t, filepath.Join(dir, StatsFile))["dropped.no_session_trace_at_exit.traces"]; got != 0 {
		t.Fatalf("the restart dropped %d held spans", got)
	}
}

// A relay that installs another release while it still waits for its agents to pause waits
// no longer for it: updatePauseWait runs from the first release it installed.
func TestALaterInstallDoesNotStartTheWaitForAPauseAgain(t *testing.T) {
	was := updatePauseWait
	updatePauseWait = 2 * time.Second
	t.Cleanup(func() { updatePauseWait = was })
	stateDir, _, _ := setUpRelay(t)
	c := runConfig(stateDir, 0, nil)
	c.Version, c.Service = "1.2.0", true
	// A release every 200ms, ten in the wait: were each to start it again, it would never
	// end. The relay started moments ago, so nothing has paused for updateQuiet yet.
	var installs atomic.Int64
	c.Updater = &Updater{Every: 200 * time.Millisecond, Jitter: func(time.Duration) time.Duration { return 0 },
		Update: func(context.Context) (Pass, error) {
			return Pass{Installed: fmt.Sprintf("1.3.%d", installs.Add(1))}, nil
		}}
	r := startRun(t, c)
	r.await(t, "the relay")
	select {
	case <-r.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("after %d installs the relay still waits for a pause", installs.Load())
	}
	if !r.res.Updated || installs.Load() < 2 {
		t.Fatalf("Run = %+v after %d installs, want Updated after several", r.res, installs.Load())
	}
}
