package spool

import "time"

// The queue's per-destination retry windows, as a Spool's.

// RetryAt is Spool.RetryAt.
func (q *Queue) RetryAt(dest string) time.Time { return q.s.RetryAt(dest) }

// DestinationFailed is Spool.DestinationFailed.
func (q *Queue) DestinationFailed(dest string, now time.Time) time.Time {
	return q.s.DestinationFailed(dest, now)
}

// DestinationFailedUntil is Spool.DestinationFailedUntil.
func (q *Queue) DestinationFailedUntil(dest string, now, until time.Time) time.Time {
	return q.s.DestinationFailedUntil(dest, now, until)
}

// DestinationStalled is Spool.DestinationStalled.
func (q *Queue) DestinationStalled(dest string, now time.Time) time.Time {
	return q.s.DestinationStalled(dest, now)
}

// DestinationDelivered is Spool.DestinationDelivered.
func (q *Queue) DestinationDelivered(dest string, now time.Time) { q.s.DestinationDelivered(dest, now) }

// RetryWindows is Spool.RetryWindows.
func (q *Queue) RetryWindows(now time.Time) map[string]time.Time { return q.s.RetryWindows(now) }

// StalledWindows is Spool.StalledWindows.
func (q *Queue) StalledWindows(now time.Time) map[string]time.Time { return q.s.StalledWindows(now) }
