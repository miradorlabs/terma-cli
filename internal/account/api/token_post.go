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

// errPastBudget is a refresh token held back because the wall clock had passed its budget
// (RefreshBudget) when the transport first read the request body: the body was never read,
// so the transport never had the token to send.
var errPastBudget = errors.New("refresh not sent: the wall clock moved past its budget")

// postToken sends body to the token endpoint. mayBeSent, when not nil, is asked once more
// when the transport first reads the body, which alone carries a refresh token: a dial, a
// TLS handshake or a wait for a connection can outlast the budget after the exchange last
// checked it, as when a Mac sleeps through it. When it says no, the body is not read and
// the request fails (errPastBudget). That is all it guarantees: a body read in budget can
// still reach the server late, held back by HTTP/2 flow control or sent from the kernel's
// buffers once a sleeping Mac wakes.
func (c *Client) postToken(ctx context.Context, body tokenRequest, mayBeSent func() bool) (*tokenResponse, error) {
	// Whether a connection was ever made is whether anything can have been written: a
	// DNS, dial, TLS or proxy failure (a captive portal's certificate) comes before it.
	// It stays true once set: a transport that retries a written POST must not read as
	// "nothing sent".
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	req, err := c.newRequest(ctx, authHost, http.MethodPost, tokenPath, nil, nil, body)
	if err != nil {
		return nil, err
	}
	var guard sendGuard
	if mayBeSent != nil {
		guard.guardBody(req, mayBeSent)
	}
	// Every failure that is not an unsettledError spent nothing: the server answered, and
	// not with a pair, or the request never reached it.
	resp, err := c.http.Do(req)
	switch {
	case err != nil && guard.heldBack.Load() && !guard.passed.Load():
		return nil, fmt.Errorf("%s %s: %w", http.MethodPost, tokenPath, errPastBudget)
	case err != nil && !connected.Load():
		return nil, fmt.Errorf("%s %s: %w", http.MethodPost, tokenPath, err)
	case err != nil:
		return nil, unsettledError{fmt.Errorf("%s %s: %w", http.MethodPost, tokenPath, err)}
	}
	defer resp.Body.Close()

	var out tokenResponse
	if err := decode(resp, &out); err != nil {
		var apiErr *Error
		switch {
		case !errors.As(err, &apiErr):
			// A success cut off or garbled on the way: its pair is lost.
			return nil, unsettledError{err}
		case apiErr.Unauthenticated() && apiErr.Code == "UNAUTHENTICATED":
			return nil, refusalError{fmt.Errorf("%w: %s", auth.ErrNotLoggedIn, apiErr.Message)}
		case apiErr.Unauthenticated() && body.GrantType == "refresh_token":
			// No gateway envelope, maybe a proxy's: no sign-out, so apiErr is not wrapped.
			return nil, fmt.Errorf("the token endpoint answered 401 without the auth gateway's refusal: %s", apiErr.Error())
		case apiErr.Unauthenticated():
			return nil, fmt.Errorf("%w: %s", auth.ErrNotLoggedIn, apiErr.Message)
		case apiErr.StatusCode == http.StatusServiceUnavailable && apiErr.Code == "UNAVAILABLE":
			// Nothing spent: the auth gateway's own answer when the account service could not
			// be reached (gRPC Unavailable); a failure after the rotation committed is 500
			// INTERNAL. Any other 503 stays unknown below: the GKE load balancer in front of
			// the gateway can answer a bare 502 or 503 after the rotation committed, as when
			// the gateway's pod is killed mid-request.
			//
			// The risk taken: a gRPC connection that breaks after the account service
			// committed the rotation also surfaces as Unavailable (an outage, or an account
			// pod stopped hard mid-call in a rollout). That needs the break to fall after the
			// commit and before the answer, and it ends the session family only if a later
			// refresh then sends the spent token after the server's grace.
			return nil, err
		case apiErr.StatusCode >= 500:
			return nil, unsettledError{err}
		}
		// A 429 (turned away before the account service runs), or any other 4xx: nothing spent.
		return nil, err
	}
	// A success all the same, so the server may have spent the token. Half a pair is no
	// sign-in: one without a refresh token could not refresh, and would replace one that can.
	switch {
	case out.AccessToken == "":
		return nil, unsettledError{errors.New("token endpoint returned no access token")}
	case out.RefreshToken == "":
		return nil, unsettledError{errors.New("token endpoint returned no refresh token")}
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
