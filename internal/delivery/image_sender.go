package delivery

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// ImageSender is the relay's image lane: a loop that sleeps until woken, then delivers what
// the image queue holds (FlushImages's pass), one image at a time, each request in its own
// time and no pass's. Three things wake it, none of them a poll: the relay's start; an
// export the relay accepted (Nudge), which is how a turn whose hook just queued an image
// ends; and a deadline: the earliest retry window's end, or the next look at images held
// for what they need (a key, a policy). A nudge costs one stat of the queue: no pass runs
// unless the queue changed since the last pass read it, or a deadline has passed.
type ImageSender struct {
	router Router
	events *spool.Spool
	config func() (*config.Config, error)
	// Logf, when set, is told of a pass that could not deliver everything it tried.
	Logf func(format string, args ...any)

	wake    chan struct{}
	state   atomic.Int32
	halting atomic.Bool
	passes  atomic.Int64
	loops   atomic.Int64
	// after is the project the last pass started with; read the queue as it read it.
	after string
	read  spool.Stamp
	// timing is when the sender wakes by itself and whether a wake runs a pass: its whole
	// timing policy, worked out by timing.wake and timing.after alone.
	timing timing
	// heldMin and heldMax bound the wait for held images; heldMin is also the least a
	// deadline-started pass that sent nothing waits before the next.
	heldMin, heldMax time.Duration
	// lastErr is the error last logged, when.
	lastErr    string
	lastLogged time.Time
	// clock and timer are time.Now and time.NewTimer, but for tests.
	clock func() time.Time
	timer func(d time.Duration) (<-chan time.Time, func() bool)
}

// The sender's states. It moves between idle and uploading around each image, and from idle
// to stopped (TryHalt), for good; each move is a compare-and-swap, so a halt never lands
// mid-upload and an upload never starts once halted.
const (
	senderIdle int32 = iota
	senderUploading
	senderStopped
)

// NewImageSender sends r.Images under the profile config reads at each pass, since keys and
// hosts change while the relay runs. events is the events' spool, read before stored bytes
// are swept (FlushImages).
func NewImageSender(r Router, events *spool.Spool, config func() (*config.Config, error)) *ImageSender {
	return &ImageSender{router: r, events: events, config: config, wake: make(chan struct{}, 1),
		heldMin: 30 * time.Second, heldMax: time.Hour, clock: time.Now,
		timer: func(d time.Duration) (<-chan time.Time, func() bool) {
			t := time.NewTimer(d)
			return t.C, t.Stop
		}}
}

// Nudge wakes the sender, without waiting: the relay accepted an export.
func (s *ImageSender) Nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// TryHalt stops the sender for good if it is between two images, and reports whether it
// is stopped: the relay asks it last before an idle exit, a restart or making way, and
// waits while an image is on its way. A final halt (a restart, making way: the relay goes
// whatever happens) also has the sender start no further image, so it stops at the next
// image's end; an idle exit's is not, since a new export may yet keep the relay.
func (s *ImageSender) TryHalt(final bool) bool {
	if final {
		s.halting.Store(true)
	}
	return s.state.CompareAndSwap(senderIdle, senderStopped) || s.state.Load() == senderStopped
}

// uploading reports whether an image is on its way.
func (s *ImageSender) uploading() bool { return s.state.Load() == senderUploading }

// begin moves the sender to uploading before an image, unless it was halted, or a final
// halt was asked, which stops it now.
func (s *ImageSender) begin() bool {
	if s.halting.Load() {
		s.state.CompareAndSwap(senderIdle, senderStopped)
		return false
	}
	return s.state.CompareAndSwap(senderIdle, senderUploading)
}

// end moves it back once the image is done.
func (s *ImageSender) end() { s.state.CompareAndSwap(senderUploading, senderIdle) }

// Run delivers the queue at once, then each time it is woken, until ctx ends or it is
// halted. An upload in flight as ctx ends is cut off, and its record stays queued.
func (s *ImageSender) Run(ctx context.Context) {
	if s.router.Images == nil {
		return
	}
	s.pass(ctx, false)
	for {
		s.loops.Add(1)
		if s.state.Load() == senderStopped {
			// Halted for good: nothing more to wake for.
			<-ctx.Done()
			return
		}
		var fired <-chan time.Time
		stop := func() bool { return false }
		if at := s.timing.wakeAt(); !at.IsZero() {
			fired, stop = s.timer(at.Sub(s.clock()))
		}
		cause := wokeByTimer
		select {
		case <-ctx.Done():
		case <-s.wake:
			cause = wokeByNudge
		case <-fired:
		}
		stop()
		if ctx.Err() != nil {
			return
		}
		changed := cause == wokeByNudge && !s.router.Images.Stamp().Same(s.read)
		if run, forDeadline := s.timing.wake(cause, changed, s.clock()); run {
			s.pass(ctx, forDeadline)
		}
	}
}

// pass delivers the queue once, then notes what the next wake compares against: the queue
// as the pass read it, so whatever was queued or settled since, by a hook or by the pass
// itself, makes the next nudge run another pass; and its timing (timing.after). A pass that
// could not read the profile or the queue notes the queue as it is now, so nudges wait for
// it to change, or for the look heldMin on.
func (s *ImageSender) pass(ctx context.Context, forDeadline bool) {
	defer s.passes.Add(1)
	out := passOutcome{heldLook: s.timing.due.heldPassed(s.clock())}
	sent := false
	if cfg, err := s.config(); err != nil {
		s.logError(err)
		out.unread = true
	} else {
		first := s.after
		res := s.router.flushImages(ctx, s.events, cfg, false,
			imageTurns{after: s.after, first: &first, read: &s.read, begin: s.begin, end: s.end, now: s.clock})
		s.after = first
		out.unread, out.held, out.window = res.Unread, res.Held > 0, res.nextWindow()
		sent = res.Sent > 0
		if res.Err != nil && ctx.Err() == nil {
			s.logError(res.Err)
		}
	}
	if out.unread {
		s.read = s.router.Images.Stamp()
	}
	s.timing = s.timing.after(out, forDeadline, sent, s.clock(), s.heldMin, s.heldMax)
}

// timing is the sender's timing policy's state: its deadlines, and notBefore, the earliest
// a deadline may start a pass once one a deadline started sent nothing.
type timing struct {
	due       deadlines
	notBefore time.Time
}

// wakeCause is what woke the sender.
type wakeCause int

const (
	// wokeByNudge is an export the relay accepted; wokeByTimer the timer wakeAt armed.
	wokeByNudge wakeCause = iota
	wokeByTimer
)

// wake decides whether a wake runs a pass, and whether that pass is a deadline's. The timer
// is armed at wakeAt, so its firing runs one, for the deadline. A nudge runs one when the
// queue changed since the last pass read it; otherwise only for a deadline that has come,
// and not while notBefore holds deadlines off.
func (t timing) wake(cause wakeCause, queueChanged bool, now time.Time) (run, forDeadline bool) {
	switch {
	case cause == wokeByTimer:
		return true, true
	case queueChanged:
		return true, false
	case t.due.passed(now) && !now.Before(t.notBefore):
		return true, true
	}
	return false, false
}

// wakeAt is when the sender wakes by itself: its next deadline, no earlier than notBefore.
func (t timing) wakeAt() time.Time {
	at := t.due.next()
	if !at.IsZero() && at.Before(t.notBefore) {
		at = t.notBefore
	}
	return at
}

// after is the timing once a pass ended at now with out: the deadlines nextDeadlines
// works out, and notBefore, which only a pass that sent something clears, and which a
// deadline's pass that sent nothing sets heldMin on: whatever is wrong is retried slowly,
// never in a loop.
func (t timing) after(out passOutcome, forDeadline, sent bool, now time.Time, heldMin, heldMax time.Duration) timing {
	next := timing{due: nextDeadlines(t.due, out, now, heldMin, heldMax), notBefore: t.notBefore}
	switch {
	case sent:
		next.notBefore = time.Time{}
	case forDeadline:
		next.notBefore = now.Add(heldMin)
	}
	return next
}

// deadlines are when the sender wakes by itself: a retry window's end, and the next look
// at images held for what they need (a key, a policy), which waits held.
type deadlines struct {
	windowEnds, heldDue time.Time
	held                time.Duration
}

// next is the earlier deadline, zero for none.
func (d deadlines) next() time.Time {
	switch {
	case d.windowEnds.IsZero():
		return d.heldDue
	case d.heldDue.IsZero() || d.windowEnds.Before(d.heldDue):
		return d.windowEnds
	}
	return d.heldDue
}

// passed reports whether a deadline has come at now.
func (d deadlines) passed(now time.Time) bool {
	next := d.next()
	return !next.IsZero() && !now.Before(next)
}

// heldPassed reports whether the look at held images has come at now.
func (d deadlines) heldPassed(now time.Time) bool {
	return !d.heldDue.IsZero() && !now.Before(d.heldDue)
}

// passOutcome is what a pass learned for the deadlines.
type passOutcome struct {
	// unread is a pass that could not read the profile or the queue, and did nothing.
	unread bool
	// window is the earliest end of the windows it waited out or opened (nextWindow).
	window time.Time
	// held says images were held for what they need; heldLook that the held images'
	// deadline came as the pass started.
	held, heldLook bool
}

// nextDeadlines is the sender's deadlines after a pass that ended at now with out, from
// prev: the one place they are worked out. A deadline already past is never kept. A pass
// that read nothing keeps the deadlines still ahead and looks again within heldMin, nothing
// telling it when the profile or the queue can be read. Otherwise the window's end is the
// pass's own, due at once if it ended while the pass ran; the held images' wait starts at
// heldMin and doubles, to heldMax, only when their own look found them still held.
func nextDeadlines(prev deadlines, out passOutcome, now time.Time, heldMin, heldMax time.Duration) deadlines {
	ahead := func(t time.Time) time.Time {
		if t.After(now) {
			return t
		}
		return time.Time{}
	}
	if out.unread {
		d := deadlines{windowEnds: ahead(prev.windowEnds), heldDue: ahead(prev.heldDue), held: prev.held}
		if look := now.Add(heldMin); d.heldDue.IsZero() || look.Before(d.heldDue) {
			d.heldDue = look
		}
		return d
	}
	d := deadlines{windowEnds: out.window}
	if !d.windowEnds.IsZero() && !d.windowEnds.After(now) {
		d.windowEnds = now
	}
	switch {
	case !out.held:
		return d
	case prev.held == 0:
		d.held = heldMin
	case out.heldLook:
		d.held = min(2*prev.held, heldMax)
	default:
		d.held = prev.held
	}
	d.heldDue = now.Add(d.held)
	return d
}

// errorLogEvery is how often the sender logs an error it already logged.
const errorLogEvery = 10 * time.Minute

// logError tells Logf of err, once while it repeats, again every errorLogEvery.
func (s *ImageSender) logError(err error) {
	msg := err.Error()
	if s.Logf == nil || msg == s.lastErr && s.clock().Sub(s.lastLogged) < errorLogEvery {
		return
	}
	s.lastErr, s.lastLogged = msg, s.clock()
	s.Logf("image sender: %s", msg)
}
