package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
)

// redeem exchanges current's refresh token if redeem asks, and saves the new pair in tx
// when there is one. It follows the server's rules for an answer that is not a new pair:
//
//   - 409 REFRESH_SUPERSEDED: a concurrent refresh of the same token won. Re-read the
//     stored pair and take it when its refresh token changed; otherwise retry once at
//     once, within the grace, and a second 409 ends the sign-in.
//   - 5xx, 408, or no answer: something may have rotated without being returned. Retry
//     with the same token, after Retry-After, until refreshAttemptBudget after the first
//     attempt; past it the sign-in ends rather than present a spent token after the grace.
//     A Retry-After that would end past the budget is cut short, so one last retry still
//     starts a margin before the budget ends, inside the grace.
//   - After an attempt in doubt, an answer that rotated nothing (a 429, a bare 401, a
//     request that never connected) is retried the same way: the doubt stays, and a later
//     answer may still return the rotation.
//   - 401 in the auth gateway's envelope: the session is over, and the stored pair goes,
//     compare-and-set on its refresh token. A bare 401, maybe a proxy's, spent nothing.
//
// The store records when the refresh came into doubt before the token first leaves, so a
// process killed mid-request leaves the next one the same budget; the mark goes again only
// on an answer that rotated nothing.
//
// The exchange outlives its caller: a heartbeat's deadline or a Ctrl-C after the server
// rotated the pair would otherwise lose the rotation. A caller already gone sends
// nothing; once the token leaves, the sends, retries and save run within the budget
// whatever the caller does.
func (c *Client) redeem(ctx context.Context, held, current *auth.Credential, redeem redeemFunc, tx *auth.Tx) error {
	if !redeem(held, current) {
		return nil
	}
	// The gateway refused this token: it is never presented again, and its removal is
	// tried again, compare-and-set as the first time.
	if c.wasRefused(current.RefreshToken) {
		if tx != nil {
			_ = tx.DeleteIf(c.profile, current.RefreshToken)
		}
		return errSessionExpired
	}
	first := current.RefreshInDoubtSince
	if !first.IsZero() && !withinBudget(first, 0) {
		return c.endSignIn(tx, current)
	}
	// clean is whether no attempt so far may have rotated: only then may the mark go.
	clean := first.IsZero()
	if clean {
		// The wall clock, as a stored mark has it: a monotonic reading stops while the
		// machine sleeps, and a retry after a sleep must count the time asleep.
		first = time.Now().Round(0)
	}
	superseded := false
	// Nothing leaves for a caller that has gone.
	if err := ctx.Err(); err != nil {
		return c.settle(tx, current, clean, err)
	}
	exchange := context.WithoutCancel(ctx)
	for {
		if !clean && !withinBudget(first, 0) {
			return c.endSignIn(tx, current)
		}
		// Unrecorded, the token does not leave: a process that died after the server
		// rotated it would leave the next one a spent token it took for fresh, whose
		// presentation after the grace revokes the whole family.
		marked, err := c.markInDoubt(tx, current, first)
		if err != nil {
			return err
		}
		current = marked
		// The whole attempt, its dial and handshake too, ends with the budget.
		attempt, cancel := context.WithDeadline(exchange, first.Add(refreshAttemptBudget))
		inBudget := func() bool { return withinBudget(first, 0) }
		resp, err := c.postToken(attempt, inBudget, tokenRequest{GrantType: "refresh_token", RefreshToken: current.RefreshToken})
		cancel()
		switch kind, wait := classifyRefresh(err); kind {
		case refreshRotated:
			return c.keepRotation(tx, current, c.credentialFrom(resp))
		case refreshOver:
			// Remembered first: should the store not take the removal, this client still
			// never presents the token again.
			c.refuse(current.RefreshToken)
			if tx != nil {
				_ = tx.DeleteIf(c.profile, current.RefreshToken)
			}
			return fmt.Errorf("%w: session expired, run `terma setup`", err)
		case refreshSuperseded:
			clean = false
			if tx != nil {
				if stored, loadErr := tx.Load(c.profile); loadErr == nil && stored.SessionID == current.SessionID &&
					stored.RefreshToken != current.RefreshToken {
					c.replace(stored)
					return nil
				}
			}
			if superseded || !withinBudget(first, 0) {
				return c.endSignIn(tx, current)
			}
			superseded = true
		case refreshInDoubt:
			clean = false
			wait, ok := retryWait(first, wait)
			if !ok {
				return c.endSignIn(tx, current)
			}
			time.Sleep(wait)
		default:
			// Answered, or never sent, before anything rotated. After an earlier attempt in
			// doubt, one held back past the budget ends the sign-in, as the next would, and
			// one that may pass on a retry is tried again within the budget: a later answer
			// may still return the rotation.
			if !clean && errors.Is(err, errPastBudget) {
				return c.endSignIn(tx, current)
			}
			if !clean && retryable(err) {
				if wait, ok := retryWait(first, cmp.Or(retryAfterOf(err), defaultRetryAfter)); ok {
					time.Sleep(wait)
					continue
				}
			}
			return c.settle(tx, current, clean, err)
		}
	}
}

// withinBudget reports whether a send after wait more still starts within the budget of a
// refresh first sent at first, on the wall clock. A first in the future (the clock went
// back since it was recorded) is past the budget: how long ago the token really left is
// unknown, and presenting it after the server's grace revokes the whole family.
func withinBudget(first time.Time, wait time.Duration) bool {
	elapsed := time.Since(first)
	return elapsed >= 0 && elapsed+wait <= refreshAttemptBudget
}

// retryWait is how long to wait before retrying a refresh first sent at first, wanting
// wait: cut short so the retry starts a margin (a tenth of the budget) before the budget
// ends; false when no retry can start by then.
func retryWait(first time.Time, wait time.Duration) (time.Duration, bool) {
	margin := refreshAttemptBudget / 10
	elapsed := time.Since(first)
	left := refreshAttemptBudget - margin - elapsed
	if elapsed < 0 || left <= 0 {
		return 0, false
	}
	return min(wait, left), true
}

// retryable reports whether a refresh answered without a rotation may pass on a retry: one
// that never connected, a 429, or a bare 401, maybe a proxy's.
func retryable(err error) bool {
	apiErr, isAPI := errors.AsType[*Error](err)
	return notSent(err) || errors.As(err, new(bare401Error)) || isAPI && apiErr.StatusCode == http.StatusTooManyRequests
}

// retryAfterOf is the wait err's answer named, 0 for none.
func retryAfterOf(err error) time.Duration {
	if apiErr, ok := errors.AsType[*Error](err); ok {
		return apiErr.RetryAfter
	}
	return 0
}

// What a refresh's answer says of the token it presented.
const (
	refreshRotated    = iota // a new pair
	refreshOver              // 401: the session is over
	refreshSuperseded        // 409: a concurrent refresh of the token won
	refreshInDoubt           // a 5xx or no answer: it may have rotated
	refreshUnspent           // anything else, or a request that never left: nothing rotated
)

// classifyRefresh sorts a refresh's error, and for one in doubt says how long to wait
// before the retry.
func classifyRefresh(err error) (kind int, wait time.Duration) {
	apiErr, isAPI := errors.AsType[*Error](err)
	switch {
	case err == nil:
		return refreshRotated, 0
	case errors.Is(err, auth.ErrNotLoggedIn):
		return refreshOver, 0
	case isAPI && apiErr.Superseded():
		return refreshSuperseded, 0
	case isAPI && (apiErr.StatusCode >= 500 || apiErr.StatusCode == http.StatusRequestTimeout):
		return refreshInDoubt, cmp.Or(apiErr.RetryAfter, defaultRetryAfter)
	case errors.Is(err, errPastBudget):
		return refreshUnspent, 0
	case errors.As(err, new(bare401Error)):
		// A 401 without the gateway's envelope never reached the auth host's verdict.
		return refreshUnspent, 0
	case !isAPI && !notSent(err):
		return refreshInDoubt, defaultRetryAfter
	default:
		return refreshUnspent, 0
	}
}

// settle returns err, first clearing the mark this call wrote when nothing it sent can
// have rotated.
func (c *Client) settle(tx *auth.Tx, current *auth.Credential, clean bool, err error) error {
	if clean {
		_, _ = c.markInDoubt(tx, current, time.Time{})
	}
	return err
}

// notSent reports whether err failed a request before any of it reached the server: no
// address, or no connection (a TLS or proxy failure included). Such a refresh rotated
// nothing, so it is safe to try later.
func notSent(err error) bool {
	if _, ok := errors.AsType[notConnectedError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return true
	}
	opErr, ok := errors.AsType[*net.OpError](err)
	return ok && opErr.Op == "dial"
}

// markInDoubt records, in the store, since as when current's refresh came into doubt (zero:
// not in doubt), and returns current so marked; with no store there is nothing to record.
func (c *Client) markInDoubt(tx *auth.Tx, current *auth.Credential, since time.Time) (*auth.Credential, error) {
	if tx == nil || current.RefreshInDoubtSince.Equal(since) {
		return current, nil
	}
	if err := tx.MarkRefreshInDoubt(c.profile, current.SessionID, current.RefreshToken, since); err != nil {
		return current, fmt.Errorf("record the refresh before sending it: %w", err)
	}
	marked := *current
	marked.RefreshInDoubtSince = since
	return &marked, nil
}
