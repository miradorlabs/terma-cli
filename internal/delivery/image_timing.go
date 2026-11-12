package delivery

import "time"

// The relay's image sender's timing policy, pure: when a wake runs a pass (timing.wake),
// and the deadlines and guard a pass leaves (timing.after, nextDeadlines). ImageSender keeps
// the state and the clock; everything it decides about time is decided here.

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
	// unread is the wait before the next look at a profile or queue that could not be read,
	// doubling while it stays so.
	unread time.Duration
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
// that read nothing keeps the deadlines still ahead and looks again, nothing telling it when
// the profile or the queue can be read: heldMin on, doubling to heldMax while each look
// reads nothing, as for held images. Otherwise the window's end is the
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
		d := deadlines{windowEnds: ahead(prev.windowEnds), heldDue: ahead(prev.heldDue), held: prev.held,
			unread: min(max(2*prev.unread, heldMin), heldMax)}
		if look := now.Add(d.unread); d.heldDue.IsZero() || look.Before(d.heldDue) {
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
