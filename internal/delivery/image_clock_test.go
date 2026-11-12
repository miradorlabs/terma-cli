package delivery

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// fakeClock is a clock a test moves, with timers that fire as it passes them.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at   time.Time
	c    chan time.Time
	done bool
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Timer(d time.Duration) (<-chan time.Time, func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), c: make(chan time.Time, 1)}
	if d <= 0 {
		t.done = true
		t.c <- c.now
	} else {
		c.timers = append(c.timers, t)
	}
	return t.c, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.done
		t.done = true
		return was
	}
}

// armed is how many timers wait.
func (c *fakeClock) armed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.done {
			n++
		}
	}
	return n
}

// Advance moves the clock on by d, firing each timer it passes.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		if !t.done && !t.at.After(c.now) {
			t.done = true
			t.c <- c.now
		}
	}
}

// onClock has s keep time by c.
func onClock(s *ImageSender, c *fakeClock) *ImageSender {
	s.clock, s.timer = c.Now, c.Timer
	return s
}

// runSender runs s until the test ends.
func runSender(t *testing.T, s *ImageSender) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// The sender's deadlines, worked out in one place: a deadline already past is never kept;
// a pass that read nothing keeps those ahead and looks again within heldMin; the window's
// end is the pass's own, due at once if it ended during the pass; the held images' wait
// doubles only on its own look.
func TestNextDeadlines(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	const minWait, maxWait = 30 * time.Second, time.Hour
	past, soon, later := now.Add(-time.Minute), now.Add(10*time.Second), now.Add(time.Hour)
	for _, c := range []struct {
		name string
		prev deadlines
		out  passOutcome
		want deadlines
	}{
		{"nothing", deadlines{}, passOutcome{}, deadlines{}},
		{"a window ahead", deadlines{}, passOutcome{window: later}, deadlines{windowEnds: later}},
		{"a window that ended during the pass", deadlines{}, passOutcome{window: past}, deadlines{windowEnds: now}},
		{"the pass's window replaces the last", deadlines{windowEnds: soon}, passOutcome{window: later}, deadlines{windowEnds: later}},
		{"first held", deadlines{}, passOutcome{held: true}, deadlines{held: minWait, heldDue: now.Add(minWait)}},
		{"held on a nudge", deadlines{held: minWait, heldDue: soon}, passOutcome{held: true}, deadlines{held: minWait, heldDue: now.Add(minWait)}},
		{"held on its own look", deadlines{held: minWait, heldDue: past}, passOutcome{held: true, heldLook: true}, deadlines{held: 2 * minWait, heldDue: now.Add(2 * minWait)}},
		{"held at the cap", deadlines{held: maxWait}, passOutcome{held: true, heldLook: true}, deadlines{held: maxWait, heldDue: now.Add(maxWait)}},
		{"no longer held", deadlines{held: minWait, heldDue: soon}, passOutcome{}, deadlines{}},
		{"unread keeps what is ahead", deadlines{windowEnds: later, heldDue: later, held: maxWait}, passOutcome{unread: true},
			deadlines{windowEnds: later, heldDue: now.Add(minWait), held: maxWait}},
		{"unread keeps an earlier look", deadlines{heldDue: soon, held: minWait}, passOutcome{unread: true}, deadlines{heldDue: soon, held: minWait}},
		{"unread drops what is past", deadlines{windowEnds: past, heldDue: past, held: minWait}, passOutcome{unread: true},
			deadlines{heldDue: now.Add(minWait), held: minWait}},
		{"unread does not double the wait", deadlines{heldDue: past, held: minWait}, passOutcome{unread: true, heldLook: true},
			deadlines{heldDue: now.Add(minWait), held: minWait}},
	} {
		got := nextDeadlines(c.prev, c.out, now, minWait, maxWait)
		if !got.windowEnds.Equal(c.want.windowEnds) || !got.heldDue.Equal(c.want.heldDue) || got.held != c.want.held {
			t.Errorf("%s: nextDeadlines = %+v, want %+v", c.name, got, c.want)
		}
		if next := got.next(); !next.IsZero() && next.Before(now) {
			t.Errorf("%s: kept a deadline already past: %v", c.name, next)
		}
	}
}

// A profile that cannot be read never makes the sender spin: its passes stay a heldMin
// apart, though a deadline was already past when it started.
func TestAnUnreadableProfileDoesNotSpin(t *testing.T) {
	t.Parallel()
	r, _, _ := imageFixture(t, &recorder{}, true)
	var logged atomic.Int64
	s := NewImageSender(r, openSpool(t), func() (*config.Config, error) { return nil, errors.New("config.json: unreadable") })
	s.Logf = func(string, ...any) { logged.Add(1) }
	s.heldMin = 20 * time.Millisecond
	s.timing.due.windowEnds = time.Now().Add(-time.Second)
	runSender(t, s)
	time.Sleep(500 * time.Millisecond)
	if n := s.passes.Load(); n > 30 {
		t.Fatalf("%d passes in 500 ms with an unreadable profile", n)
	}
	if n := logged.Load(); n != 1 {
		t.Fatalf("the same error was logged %d times", n)
	}
}

// A pass its deadline started that sent nothing has the next deadline wait heldMin at
// least, whatever the deadlines say: a bug that left one past is a slow retry, not a loop.
func TestADeadlinePassThatSendsNothingWaits(t *testing.T) {
	t.Parallel()
	r, cfg, _ := imageFixture(t, &recorder{}, true)
	clk := newFakeClock()
	s := onClock(NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil }), clk)
	s.pass(t.Context(), true)
	s.timing.due.windowEnds = clk.Now().Add(-time.Second) // as a bug would leave it
	if at := s.timing.wakeAt(); at.Before(clk.Now().Add(s.heldMin)) {
		t.Fatalf("the next deadline pass may start at %v, before heldMin from now", at)
	}
	guard := s.timing.notBefore
	s.pass(t.Context(), false) // a nudge's, sending nothing: the guard stands as it was
	if !s.timing.notBefore.Equal(guard) {
		t.Fatalf("a nudged pass that sent nothing moved the guard from %v to %v", guard, s.timing.notBefore)
	}
}

// A retry window's end wakes the sender, with no export and no poll.
func TestAWindowsEndWakesTheSender(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	clk := newFakeClock()
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	r.Images.DestinationFailed("p1", clk.Now().Add(-29*time.Second)) // ends within a second
	s := onClock(NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil }), clk)
	runSender(t, s)
	waitFor(t, "the window's timer", func() bool { return clk.armed() == 1 })
	if len(rec.requests()) != 0 {
		t.Fatal("the sender sent into an open window")
	}
	clk.Advance(2 * time.Second)
	waitFor(t, "the window's end", func() bool { return len(rec.requests()) == 1 })
}

// A window that ends while the pass that deferred to it still runs wakes the sender all
// the same: its end comes from the pass, not from the windows still open afterwards.
func TestAWindowEndingMidPassWakesTheSender(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	clk := newFakeClock()
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	r.Images.DestinationFailed("p1", clk.Now().Add(-29*time.Second)) // ends within a second
	policy := r.Policy
	var lookups atomic.Int64
	r.Policy = func(ctx context.Context, cfg *config.Config, team string) (config.Policy, error) {
		if lookups.Add(1) == 1 {
			clk.Advance(2 * time.Second) // the first lookup outlasts the window
		}
		return policy(ctx, cfg, team)
	}
	runSender(t, onClock(NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil }), clk))
	waitFor(t, "the image, with no nudge", func() bool { return len(rec.requests()) == 1 })
}

// Images held for a key have a deadline of their own: once the key is there, they go with
// no new image and no export.
func TestHeldImagesAreLookedAtAgain(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	r, cfg, stateDir := imageFixture(t, rec, true)
	clk := newFakeClock()
	img := pixelN(1)
	store(t, stateDir, img)
	e := imageEvent(img)
	e.Attrs[hookrun.AttrProjectID] = "keyless"
	queue(t, r.Images, e)
	s := onClock(NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil }), clk)
	runSender(t, s)
	waitFor(t, "the held images' timer", func() bool { return clk.armed() == 1 })
	if len(rec.requests()) != 0 {
		t.Fatal("an image with no key went")
	}
	if err := keystore.Set(r.ConfigDir, "keyless", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(s.heldMin)
	waitFor(t, "the held image", func() bool { return len(rec.requests()) == 1 })
}

// A queue that cannot be read is a pass that read nothing: the sender keeps its deadlines
// and looks again within heldMin.
func TestAnUnreadableQueueIsMarked(t *testing.T) {
	t.Parallel()
	r, cfg, _ := imageFixture(t, &recorder{}, true)
	dir := t.TempDir()
	images, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r.Images = images
	// The queue's append lock is a folder: it cannot be taken, so the queue cannot be read.
	if err := os.Mkdir(filepath.Join(dir, "flush.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	if res := r.FlushImages(t.Context(), openSpool(t), cfg, false); !res.Unread || res.Err == nil {
		t.Fatalf("FlushImages = %+v", res)
	}
}

// The sender's whole timing policy: when a wake runs a pass, and what a pass leaves.
func TestTiming(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	past, ahead := now.Add(-time.Second), now.Add(time.Minute)
	type woke struct {
		cause   wakeCause
		changed bool
	}
	for _, c := range []struct {
		name             string
		t                timing
		woke             woke
		run, forDeadline bool
	}{
		{"the timer", timing{}, woke{wokeByTimer, false}, true, true},
		{"a nudge, the queue changed", timing{}, woke{wokeByNudge, true}, true, false},
		{"a nudge, the queue changed, under the guard", timing{notBefore: ahead}, woke{wokeByNudge, true}, true, false},
		{"a nudge, nothing new", timing{}, woke{wokeByNudge, false}, false, false},
		{"a nudge, nothing new, a deadline come", timing{due: deadlines{windowEnds: past}}, woke{wokeByNudge, false}, true, true},
		{"a nudge, nothing new, a deadline come, under the guard", timing{due: deadlines{windowEnds: past}, notBefore: ahead}, woke{wokeByNudge, false}, false, false},
		{"a nudge, nothing new, a deadline ahead", timing{due: deadlines{heldDue: ahead}}, woke{wokeByNudge, false}, false, false},
	} {
		run, forDeadline := c.t.wake(c.woke.cause, c.woke.changed, now)
		if run != c.run || forDeadline != c.forDeadline {
			t.Errorf("%s: wake = %v, %v; want %v, %v", c.name, run, forDeadline, c.run, c.forDeadline)
		}
	}
	if at := (timing{due: deadlines{windowEnds: past}, notBefore: ahead}).wakeAt(); !at.Equal(ahead) {
		t.Errorf("wakeAt under the guard = %v, want %v", at, ahead)
	}
	const minWait = 30 * time.Second
	for _, c := range []struct {
		name              string
		t                 timing
		forDeadline, sent bool
		want              time.Time
	}{
		{"a deadline's pass that sent nothing sets the guard", timing{}, true, false, now.Add(minWait)},
		{"a deadline's pass that sent something clears it", timing{notBefore: ahead}, true, true, time.Time{}},
		{"a nudge's pass that sent something clears it", timing{notBefore: ahead}, false, true, time.Time{}},
		{"a nudge's pass that sent nothing leaves it", timing{notBefore: ahead}, false, false, ahead},
	} {
		if got := c.t.after(passOutcome{}, c.forDeadline, c.sent, now, minWait, time.Hour).notBefore; !got.Equal(c.want) {
			t.Errorf("%s: notBefore = %v, want %v", c.name, got, c.want)
		}
	}
}

// A pass that could not read the profile notes the queue as it is, so nudges with nothing
// new run no pass until the look heldMin on.
func TestAnUnreadPassNotesTheQueue(t *testing.T) {
	t.Parallel()
	r, _, _ := imageFixture(t, &recorder{}, true)
	queue(t, r.Images, imageEvent(pixelN(1)))
	s := NewImageSender(r, openSpool(t), func() (*config.Config, error) { return nil, errors.New("config.json: unreadable") })
	s.pass(t.Context(), false)
	if !s.read.Same(r.Images.Stamp()) {
		t.Fatal("an unread pass left the queue unnoted: every nudge would run another")
	}
}

// A nudge while the guard holds deadlines off, with nothing new queued, runs no pass,
// though a deadline has come: the guard holds for nudges as for the timer.
func TestANudgeUnderTheGuardRunsNoPass(t *testing.T) {
	t.Parallel()
	rec := &recorder{status: http.StatusServiceUnavailable, refuse: func(string) bool { return true }}
	r, cfg, stateDir := imageFixture(t, rec, true)
	if err := keystore.Set(r.ConfigDir, "p2", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	clk := newFakeClock()
	img := pixelN(1)
	store(t, stateDir, img)
	theirs := imageEvent(img)
	theirs.Attrs[hookrun.AttrProjectID] = "p2"
	held := imageEvent(pixelN(2))
	held.Attrs[hookrun.AttrProjectID] = "keyless"
	queue(t, r.Images, theirs, held)
	r.Images.DestinationFailed("p2", clk.Now().Add(-29*time.Second)) // ends within a second
	s := onClock(NewImageSender(r, openSpool(t), func() (*config.Config, error) { return cfg, nil }), clk)
	s.heldMin = 2 * time.Minute // a guard longer than p2's next window
	runSender(t, s)
	waitFor(t, "the first pass's timer", func() bool { return clk.armed() == 1 })
	// The window ends; the host is between deploys (503): p2's image keeps its place, the
	// queue is unchanged, a window of a minute opens, and the guard of two.
	clk.Advance(2 * time.Second)
	waitFor(t, "the deadline's pass", func() bool { return s.passes.Load() == 2 && clk.armed() == 1 })
	clk.Advance(80 * time.Second) // p2's window is over; the guard is not
	passes, loops := s.passes.Load(), s.loops.Load()
	s.Nudge()
	waitFor(t, "the nudge", func() bool { return s.loops.Load() > loops })
	if n := s.passes.Load(); n != passes {
		t.Fatalf("a nudge under the guard, with nothing new, ran %d passes", n-passes)
	}
}

// A pass keeps one clock: the windows it opens start at its now, so a fake clock drives
// their ends.
func TestAPassKeepsItsOwnClock(t *testing.T) {
	t.Parallel()
	rec := &recorder{refuse: func(string) bool { return true }} // a 500
	r, cfg, stateDir := imageFixture(t, rec, true)
	img := pixelN(1)
	store(t, stateDir, img)
	queue(t, r.Images, imageEvent(img))
	then := time.Now().Add(-time.Hour)
	res := r.flushImages(t.Context(), openSpool(t), cfg, false, imageTurns{now: func() time.Time { return then }})
	if len(res.Failures) != 1 {
		t.Fatalf("flushImages = %+v", res)
	}
	if at := res.Failures[0].RetryAt; at.After(then.Add(time.Minute)) {
		t.Fatalf("the window ends at %v, by another clock than the pass's %v", at, then)
	}
}
