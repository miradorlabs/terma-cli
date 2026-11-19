package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
)

// unsent reports whether err is a request that never left this machine: its connection
// was never made, so the server cannot have spent the token in it.
func unsent(err error) bool {
	var dnsErr *net.DNSError
	var opErr *net.OpError
	return errors.As(err, &dnsErr) || errors.As(err, &opErr) && opErr.Op == "dial"
}

// unspentError is a token exchange the server cannot have spent the token in: the request
// never left this machine, or the gateway turned it away (429). It ends an exchange only
// when no attempt before it has an unknown outcome.
type unspentError struct{ error }

func (e unspentError) Unwrap() error { return e.error }

// unsettledError is a token exchange whose outcome this client never learned: the
// request was sent and no answer came, a 5xx did, or a success without a usable pair. The
// server may still have spent the token it was sent.
type unsettledError struct{ error }

func (e unsettledError) Unwrap() error { return e.error }

// refusalError is the auth gateway's own refusal of a token: a 401 in its envelope. A 401
// without it may come from some other hop, and says nothing about the session.
type refusalError struct{ error }

func (e refusalError) Unwrap() error { return e.error }

// errSessionExpired is a refresh the server refused.
var errSessionExpired = fmt.Errorf("%w: session expired, run `terma setup`", auth.ErrNotLoggedIn)

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
//     The pauses between attempts (refreshPause) start short and double.
const RefreshBudget = 40 * time.Second

// refreshPause is the pause after attempt n (from 0): 1 s, then doubling up to 8 s.
func refreshPause(n int) time.Duration { return time.Second << min(n, 3) }

// errRefreshUnsettled is a stored refresh token whose last refresh got no answer longer
// than RefreshBudget ago. The server may have spent it, and forgives it no longer: sent,
// it would revoke the whole family of sign-ins it belongs to, Terma Desktop's included.
// So it is not sent, and the refresh does not remove the credential either, since it may
// be alive; `terma setup` replaces it. If it is alive, the developer signs in again for
// nothing; if it is not, that saves every session.
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

// inBudget reports whether a token first sent at first may still be sent again. A wall
// clock that has gone back says nothing about how long ago that was, so it may not.
func (c *Client) inBudget(first time.Time) bool {
	elapsed := c.wallNow().Sub(first)
	return elapsed >= 0 && elapsed <= c.budget()
}

// exchangeRefreshToken redeems refreshToken, first sent at first. unknown says an earlier
// exchange of it, maybe another process's, got no answer.
//
// Until an attempt's outcome is unknown (unsettledError), the first answer is final: a
// refusal, or an attempt that spent nothing (unspentError), ends the exchange at once.
// Once one is unknown, the same token is sent again until an answer settles it: a pair,
// or the auth gateway's refusal twice in a row. Nothing else can. The server answers a
// refresh that lost a race to another rotation of the session with a refusal too, and the
// unknown attempt may be that rotation, committed late; sent once more, the token is the
// one it spent, which the server forgives. An exchange that runs out unsettled is no
// sign-out: the session may be alive, and must not be removed.
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
		pause := time.NewTimer(refreshPause(attempt))
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

// withoutMark is cred without its mark, which only a read under the credential lock may
// trust: refresh reads the store's anew, and the client keeps its own (Client.unsettled).
func withoutMark(cred *auth.Credential) *auth.Credential {
	if cred == nil || cred.RefreshUnsettledAt.IsZero() {
		return cred
	}
	bare := *cred
	bare.RefreshUnsettledAt = time.Time{}
	return &bare
}

// unsettledSince is when current's refresh token was first sent without an answer, as
// current (read under the lock) or this client's own mark has it: the earlier of the two.
func (c *Client) unsettledSince(current *auth.Credential) time.Time {
	at := current.RefreshUnsettledAt
	c.mu.Lock()
	mine := c.unsettled
	c.mu.Unlock()
	if mine.refreshToken == current.RefreshToken && !mine.at.IsZero() && (at.IsZero() || mine.at.Before(at)) {
		return mine.at
	}
	return at
}

// markUnsettled records that current's refresh token, first sent at first, has no answer
// yet: on the client's credential while it still holds that token, and on tx's while it
// does (Tx.MarkUnsettled), so every process that later takes the token knows. An earlier
// mark stays, and replaces a later one.
func (c *Client) markUnsettled(current *auth.Credential, first time.Time, tx *auth.Tx) {
	c.setUnsettled(current.RefreshToken, func(at time.Time) bool { return at.IsZero() || first.Before(at) }, first)
	if tx != nil {
		// Best effort: unmarked, the next refresh sends the token afresh, as before.
		_ = tx.MarkUnsettled(c.profile, current.OrganizationID, current.RefreshToken, first)
	}
}

// clearUnsettled takes back markUnsettled(current, first, tx) once an answer shows the
// server spent nothing.
func (c *Client) clearUnsettled(current *auth.Credential, first time.Time, tx *auth.Tx) {
	c.setUnsettled(current.RefreshToken, first.Equal, time.Time{})
	if tx != nil {
		_ = tx.ClearUnsettled(c.profile, current.OrganizationID, current.RefreshToken, first)
	}
}

// setUnsettled sets the client's own mark of refreshToken to to, while the mark it has
// for that token (zero for another) is one from accepts.
func (c *Client) setUnsettled(refreshToken string, from func(time.Time) bool, to time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var at time.Time
	if c.unsettled.refreshToken == refreshToken {
		at = c.unsettled.at
	}
	if from(at) {
		c.unsettled = unsettledMark{refreshToken: refreshToken, at: to}
	}
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
