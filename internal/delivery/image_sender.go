package delivery

import (
	"context"
	"os"
	"slices"
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
	// after is the project the last pass started with; read the queue as it read it, and
	// seen each watched file as it stood then.
	after   string
	read    spool.Stamp
	watched []string
	seen    []fileMark
	// timing is when the sender wakes by itself and whether a wake runs a pass: its whole
	// timing policy, worked out by timing.wake and timing.after alone.
	timing timing
	// heldMin and heldMax bound the wait for held images; heldMin is also the least a
	// deadline-started pass that sent nothing waits before the next.
	heldMin, heldMax time.Duration
	// pruned is when a pass last pruned the image store.
	pruned time.Time
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

// Resume undoes a halt the relay asked for and then did not stop for (an export came): the
// sender starts images again and runs a pass. It must not block: the relay calls it under
// its lock.
func (s *ImageSender) Resume() {
	s.halting.Store(false)
	s.state.CompareAndSwap(senderStopped, senderIdle)
	s.Nudge()
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
			// Halted: only a resume (Resume, then a nudge) or the end wakes it, no deadline.
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
			if s.state.Load() != senderStopped {
				s.pass(ctx, false)
			}
			continue
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
		changed := cause == wokeByNudge && (!s.router.Images.Stamp().Same(s.read) || !slices.Equal(marks(s.watched), s.seen))
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
	s.seen = marks(s.watched)
	out := passOutcome{heldLook: s.timing.due.heldPassed(s.clock())}
	sent := false
	if cfg, err := s.config(); err != nil {
		s.logError(err)
		out.unread = true
	} else {
		prune := s.clock().Sub(s.pruned) >= imagePruneEvery
		if prune {
			s.pruned = s.clock()
		}
		res := s.router.flushImages(ctx, s.events, cfg, false, imageTurns{after: s.after, gate: s, now: s.clock, prune: prune})
		s.read = res.read
		if res.first != "" {
			s.after = res.first
		}
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

// Watch has a nudge run a pass when one of paths changed since the last, as when the queue
// did: the keys and the policies, which images held for a key or a policy wait for, so they
// go soon after `terma setup` stores them rather than at their next look. One stat each.
func (s *ImageSender) Watch(paths ...string) { s.watched = paths }

// fileMark is a file or folder as one stat finds it: gone, or its size and modification
// time. A file rewritten by an atomic rename, or a folder something was renamed into, has
// another.
type fileMark struct {
	exists bool
	size   int64
	mod    int64
}

// marks are paths as they stand.
func marks(paths []string) []fileMark {
	out := make([]fileMark, len(paths))
	for i, p := range paths {
		if info, err := os.Stat(p); err == nil {
			out[i] = fileMark{exists: true, size: info.Size(), mod: info.ModTime().UnixNano()}
		}
	}
	return out
}

// imagePruneEvery is how often the sender prunes the image store of what outlived every
// record: a listing of the folder, which a pass need not repeat.
const imagePruneEvery = time.Hour

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
