package relay

import (
	"context"
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
)

// arrivalLane is the lane what arrives now is bound to: the account key of the person
// signed in, or no person's (sendas.NoPersonLane) under a server key. With neither, it is
// why what arrives is lost once it is placed: signed out, or a sign-in that still can't be
// read after unreadableRetry. A retry that failed within unreadableRetry's last few
// multiples is not waited out again: a sign-in that stays unreadable costs each export one
// read, not the retry.
func (r *Relay) arrivalLane(ctx context.Context) (lane, lost string) {
	start := time.Now()
	r.mu.Lock()
	deadline := start.Add(r.unreadableRetry)
	if !r.unreadableAt.IsZero() && start.Sub(r.unreadableAt) < unreadableMemory*r.unreadableRetry {
		deadline = start
	}
	r.mu.Unlock()
	for {
		switch account := r.account(); account {
		case "":
			return "", lostSignedOut
		case sendas.AccountUnreadable:
		default:
			r.mu.Lock()
			r.unreadableAt = time.Time{}
			r.mu.Unlock()
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
// person's only under a server key. Every other route, an older terma's ambiguous backlog
// among them, waits, and ages out with the outbox.
func (r *Relay) mayLeave(rt route, serverKey bool) bool {
	if serverKey {
		return rt.account == sendas.NoPersonLane
	}
	return auth.ValidAccountKey(rt.account) && rt.account == r.account()
}
