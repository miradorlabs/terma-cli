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
	// Wait: keep it queued; its team may still recover (expired, signing in, suspended,
	// or its binding still to be decided).
	Wait Verdict = "wait"
	// Discard: drop it. It is never sent to another team.
	Discard Verdict = "drop"
)

// Recheck judges a queued record against the state now: it is sent only to the team it
// was routed to, while that team is Active and its list, authority and consent still
// cover the record's location; it waits while that team may recover; anything else drops
// it, whatever another team would now collect. The caller checks the record's run against
// the run ledger first (a run-attributed record of a run Mixed by its time is dropped),
// and keeps it queued while its team's key is unavailable.
func (r *Router) Recheck(d Destination, now time.Time) (Verdict, Decision) {
	if !r.s.Ready() {
		// The migration commits it, or a relay start finishes it: until then nothing is
		// sent, and nothing queued is lost for it.
		return Wait, Decision{Outcome: Drop, Reason: ReasonNotReady}
	}
	if d.EnrollmentID == "" || !r.s.Selected(d.EnrollmentID) {
		return Discard, Decision{Outcome: Drop}
	}
	got := r.Route(d.Location, now)
	switch {
	case got.EnrollmentID != d.EnrollmentID:
		return Discard, got
	case got.Outcome == Send:
		return SendNow, got
	case got.Outcome == Hold && (got.Reason == ReasonPending || got.Blocked != Refused):
		return Wait, got
	}
	return Discard, got
}
