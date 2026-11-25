package relay

import (
	"context"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// unreadableRetry is how long an export waits for a sign-in that can't be read to read
// again, covering a sign-in another process is writing as it arrives; unreadablePoll is
// how often it asks.
const (
	unreadableRetry = 3 * time.Second
	unreadablePoll  = 100 * time.Millisecond
	// unreadableMemory is how many retries long a failed retry is remembered.
	unreadableMemory = 10
	// unreadableBrief is how long an export waits while a failed retry is remembered: a
	// couple of polls, enough for a sign-in written as it arrives.
	unreadableBrief = 2 * unreadablePoll
)

// arrivalLane is the lane what arrives now is bound to: the account key of the person
// signed in, or no person's (sendas.NoPersonLane) under a server key. With neither, it is
// why what arrives is lost once it is placed: signed out, or a sign-in that still can't be
// read after unreadableRetry. Within unreadableRetry's last few multiples of a retry that
// failed, an export waits only unreadableBrief: a sign-in that stays unreadable costs each
// export a couple of polls, not the retry, and one that reads again is used at once.
//
// The retry covers a sign-in rewritten as the export arrives, as the same person's token
// rotation rewrites it, so it may end only on the lane last read when the export arrived,
// at the same generation of the sign-in, and only if no read since changed either
// (laneState: taken together at the start). A rotation leaves the generation; any change of
// whose lane it is bumps it, in the same write, so a switch to B and back to A, or to a
// server key and back to A, that no export read still reads as A at another generation.
// One that reads as another lane or generation, or the same once another export read
// another meanwhile, was a switch, and what arrived
// during it may be either's, so it is lost, as unreadable. A relay that has read no lane
// yet (none was readable when it started, nor since) has nothing to tell a switch by, and
// takes what the retry reads, unless a read meanwhile found one: losing every export a
// rotation meets at its start would cost more than the rare switch in the moment it starts.
func (r *Relay) arrivalLane(ctx context.Context) (lane, lost string) {
	start := time.Now()
	startLane, startGen, startChanges := r.lanes.snapshot()
	r.mu.Lock()
	deadline := start.Add(r.unreadableRetry)
	if !r.unreadableAt.IsZero() && start.Sub(r.unreadableAt) < unreadableMemory*r.unreadableRetry {
		deadline = start.Add(min(unreadableBrief, r.unreadableRetry))
	}
	r.mu.Unlock()
	for retried := false; ; retried = true {
		ticket := r.lanes.take()
		switch account, gen := r.lane(); account {
		case "":
			return "", lostSignedOut
		case sendas.AccountUnreadable:
		default:
			// The changes other reads made: this one's own is no switch it missed.
			changes := r.lanes.apply(ticket, account, gen)
			r.mu.Lock()
			r.unreadableAt = time.Time{}
			r.mu.Unlock()
			if retried && (changes != startChanges || startLane != "" && (startLane != account || startGen != gen)) {
				return "", lostUnreadable
			}
			return account, ""
		}
		wait := min(unreadablePoll, time.Until(deadline))
		if wait <= 0 {
			r.mu.Lock()
			r.unreadableAt = time.Now()
			r.mu.Unlock()
			return "", lostUnreadable
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", lostUnreadable
		case <-t.C:
		}
	}
}

// laneState is the lane the sign-in read as last and the generation it was read at, with a
// count of the changes reads made to either, so a retry can tell whether they changed while
// it waited, even back to what they were (arrivalLane). The sign-in is read outside mu, so
// reads finish in any order: each takes a ticket before it reads, and only one newer than
// every read applied so far moves the lane, so a stale read never moves it back. Reads are
// ordered by ticket, not by generation: a terma that drops the generation from its file
// starts it again at 0.
type laneState struct {
	mu      sync.Mutex
	last    string
	gen     uint64 // the generation of the sign-in last was read from
	changes uint64 // bumped whenever last or gen changes
	ticket  uint64 // the last ticket taken
	applied uint64 // the newest ticket whose read was applied
}

// take is the ticket of a read about to start.
func (l *laneState) take() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ticket++
	return l.ticket
}

// apply records lane, read at generation gen under ticket, unless a newer read was applied
// first, and returns the changes as they stood before: what other reads made.
func (l *laneState) apply(ticket uint64, lane string, gen uint64) (before uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	before = l.changes
	if ticket > l.applied {
		l.applied = ticket
		if lane != l.last || gen != l.gen {
			l.last, l.gen = lane, gen
			l.changes++
		}
	}
	return before
}

// snapshot is the lane read last, its generation and the changes so far, together.
func (l *laneState) snapshot() (lane string, gen, changes uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last, l.gen, l.changes
}

// seedLane records the lane in place as the relay starts, which its first export's retry
// may end on (arrivalLane): none while the sign-in can't be read, or is signed out.
func (r *Relay) seedLane() {
	ticket := r.lanes.take()
	if lane, gen := r.lane(); lane != "" && lane != sendas.AccountUnreadable {
		r.lanes.apply(ticket, lane, gen)
	}
}

// lane is account with the generation of the sign-in it was read from (SignIn.Lane).
func (r *Relay) lane() (string, uint64) {
	if r.opts.SignIn == nil {
		return sendas.NoPersonLane, 0
	}
	return r.opts.SignIn.Lane()
}

// account is the lane of the sign-in now: the account key of the person signed in, or no
// person's under a server key.
func (r *Relay) account() string {
	if r.opts.SignIn == nil {
		return sendas.NoPersonLane
	}
	return r.opts.SignIn.Account()
}

// mayLeave reports whether rt's parts may leave now. A route is one lane: a person's leaves
// only with no server key (serverKey), while its account's sign-in is the one in place; no
// person's only under a server key. Every other route, one without a lane among them,
// waits, and ages out with the outbox.
func (r *Relay) mayLeave(rt route, serverKey bool) bool {
	return r.mayLeaveAccount(rt.account, serverKey)
}
func (r *Relay) mayLeaveAccount(account string, serverKey bool) bool {
	if serverKey {
		return account == sendas.NoPersonLane
	}
	return auth.ValidAccountKey(account) && account == r.account()
}
func (r *Relay) laneActive(account string) bool {
	return r.account() == account && (account == sendas.NoPersonLane || auth.ValidAccountKey(account))
}
