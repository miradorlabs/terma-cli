package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
)

const tokenPath = "/v1/auth/cli/token"

type tokenRequest struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code,omitempty"`
	CodeVerifier string `json:"code_verifier,omitempty"`
	// RedirectPort is a pointer so a delegated code's 0 is sent, and a refresh sends none.
	RedirectPort *int   `json:"redirect_port,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	SessionID    string `json:"session_id"`
	Organization struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"organization"`
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

// LoginResult is a completed sign-in, with the organization and user names to print.
type LoginResult struct {
	Credential       *auth.Credential
	OrganizationName string
	UserID           string
}

// ExchangeCode implements auth.TokenExchanger.
func (c *Client) ExchangeCode(ctx context.Context, code, verifier string, port int) (*auth.Credential, error) {
	return c.exchangeCode(ctx, code, verifier, port)
}

// ExchangeDelegatedCode implements auth.TokenExchanger for a code Terma Desktop obtained
// for this process's challenge. No listener took it, so redirect_port is 0; the auth host
// accepts 0 only for a code its own record says was delegated.
func (c *Client) ExchangeDelegatedCode(ctx context.Context, code, verifier string) (*auth.Credential, error) {
	return c.exchangeCode(ctx, code, verifier, 0)
}

func (c *Client) exchangeCode(ctx context.Context, code, verifier string, port int) (*auth.Credential, error) {
	resp, err := c.postToken(ctx, tokenRequest{
		GrantType:    "authorization_code",
		Code:         code,
		CodeVerifier: verifier,
		RedirectPort: &port,
	})
	if err != nil {
		return nil, err
	}
	cred := c.credentialFrom(resp)
	c.mu.Lock()
	c.lastLogin = &LoginResult{
		Credential:       cred,
		OrganizationName: resp.Organization.Name,
		UserID:           resp.User.ID,
	}
	c.mu.Unlock()
	return cred, nil
}

// LastLogin returns the most recent exchange's result on this client.
func (c *Client) LastLogin() *LoginResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastLogin
}

func (c *Client) postToken(ctx context.Context, body tokenRequest) (*tokenResponse, error) {
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
	resp, err := c.http.Do(req)
	switch {
	case err != nil && !connected.Load():
		return nil, unspentError{fmt.Errorf("%s %s: %w", http.MethodPost, tokenPath, err)}
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
		case apiErr.Unauthenticated():
			return nil, fmt.Errorf("%w: %s", auth.ErrNotLoggedIn, apiErr.Message)
		case apiErr.StatusCode >= 500:
			return nil, unsettledError{err}
		case apiErr.StatusCode == http.StatusTooManyRequests:
			// The gateway turns it away before the account service runs.
			return nil, unspentError{err}
		}
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

func (c *Client) credentialFrom(resp *tokenResponse) *auth.Credential {
	cred := credentialFrom(resp)
	cred.AuthURL = c.authURL
	return cred
}

func credentialFrom(resp *tokenResponse) *auth.Credential {
	return &auth.Credential{
		AccessToken:    resp.AccessToken,
		RefreshToken:   resp.RefreshToken,
		ExpiresAt:      time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second),
		SessionID:      resp.SessionID,
		OrganizationID: resp.Organization.ID,
		UserEmail:      resp.User.Email,
		UserID:         resp.User.ID,
	}
}

func (c *Client) canRefresh() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.apiKey == "" && c.credential != nil && c.credential.RefreshToken != ""
}

func (c *Client) currentGen() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokenGen
}

func (c *Client) refreshIfNeeded(ctx context.Context) error {
	c.mu.Lock()
	needs := c.apiKey == "" && c.credential != nil && c.credential.Expired() && c.credential.RefreshToken != ""
	gen := c.tokenGen
	c.mu.Unlock()
	if !needs {
		return nil
	}
	return c.refreshIfCurrent(ctx, gen, func(_, current *auth.Credential) bool { return current.Expired() })
}

// refreshIfCurrent redeems the refresh token only if no other goroutine has rotated past
// gen, so racing callers make exactly one exchange.
func (c *Client) refreshIfCurrent(ctx context.Context, gen uint64, redeem redeemFunc) error {
	unlock, err := c.lockRefresh(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if c.currentGen() != gen {
		return nil
	}
	return c.refresh(ctx, redeem)
}

// lockRefresh takes this process's one refresh slot. It gives up when ctx ends, so a
// caller with a short deadline does not wait behind a sibling's long wait for the lock.
func (c *Client) lockRefresh(ctx context.Context) (unlock func(), err error) {
	select {
	case c.refreshing <- struct{}{}:
		return func() { <-c.refreshing }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for another refresh in this process: %w", ctx.Err())
	}
}

// Refresh rotates the pair when its access token is about to expire.
func (c *Client) Refresh(ctx context.Context) error { return c.refreshIfNeeded(ctx) }

// RefreshRefused rotates the pair even though its access token has not expired, because a
// server refused it. It rotates only while the stored access token is still the refused
// one, whose SHA-256 in hex is refusedSHA256. That way every process that saw the same
// refusal shares one rotation.
func (c *Client) RefreshRefused(ctx context.Context, refusedSHA256 string) error {
	if !c.canRefresh() {
		return auth.ErrNotLoggedIn
	}
	unlock, err := c.lockRefresh(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return c.refresh(ctx, func(_, current *auth.Credential) bool {
		return TokenSHA256(current.AccessToken) == strings.ToLower(refusedSHA256) || current.Expired()
	})
}

// TokenSHA256 is how a token is named where the token itself must not appear: its
// SHA-256, in lowercase hex.
func TokenSHA256(token string) string { return auth.TokenSHA256(token) }

// redeemFunc decides whether to redeem the refresh token. held is the credential this
// client had. current is the pair as it stands now, which another process may have rotated.
type redeemFunc func(held, current *auth.Credential) bool

// refresh brings the token pair up to date as one transaction across terma processes.
// The server revokes the whole session if a rotated refresh token is redeemed twice, so
// every step runs under the credential lock:
//
//   - reload the stored pair;
//   - take it if another process has already rotated past this client's pair;
//   - redeem only if redeem still asks for it;
//   - save the new pair before the lock is released.
//
// The lock is held through the exchange and the keychain writes. So the wait for it is
// auth's lockWait, which is longer than the exchange's RefreshBudget. The waits for the
// refresh slot and the lock follow ctx; the exchange and the save, once begun, do not.
//
// Call it in this process's refresh slot (lockRefresh).
func (c *Client) refresh(ctx context.Context, redeem redeemFunc) error {
	c.mu.Lock()
	held := c.credential
	c.mu.Unlock()
	if held == nil || held.RefreshToken == "" {
		return auth.ErrNotLoggedIn
	}
	if c.profile == "" {
		return c.redeem(ctx, held, held, redeem, nil, nil)
	}
	return auth.Locked(ctx, c.dir, func(tx *auth.Tx) error {
		stored, err := tx.LoadFor(c.profile, held.OrganizationID)
		switch {
		case err == nil && sameSession(stored, held):
		case err == nil:
			// The developer signed in again since this client loaded its pair, and the
			// sign-in normally revokes this session (revokeSession, best effort): rotate it
			// in memory only. The marks this client was given and made, and its refusals,
			// still hold.
			return c.redeem(ctx, held, held, redeem, nil, nil)
		case errors.Is(err, auth.ErrNotLoggedIn):
			// Signed out, or removed when the server refused it: never sent again.
			return errSessionExpired
		default:
			return err
		}
		if c.spent(stored.RefreshToken) {
			// The store still holds a token this client spent, because saving its
			// rotation failed. Keep the client's own live pair and try to save it now.
			// While the store does not take it, held's token is known to this client
			// alone: its marks stay here, and a mark in the store, which names no token
			// it can check against the keychain, would replace the spent token's.
			marks := tx
			if c.save(tx, held, "") != nil {
				marks = nil
			}
			return c.redeem(ctx, held, held, redeem, tx, marks)
		}
		// Decide from the stored pair, not the held one. The held one was read without the
		// lock, and may pair a rotation's tokens with the expiry from before it.
		if !samePair(stored, held) {
			c.replace(stored)
		}
		return c.redeem(ctx, held, stored, redeem, tx, tx)
	})
}

// redeem exchanges current's refresh token if redeem asks, and saves the new pair in tx
// when there is one. A token the auth gateway refused is never sent again. When tx still
// stores it, it is removed there too, so no process that reads the store afterwards sends
// it. A token whose refresh got no answer is marked so (markUnsettled), and sent again
// only within the budget of its first send. marks is where those marks go: tx while the
// store holds current's refresh token, and nil when only this client knows it.
func (c *Client) redeem(ctx context.Context, held, current *auth.Credential, redeem redeemFunc, tx, marks *auth.Tx) error {
	if !redeem(held, current) {
		return nil
	}
	if c.wasRefused(current.RefreshToken) {
		return c.refused(current, tx)
	}
	first, unknown := c.unsettledSince(current), true
	switch {
	case first.IsZero():
		first, unknown = c.wallNow(), false
	case !c.inBudget(first):
		// Past its budget, or in the future: expired for good, so a wall clock that comes
		// round to the mark again never reopens it, and for the others too.
		return errors.Join(errRefreshUnsettled, c.expireUnsettled(current, marks))
	}
	// Marked before it is sent, so a process killed with the answer outstanding leaves the
	// mark too. A pair saved, or the credential removed, takes the mark with it. A token
	// that cannot be marked is not sent: if its answer were lost, nothing would say so.
	if err := c.markUnsettled(current, first, marks); err != nil {
		return fmt.Errorf("record the refresh before sending it: %w", err)
	}
	resp, err := c.exchangeRefreshToken(ctx, current.RefreshToken, first, unknown)
	switch {
	case errors.As(err, new(unsettledError)):
		// It ran until its budget was spent: no process may send the token again.
		return errors.Join(err, c.expireUnsettled(current, marks))
	case errors.As(err, new(refusalError)):
		return c.refused(current, tx)
	case err != nil && !unknown:
		// A first answer that settles it without a pair: the server spent nothing.
		c.clearUnsettled(current, first, marks)
	}
	switch {
	case errors.Is(err, auth.ErrNotLoggedIn):
		return errSessionExpired
	case err != nil:
		return err
	}
	cred := c.credentialFrom(resp)
	// A refresh keeps the session, its user and its label.
	cred.SessionID = cmp.Or(cred.SessionID, current.SessionID)
	cred.UserID = cmp.Or(cred.UserID, current.UserID)
	cred.Label = current.Label
	// The server has spent current's refresh token, so this pair is the session's only
	// live one. Hold it before trying to store it. The answer settles current's mark.
	c.replace(cred)
	c.settled(current.RefreshToken)
	if tx == nil {
		return nil
	}
	if err := c.save(tx, cred, current.RefreshToken); err != nil {
		return fmt.Errorf("persist refreshed credential: %w", err)
	}
	return nil
}

// save stores cred and keeps unsaved in step. spent is the refresh token cred replaced,
// or "" when cred replaced nothing.
func (c *Client) save(tx *auth.Tx, cred *auth.Credential, spent string) error {
	err := tx.Update(c.profile, cred)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case err == nil:
		c.unsaved = nil
	case spent != "":
		c.unsaved = append(c.unsaved, spent)
	}
	return err
}

// spent reports whether refreshToken is one this client spent but could not save past.
func (c *Client) spent(refreshToken string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.unsaved, refreshToken)
}

// refused records that the server refused current's refresh token, and removes current
// from tx while tx still stores it. A removal that fails is tried again at this client's
// next refresh, which ends here too. The keychain item stays (Tx.DeleteFor): read by
// nothing once its entry is gone, until the next sign-in overwrites it.
func (c *Client) refused(current *auth.Credential, tx *auth.Tx) error {
	c.mu.Lock()
	c.refusedToken = current.RefreshToken
	c.mu.Unlock()
	if tx != nil {
		_ = tx.DeleteFor(c.profile, current.OrganizationID, current.RefreshToken)
	}
	return errSessionExpired
}

// wasRefused reports whether the server refused refreshToken.
func (c *Client) wasRefused(refreshToken string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refusedToken != "" && c.refusedToken == refreshToken
}

// replace makes cred the client's credential.
func (c *Client) replace(cred *auth.Credential) {
	c.mu.Lock()
	c.credential = cred
	c.tokenGen++
	c.mu.Unlock()
}

// samePair reports whether two credentials hold the same tokens and expiry.
func samePair(a, b *auth.Credential) bool {
	return a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken && a.ExpiresAt.Equal(b.ExpiresAt)
}

// sameSession reports whether two credentials belong to one sign-in, possibly rotated.
// A credential saved before sessions had ids has no session id, and matches any.
func sameSession(a, b *auth.Credential) bool {
	return a.SessionID == "" || b.SessionID == "" || a.SessionID == b.SessionID
}

// RevokeSession ends the calling session server-side.
func (c *Client) RevokeSession(ctx context.Context) error {
	return c.AuthPost(ctx, "/v1/auth/cli/revoke", nil, nil)
}
