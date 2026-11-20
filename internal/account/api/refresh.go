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
//     A Retry-After that would end past the budget ends it at once, deliberately: no send
//     may land after the grace.
//   - 401: the session is over.
//
// The store records when the refresh came into doubt before the token first leaves, so a
// process killed mid-request leaves the next one the same budget; the mark goes again only
// on an answer that rotated nothing.
func (c *Client) redeem(ctx context.Context, held, current *auth.Credential, redeem redeemFunc, tx *auth.Tx) error {
	if !redeem(held, current) {
		return nil
	}
	first := current.RefreshInDoubtSince
	if !first.IsZero() && time.Since(first) > refreshAttemptBudget {
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
	for {
		// Nothing leaves for a caller that has gone, nor past the budget.
		if err := ctx.Err(); err != nil {
			return c.settle(tx, current, clean, err)
		}
		if !clean && time.Since(first) > refreshAttemptBudget {
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
		attempt, cancel := context.WithDeadline(ctx, first.Add(refreshAttemptBudget))
		resp, err := c.postToken(attempt, tokenRequest{GrantType: "refresh_token", RefreshToken: current.RefreshToken})
		cancel()
		switch kind, wait := classifyRefresh(err); kind {
		case refreshRotated:
			return c.keepRotation(tx, current, c.credentialFrom(resp))
		case refreshRetired:
			if tx != nil {
				_ = tx.MarkRetired(c.profile, current.RefreshToken)
			}
			return err
		case refreshOver:
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
			if superseded || time.Since(first) > refreshAttemptBudget {
				return c.endSignIn(tx, current)
			}
			superseded = true
		case refreshInDoubt:
			clean = false
			if ctx.Err() != nil {
				return err
			}
			if time.Since(first)+wait > refreshAttemptBudget {
				return c.endSignIn(tx, current)
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return err
			case <-timer.C:
			}
		default:
			// Answered, or never sent, before anything rotated.
			return c.settle(tx, current, clean, err)
		}
	}
}

// What a refresh's answer says of the token it presented.
const (
	refreshRotated    = iota // a new pair
	refreshOver              // 401: the session is over
	refreshRetired           // SIGN_IN_RETIRED: only a newer terma, signed in again, sends again
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
	case errors.Is(err, auth.ErrSignInRetired):
		return refreshRetired, 0
	case errors.Is(err, auth.ErrNotLoggedIn):
		return refreshOver, 0
	case isAPI && apiErr.Superseded():
		return refreshSuperseded, 0
	case isAPI && (apiErr.StatusCode >= 500 || apiErr.StatusCode == http.StatusRequestTimeout):
		return refreshInDoubt, cmp.Or(apiErr.RetryAfter, defaultRetryAfter)
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
// address, or no connection. Such a refresh rotated nothing, so it is safe to try later.
func notSent(err error) bool {
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
	if err := tx.MarkRefreshInDoubt(c.profile, current.SessionID, since); err != nil {
		return current, fmt.Errorf("record the refresh before sending it: %w", err)
	}
	marked := *current
	marked.RefreshInDoubtSince = since
	return &marked, nil
}
