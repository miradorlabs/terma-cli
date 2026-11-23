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

// leaving keeps of a route's entries, in arrival order, up to limit of those that may be sent
// now. Every entry of a route is in its lane: a person's leaves only with no server key
// (serverKey), while its account's sign-in is the one in place; no person's only under a
// server key. An older terma's ambiguous backlog never leaves.
//
// The sign-in is read now, once: what it returns reads nothing, so a sender can run it on
// its live queue under its lock. Its result may share entries' array.
func (r *Relay) leaving(rt route, serverKey bool, limit int) func([]entry) []entry {
	may := serverKey && rt.account == sendas.NoPersonLane ||
		!serverKey && auth.ValidAccountKey(rt.account) && rt.account == r.account()
	return func(entries []entry) []entry {
		if !may {
			return nil
		}
		return entries[:min(limit, len(entries))]
	}
}
