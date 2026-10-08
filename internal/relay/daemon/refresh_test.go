package daemon

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// clock is a PolicyRefresher's time, advanced by the test.
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

// A team with no validated policy is fetched at once, then once per Interval.
func TestRefresherFetchesEachTeamOncePerInterval(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	r := startRefresher(t, func() []string { return []string{"a"} }, none, func(context.Context, string) error { return errors.New("refused") })
	r.fetched(t, "a")
	for range 6 {
		r.clock.step(t, 5*time.Second)
	}
	r.fetched(t)
	r.clock.step(t, time.Minute)
	r.fetched(t, "a")
}

// A team connected while another's fetch hangs is fetched at once; the hanging one is not fetched twice.
func TestRefresherDiscoversATeamWhileAFetchHangs(t *testing.T) {
	t.Parallel()
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

// A running relay starts a fetch of each team's policy at most 35 seconds after the last
// one started, however long the fetches take: the next is due Interval after the last
// started, at the first discovery from then.
func TestARunningRelayFetchesThePolicyEveryHalfMinute(t *testing.T) {
	t.Parallel()
	const latency = 9 * time.Second
	c := newClock()
	var mu sync.Mutex
	var starts []time.Time
	var late time.Duration // how far fetches moved the clock past the last discovery
	p := Deps{}.Refresher()
	p.Teams = func() []string { return []string{"a"} }
	p.Fetched = none
	p.Now, p.After = c.Now, c.After
	p.Refresh = func(context.Context, string) error {
		c.mu.Lock()
		start := c.now
		c.now = c.now.Add(latency)
		c.mu.Unlock()
		mu.Lock()
		starts, late = append(starts, start), late+latency
		mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	<-c.waiting
	// Each fetch runs in its own goroutine; the next discovery must see it finished.
	settle := func() {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			p.mu.Lock()
			busy := p.inFlight["a"]
			p.mu.Unlock()
			if !busy {
				return
			}
		}
		t.Fatal("a fetch never finished")
	}
	settle()
	for range 3 * time.Minute / p.Discover {
		// Discoveries keep their own beat, whatever the fetches took.
		mu.Lock()
		d := p.Discover - late
		late = 0
		mu.Unlock()
		c.step(t, d)
		settle()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) < 2 {
		t.Fatalf("fetched %d times in three minutes", len(starts))
	}
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(starts[i-1]); gap > 35*time.Second {
			t.Fatalf("fetch %d started %v after the one before; want at most 35s", i, gap)
		}
	}
}
