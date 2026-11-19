package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
)

// unsettledError is a token exchange whose outcome this client never learned: the
// request was sent and no answer came, a 5xx other than the auth gateway's 503
// UNAVAILABLE did, or a success without a usable pair. The server may still have spent
// the token it was sent.
type unsettledError struct{ error }

func (e unsettledError) Unwrap() error { return e.error }

// refusalError is the auth gateway's own refusal of a token: a 401 in its envelope. A 401
// without it may come from some other hop, and says nothing about the session.
type refusalError struct{ error }

func (e refusalError) Unwrap() error { return e.error }

// errSessionExpired is a refresh the server refused.
var errSessionExpired = fmt.Errorf("%w: session expired, run `terma setup`", auth.ErrNotLoggedIn)

// errRefused is errSessionExpired for a refresh token the auth gateway itself refused
// (refusalError), now or earlier in this client (Client.refused). It reads the same.
var errRefused = fmt.Errorf("%w", errSessionExpired)

// Refused reports whether err is the auth gateway's own refusal of a refresh token: the
// sign-in is over. Any other ErrNotLoggedIn, a bare 401 from some other hop or an access
// token refused once, says nothing about whether the refresh token is alive.
func Refused(err error) bool { return errors.Is(err, errRefused) }

// RefreshBudget bounds how long a refresh token may be sent again after it was first sent,
// on the wall clock, every attempt and pause included, by every process that sends it.
//
// The server rotates the refresh token on every refresh, and revokes the session when a
// spent one comes back, except the one it spent last, within 60 s of spending it
// (refreshRetryGrace in mirador-platform's cli_session_refresh.go). So a refresh whose
// answer was lost sends the same token again, and every attempt must start within those
// 60 s of the first. The rotation it repeats was committed after the first was sent.
//
//   - 40 s leaves the last attempt 20 s inside the grace, and keeps the lock holder within
//     auth's lockWait: 40 s plus two keychain writes. It also bounds how long a refresh
//     already sent keeps a process from exiting, so the relay's service gives it that long
//     to stop (service.StopWindow). A Client's exchangeBudget replaces it in tests.
//   - Once an attempt's outcome is unknown, the token is sent again until the budget is
//     spent, not a fixed number of times: a mark left past the budget asks for a new
//     sign-in (errRefreshUnsettled), so a few seconds of server errors must not leave one.
//     The pauses between attempts (Client.pause) start short and double.
const RefreshBudget = 40 * time.Second

// pause is the pause after attempt n (from 0): 1 s, then doubling up to 8 s. A Client's
// pauseUnit replaces the second in tests.
func (c *Client) pause(n int) time.Duration { return cmp.Or(c.pauseUnit, time.Second) << min(n, 3) }

// errRefreshUnsettled is a stored refresh token whose last refresh got no answer longer
// than RefreshBudget ago, or whose mark is in the future (the wall clock went back). The
// server may have spent it, and forgives it no longer: sent, it would revoke the whole
// family of sign-ins it belongs to, Terma Desktop's included. So it is not sent, and the
// refresh does not remove the credential either, since it may be alive; `terma setup`
// replaces it. If it is alive, the developer signs in again for nothing; if it is not,
// that saves every session.
var errRefreshUnsettled = fmt.Errorf("%w: the answer to this sign-in's last refresh was lost, and sending its token again "+
	"could end every Terma sign-in on this machine: run `terma setup`", auth.ErrNotLoggedIn)

// budget is how long an exchange may resend its token after first sending it.
func (c *Client) budget() time.Duration { return cmp.Or(c.exchangeBudget, RefreshBudget) }

// wallNow is the time on the wall clock, without the monotonic reading: the monotonic
// clock stands still while a Mac sleeps, and the server's grace does not.
func (c *Client) wallNow() time.Time {
	if c.now != nil {
		return c.now().Round(0)
	}
	return time.Now().Round(0)
}

// inBudget reports whether a token first sent at first may still be sent again.
func (c *Client) inBudget(first time.Time) bool { return inBudget(c.wallNow(), first, c.budget()) }

// inBudget reports whether, at now, a token first sent at first may still be sent again
// within budget. A wall clock that has gone back says nothing about how long ago that
// was, so it may not.
func inBudget(now, first time.Time, budget time.Duration) bool {
	elapsed := now.Sub(first)
	return elapsed >= 0 && elapsed <= budget
}

// RefreshUnsettled reports whether err is a refresh that did not send its token because the
// answer to an earlier one was lost (errRefreshUnsettled): the token may still be alive.
func RefreshUnsettled(err error) bool { return errors.Is(err, errRefreshUnsettled) }

// RefreshLost reports whether cred's refresh token may have been spent by a refresh whose
// answer never came, too long ago to send it again: the sign-in must be made anew.
func RefreshLost(cred *auth.Credential) bool {
	at := cred.UnsettledSince()
	return !at.IsZero() && !inBudget(time.Now().Round(0), at, RefreshBudget)
}

// exchangeRefreshToken redeems refreshToken, first sent at first. unknown says an earlier
// exchange of it, maybe another process's, got no answer.
//
// Until an attempt's outcome is unknown (unsettledError), the first answer is final: a
// refusal, or any other failure, which spent nothing (postToken), ends the exchange at
// once. Once one is unknown, the same token is sent again until an answer settles it: a
// pair, or the auth gateway's refusal twice in a row. Nothing else can, a 503 or a 429
// included. The server answers a refresh that lost a race to another rotation of the
// session with a refusal too, and the unknown attempt may be that rotation, committed
// late; sent once more, the token is the one it spent, which the server forgives. An
// exchange that runs out unsettled is no sign-out: the session may be alive, and must not
// be removed.
//
// It runs to the end whether or not ctx does: the server may rotate the pair after a
// caller gives up, and that pair must reach the store. So a caller's deadline, a relay's
// shutdown included, no longer bounds a refresh already sent; the budget does.
func (c *Client) exchangeRefreshToken(ctx context.Context, refreshToken string, first time.Time, unknown bool) (*tokenResponse, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.budget()-c.wallNow().Sub(first))
	defer cancel()
	lastRefused := false
	var err error
	for attempt := 0; ; attempt++ {
		// The context's deadline runs on the monotonic clock, which a sleeping Mac stops.
		switch {
		case c.inBudget(first):
		case !unknown:
			// Nothing was sent, so nothing can have been spent.
			return nil, errors.New("refresh not sent: the wall clock moved past its budget")
		default:
			// An earlier send, maybe another process's, still has no answer.
			return nil, stillUnknown(cmp.Or(err, context.DeadlineExceeded))
		}
		var resp *tokenResponse
		resp, err = c.postToken(ctx, tokenRequest{GrantType: "refresh_token", RefreshToken: refreshToken})
		refused := errors.As(err, new(refusalError))
		switch {
		case err == nil:
			return resp, nil
		case !unknown && !errors.As(err, new(unsettledError)):
			return nil, err
		case refused && lastRefused:
			return nil, err
		}
		unknown, lastRefused = true, refused
		pause := time.NewTimer(c.pause(attempt))
		select {
		case <-pause.C:
		case <-ctx.Done():
			pause.Stop()
			return nil, stillUnknown(err)
		}
	}
}

// unsettledMark is when refreshToken was first sent without an answer.
type unsettledMark struct {
	refreshToken string
	at           time.Time
}

// unsettledSince is when current's refresh token was first sent without an answer, as
// current's own mark or this client's has it: the earlier of the two. current is the pair
// read under the credential lock, or one only this client holds (a rotation the store
// missed, or a client without a profile); a mark read without the lock elsewhere is
// never consulted.
func (c *Client) unsettledSince(current *auth.Credential) time.Time {
	at := current.UnsettledSince()
	c.mu.Lock()
	mine := c.unsettled
	c.mu.Unlock()
	if mine.refreshToken == current.RefreshToken && !mine.at.IsZero() && (at.IsZero() || mine.at.Before(at)) {
		return mine.at
	}
	return at
}

// markUnsettled records that current's refresh token, first sent at first, has no answer
// yet: in tx (Tx.MarkUnsettled, a write to credentials.json alone, since current was
// loaded under the same lock), so every process that later takes the token knows, and
// then in this client. A mark the store did not take is not kept here either: the token
// is not sent, so nothing is outstanding.
func (c *Client) markUnsettled(current *auth.Credential, first time.Time, tx *auth.Tx) error {
	if tx != nil {
		if err := tx.MarkUnsettled(c.profile, current.OrganizationID, current.RefreshToken, first); err != nil {
			return err
		}
	}
	c.markOwn(current.RefreshToken, first)
	return nil
}

// expireUnsettled marks current's refresh token as never to be sent again
// (auth.UnsettledExpired): its budget is spent, or its mark is in the future. This
// client's verdict is set first and stands whatever the wall clock does next, so this
// client never sends the token again even when the store's write fails. That write fails
// only when credentials.json cannot be written, and the error says so: the store then
// keeps the mark it had, which, if it was in the future, another process takes as in
// budget once the wall clock reaches it.
func (c *Client) expireUnsettled(current *auth.Credential, tx *auth.Tx) error {
	c.markOwn(current.RefreshToken, auth.UnsettledExpired)
	if tx == nil {
		return nil
	}
	if err := tx.ExpireUnsettled(c.profile, current.OrganizationID, current.RefreshToken); err != nil {
		return fmt.Errorf("record that the refresh token may not be sent again: %w", err)
	}
	return nil
}

// markOwn sets this client's mark of refreshToken to at, unless it has an earlier one.
func (c *Client) markOwn(refreshToken string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if mine := c.unsettled; mine.refreshToken != refreshToken || mine.at.IsZero() || at.Before(mine.at) {
		c.unsettled = unsettledMark{refreshToken: refreshToken, at: at}
	}
}

// clearUnsettled takes back markUnsettled(current, first, tx) once an answer shows the
// server spent nothing. Any other mark stays.
func (c *Client) clearUnsettled(current *auth.Credential, first time.Time, tx *auth.Tx) {
	c.mu.Lock()
	if mine := c.unsettled; mine.refreshToken == current.RefreshToken && mine.at.Equal(first) {
		c.unsettled = unsettledMark{}
	}
	c.mu.Unlock()
	if tx != nil {
		// Best effort: left, the mark costs one sign-in more once its budget is past.
		_ = tx.ClearUnsettled(c.profile, current.OrganizationID, current.RefreshToken, first)
	}
}

// settled forgets this client's mark of refreshToken, which an answer has settled.
func (c *Client) settled(refreshToken string) {
	c.mu.Lock()
	if c.unsettled.refreshToken == refreshToken {
		c.unsettled = unsettledMark{}
	}
	c.mu.Unlock()
}

// stillUnknown is err, the last answer of an exchange an earlier attempt of which has an
// unknown outcome, as an unsettledError that is no sign-out.
func stillUnknown(err error) error {
	switch {
	case errors.As(err, new(unsettledError)):
		return err
	case errors.Is(err, auth.ErrNotLoggedIn):
		return unsettledError{errors.New("an earlier attempt may have refreshed the session: " + err.Error())}
	default:
		return unsettledError{fmt.Errorf("an earlier attempt may have refreshed the session: %w", err)}
	}
}
