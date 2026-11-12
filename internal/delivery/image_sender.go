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

	wake   chan struct{}
	busy   atomic.Bool
	passes atomic.Int64
	// after is the project the last pass started with; read the queue as it read it; due
	// the next deadline, zero for none.
	after string
	read  spool.Stamp
	due   time.Time
	// held is how long images held for what they need wait for the next look: heldMin,
	// doubling to heldMax while they stay held.
	held             time.Duration
	heldMin, heldMax time.Duration
}

// NewImageSender sends r.Images under the profile config reads at each pass, since keys and
// hosts change while the relay runs. events is the events' spool, read before stored bytes
// are swept (FlushImages).
func NewImageSender(r Router, events *spool.Spool, config func() (*config.Config, error)) *ImageSender {
	return &ImageSender{router: r, events: events, config: config, wake: make(chan struct{}, 1),
		heldMin: 30 * time.Second, heldMax: time.Hour}
}

// Nudge wakes the sender, without waiting: the relay accepted an export.
func (s *ImageSender) Nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Busy reports whether an image is being uploaded, which an idle exit or a quiesce waits for.
func (s *ImageSender) Busy() bool { return s.busy.Load() }

// Run delivers the queue at once, then each time it is woken, until ctx ends. An upload in
// flight then is cut off, and its record stays queued.
func (s *ImageSender) Run(ctx context.Context) {
	if s.router.Images == nil {
		return
	}
	s.pass(ctx)
	for {
		var windowEnds <-chan time.Time
		var timer *time.Timer
		if !s.due.IsZero() {
			timer = time.NewTimer(time.Until(s.due))
			windowEnds = timer.C
		}
		woken := false
		select {
		case <-ctx.Done():
		case <-s.wake:
			woken = true
		case <-windowEnds:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
		if woken && s.router.Images.Stamp().Same(s.read) && (s.due.IsZero() || time.Now().Before(s.due)) {
			continue
		}
		s.pass(ctx)
	}
}

// pass delivers the queue once, then notes what the next wake compares against: the queue
// as the pass read it, so whatever was queued or settled since, by a hook or by the pass
// itself, makes the next nudge run another pass.
func (s *ImageSender) pass(ctx context.Context) {
	defer s.passes.Add(1)
	held := false
	if cfg, err := s.config(); err != nil {
		s.logf("image sender: %v", err)
		s.read = spool.Stamp{}
	} else {
		first := s.after
		res := s.router.flushImages(ctx, s.events, cfg, false,
			imageTurns{after: s.after, first: &first, read: &s.read, uploading: s.busy.Store})
		s.after = first
		held = res.Held > 0
		if res.Err != nil && ctx.Err() == nil {
			s.logf("image sender: %v", res.Err)
		}
	}
	now := time.Now()
	s.due = time.Time{}
	if held {
		// Nothing tells the sender a key or a policy arrived: it looks again, less often.
		s.held = min(max(2*s.held, s.heldMin), s.heldMax)
		s.due = now.Add(s.held)
	} else {
		s.held = 0
	}
	for _, end := range s.router.Images.RetryWindows(now) {
		if s.due.IsZero() || end.Before(s.due) {
			s.due = end
		}
	}
}

func (s *ImageSender) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}
