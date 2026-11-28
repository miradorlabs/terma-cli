package routing

import "time"

// Destination is where a queued record was routed: the enrollment Route gave it, sending or
// holding it, and the location Route judged.
type Destination struct {
	EnrollmentID string
	Location     Location
}

// Verdict is what becomes of a queued record when it is about to be sent.
type Verdict string

// Verdicts.
const (
	// SendNow: send it to its own team, filtered by that team's current content rules.
	SendNow Verdict = "send"
	// Wait: keep it queued; its team may still recover (expired, signing in or
	// suspended).
	Wait Verdict = "wait"
	// Discard: drop it. It is never sent to another team.
	Discard Verdict = "drop"
)

// Recheck judges a queued or held record against the state now: it is sent only to the
// enrollment it was routed to, while that team is Active and its list still covers the
// record's location; it waits while that team may recover; anything else drops it, whatever
// another team would now collect.
func (r *Router) Recheck(d Destination, now time.Time) (Verdict, Decision) {
	if d.EnrollmentID == "" {
		return Discard, Decision{Outcome: Drop}
	}
	t := r.s.team(d.EnrollmentID)
	if t == nil {
		return Discard, Decision{Outcome: Drop}
	}
	st := t.StateAt(now)
	if st == Refused {
		// Route passes a refused team over; what was already pinned to it is dropped as blocked.
		return Discard, Decision{Outcome: Hold, EnrollmentID: d.EnrollmentID, Reason: ReasonBlocked, Blocked: st}
	}
	got := r.Route(d.Location, now)
	if got.Outcome == Hold && got.Reason == ReasonUnknown && !d.Location.Narrow &&
		r.s.unknownToPersonalEligible() && r.s.Teams[0].EnrollmentID == d.EnrollmentID {
		// Route returns unpinned ReasonUnknown while Personal is blocked; normalize it to a
		// pinned blocking Decision so the switch below returns Wait with the blocking reason
		// preserved for doctor/counters.
		got = Decision{Outcome: Hold, EnrollmentID: d.EnrollmentID, Reason: ReasonBlocked, Blocked: st}
	}
	switch {
	case got.EnrollmentID != d.EnrollmentID:
		return Discard, got
	case got.Outcome == Send:
		return SendNow, got
	case got.Outcome == Hold:
		return Wait, got
	}
	return Discard, got
}
