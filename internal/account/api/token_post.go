package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
)

// errPastBudget is a refresh token held back because the wall clock had passed the
// refresh's budget when the transport first read the request body: the body was never
// read, so the transport never had the token to send.
var errPastBudget = errors.New("refresh not sent: the wall clock moved past its budget")

// notConnectedError is a request that failed before any connection was made: a DNS, dial,
// TLS or proxy failure (a captive portal's certificate). Nothing of it reached the server.
type notConnectedError struct{ error }

func (e notConnectedError) Unwrap() error { return e.error }

// bare401Error is a refresh answered 401 without the auth gateway's envelope, maybe by a
// proxy: it says nothing of the session, so it signs no one out. It does not unwrap to the
// *Error, which callers would read as the session refused.
type bare401Error struct{ apiErr *Error }

func (e bare401Error) Error() string {
	return "the token endpoint answered 401 without the auth gateway's refusal: " + e.apiErr.Error()
}

// postToken sends body to the token endpoint, with no bearer. mayBeSent, when not nil, is
// asked once more when the transport first reads the body, which alone carries a refresh
// token: a dial, a TLS handshake or a wait for a connection can outlast the budget after
// the refresh last checked it, as when a Mac sleeps through it. When it says no, the body
// is not read and the request fails with errPastBudget. That is all it guarantees: a body
// read in budget can still reach the server late, sent from the kernel's buffers once a
// sleeping Mac wakes.
func (c *Client) postToken(ctx context.Context, mayBeSent func() bool, body tokenRequest) (*tokenResponse, error) {
	// Whether a connection was ever made is whether anything can have been written. It
	// stays true once set: a transport that retries a written POST must not read as
	// "nothing sent".
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	req, err := c.newRequest(ctx, authHost, http.MethodPost, tokenPath, nil, nil, body)
	if err != nil {
		return nil, err
	}
	req.Header.Del("Authorization")
	var guard sendGuard
	if mayBeSent != nil {
		guard.guardBody(req, mayBeSent)
	}
	resp, err := c.http.Do(req)
	switch {
	case err != nil && guard.heldBack.Load() && !guard.passed.Load():
		return nil, fmt.Errorf("%s %s: %w", http.MethodPost, tokenPath, errPastBudget)
	case err != nil && !connected.Load():
		return nil, notConnectedError{fmt.Errorf("%s %s: %w", http.MethodPost, tokenPath, err)}
	case err != nil:
		return nil, fmt.Errorf("%s %s: %w", http.MethodPost, tokenPath, err)
	}
	defer resp.Body.Close()

	var out tokenResponse
	if err := decode(resp, &out); err != nil {
		apiErr, ok := errors.AsType[*Error](err)
		switch {
		case !ok || !apiErr.Unauthenticated():
		case body.GrantType == "refresh_token" && apiErr.Code != "UNAUTHENTICATED":
			return nil, bare401Error{apiErr}
		default:
			return nil, fmt.Errorf("%w: %w", auth.ErrNotLoggedIn, apiErr)
		}
		return nil, err
	}
	if out.AccessToken == "" || out.RefreshToken == "" || out.SessionID == "" {
		return nil, errors.New("token endpoint returned an incomplete sign-in")
	}
	return &out, nil
}

// sendGuard is what the guarded bodies of one request found: heldBack, that one was held
// back; passed, that one was let through, so its bytes may have been written.
type sendGuard struct{ heldBack, passed atomic.Bool }

// guardBody makes req's body, and every copy the transport takes to send it again
// (GetBody), ask mayBeSent before the first byte is read.
func (g *sendGuard) guardBody(req *http.Request, mayBeSent func() bool) {
	getBody := req.GetBody
	req.GetBody = func() (io.ReadCloser, error) {
		rc, err := getBody()
		if err != nil {
			return nil, err
		}
		return &guardedBody{ReadCloser: rc, mayBeSent: mayBeSent, guard: g}, nil
	}
	req.Body, _ = req.GetBody()
}

// guardedBody is a request body that asks mayBeSent on its first Read, and is never read
// when it says no. The answer stands for every later Read.
type guardedBody struct {
	io.ReadCloser
	mayBeSent func() bool
	guard     *sendGuard
	asked, ok bool
}

func (b *guardedBody) Read(p []byte) (int, error) {
	if !b.asked {
		b.asked, b.ok = true, b.mayBeSent()
		if b.ok {
			b.guard.passed.Store(true)
		} else {
			b.guard.heldBack.Store(true)
		}
	}
	if !b.ok {
		return 0, errPastBudget
	}
	return b.ReadCloser.Read(p)
}
