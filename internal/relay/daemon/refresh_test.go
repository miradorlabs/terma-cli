package daemon

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// clock is a PolicyRefresher's time, advanced by the test: each discovery waits on
// ticks, and says so on waiting.
type clock struct {
	mu      sync.Mutex
	now     time.Time
	ticks   chan time.Time
	waiting chan struct{}
}

func newClock() *clock {
	return &clock{now: time.Unix(1_800_000_000, 0), ticks: make(chan time.Time), waiting: make(chan struct{}, 16)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) After(time.Duration) <-chan time.Time {
	c.waiting <- struct{}{}
	return c.ticks
}

// step advances the clock and lets one discovery run, returning once it has.
func (c *clock) step(t *testing.T, d time.Duration) {
	t.Helper()
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	c.ticks <- c.now
	select {
	case <-c.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresher did not come back to wait")
	}
}

type refresher struct {
	*PolicyRefresher
	clock  *clock
	calls  chan string
	cancel context.CancelFunc
	done   chan struct{}
}

func startRefresher(t *testing.T, teams func() []string, fetched func(string) time.Time, refresh func(context.Context, string) error) *refresher {
	t.Helper()
	c := newClock()
	r := &refresher{clock: c, calls: make(chan string, 16), done: make(chan struct{})}
	r.PolicyRefresher = &PolicyRefresher{Interval: time.Minute, Discover: 5 * time.Second, Teams: teams, Fetched: fetched,
		Refresh: func(ctx context.Context, team string) error {
			r.calls <- team
			return refresh(ctx, team)
		},
		Now: c.Now, After: c.After}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.Run(ctx); close(r.done) }()
	<-c.waiting
	t.Cleanup(func() {
		cancel()
		<-r.done
	})
	return r
}

func (r *refresher) fetched(t *testing.T, want ...string) {
	t.Helper()
	for range want {
		select {
		case team := <-r.calls:
			if !contains(want, team) {
				t.Fatalf("fetched %s, want %v", team, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no fetch of %v", want)
		}
	}
	select {
	case team := <-r.calls:
		t.Fatalf("fetched %s as well", team)
	case <-time.After(20 * time.Millisecond):
	}
}

func contains(s []string, v string) bool {
	return slices.Contains(s, v)
}

func none(string) time.Time { return time.Time{} }

// A team with no validated policy is fetched at once, then once per Interval however
// often the teams are listed.
func TestRefresherFetchesEachTeamOncePerInterval(t *testing.T) {
	r := startRefresher(t, func() []string { return []string{"a"} }, none, func(context.Context, string) error { return nil })
	r.fetched(t, "a")
	for range 11 {
		r.clock.step(t, 5*time.Second)
	}
	r.fetched(t)
	r.clock.step(t, 5*time.Second)
	r.fetched(t, "a")
}

// A cached policy is not fetched again before it is Interval old.
func TestRefresherWaitsForACachedPolicyToAge(t *testing.T) {
	fetchedAt := newClock().Now().Add(-30 * time.Second)
	r := startRefresher(t, func() []string { return []string{"a"} }, func(string) time.Time { return fetchedAt },
		func(context.Context, string) error { return nil })
	r.fetched(t)
	r.clock.step(t, 25*time.Second)
	r.fetched(t)
	r.clock.step(t, 5*time.Second)
	r.fetched(t, "a")
}

// A refused fetch is not retried at every discovery: the next attempt waits Interval.
func TestRefresherThrottlesARefusedFetch(t *testing.T) {
	r := startRefresher(t, func() []string { return []string{"a"} }, none, func(context.Context, string) error { return errors.New("refused") })
	r.fetched(t, "a")
	for range 6 {
		r.clock.step(t, 5*time.Second)
	}
	r.fetched(t)
	r.clock.step(t, time.Minute)
	r.fetched(t, "a")
}

// A team connected while another's fetch hangs is fetched without waiting for it, and
// the hanging one is not fetched twice at once.
func TestRefresherDiscoversATeamWhileAFetchHangs(t *testing.T) {
	var mu sync.Mutex
	teams := []string{"a"}
	release := make(chan struct{})
	r := startRefresher(t, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), teams...)
	}, none, func(ctx context.Context, team string) error {
		if team == "a" {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil
	})
	r.fetched(t, "a")
	mu.Lock()
	teams = append(teams, "b")
	mu.Unlock()
	r.clock.step(t, time.Minute)
	r.fetched(t, "b")
	close(release)
}
