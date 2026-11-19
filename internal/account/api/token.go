package api

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
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
	req, err := c.newRequest(ctx, authHost, http.MethodPost, tokenPath, nil, nil, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	switch {
	case err != nil && unsent(err):
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
	// A success all the same, so the server may have spent the token.
	if out.AccessToken == "" {
		return nil, unsettledError{errors.New("token endpoint returned no access token")}
	}
	if body.GrantType == "refresh_token" && out.RefreshToken == "" {
		return nil, unsettledError{errors.New("token endpoint returned no refresh token")}
	}
	return &out, nil
}

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

// The server rotates the refresh token on every refresh, and revokes the session when a
// spent one comes back, except the one it spent last, within 60 s of spending it
// (refreshRetryGrace in mirador-platform's cli_session_refresh.go). So a refresh whose
// answer was lost sends the same token again, and every attempt must start within those
// 60 s. The rotation it repeats was committed after the first attempt was sent.
//
//   - refreshBudget bounds every attempt and pause together. 40 s leaves the last attempt
//     20 s inside the grace, and keeps the lock holder within auth's lockWait: 40 s plus
//     two keychain writes. A Client's exchangeBudget replaces it in tests.
//   - refreshPauses come before the second and third attempts, short enough that a hung
//     first attempt (the 30 s request timeout) still leaves time for a second.
const refreshBudget = 40 * time.Second

var refreshPauses = [...]time.Duration{time.Second, 2 * time.Second}

// exchangeRefreshToken redeems refreshToken. Until an attempt's outcome is unknown
// (unsettledError), the first answer is final: a refusal, or an attempt that spent nothing
// (unspentError), ends the exchange at once.
//
// Once one is unknown, the same token is sent again until an answer settles it: a pair,
// or the auth gateway's refusal twice in a row. Nothing else can. The server answers a
// refresh that lost a race to another rotation of the session with a refusal too, and the
// unknown attempt may be that rotation, committed late; sent once more, the token is the
// one it spent, which the server forgives. An exchange that runs out unsettled is no
// sign-out: the session may be alive, and must not be removed.
//
// It runs to the end whether or not ctx does: the server may rotate the pair after a
// caller gives up, and that pair must reach the store. So a caller's deadline, a relay's
// shutdown included, no longer bounds a refresh already sent; refreshBudget does.
func (c *Client) exchangeRefreshToken(ctx context.Context, refreshToken string) (*tokenResponse, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cmp.Or(c.exchangeBudget, refreshBudget))
	defer cancel()
	unknown, lastRefused := false, false
	for attempt := 0; ; attempt++ {
		resp, err := c.postToken(ctx, tokenRequest{GrantType: "refresh_token", RefreshToken: refreshToken})
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
		if attempt == len(refreshPauses) {
			return nil, stillUnknown(err)
		}
		pause := time.NewTimer(refreshPauses[attempt])
		select {
		case <-pause.C:
		case <-ctx.Done():
			pause.Stop()
			return nil, stillUnknown(err)
		}
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
func TokenSHA256(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

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
// auth's lockWait, which is longer than the exchange's refreshBudget. The waits for the
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
		return c.redeem(ctx, held, held, redeem, nil)
	}
	return auth.Locked(ctx, c.dir, func(tx *auth.Tx) error {
		stored, err := tx.LoadFor(c.profile, held.OrganizationID)
		switch {
		case err == nil && sameSession(stored, held):
		case err == nil || errors.Is(err, auth.ErrNotLoggedIn):
			// The developer signed out, or signed in again, since this client loaded its
			// pair. This session is no longer the stored one: rotate it in memory only.
			return c.redeem(ctx, held, held, redeem, nil)
		default:
			return err
		}
		if c.spent(stored.RefreshToken) {
			// The store still holds a token this client spent, because saving its
			// rotation failed. Keep the client's own live pair and try to save it now.
			_ = c.save(tx, held, "")
			return c.redeem(ctx, held, held, redeem, tx)
		}
		// Decide from the stored pair, not the held one. The held one was read without the
		// lock, and may pair a rotation's tokens with the expiry from before it.
		if !samePair(stored, held) {
			c.replace(stored)
		}
		return c.redeem(ctx, held, stored, redeem, tx)
	})
}

// redeem exchanges current's refresh token if redeem asks, and saves the new pair in tx
// when there is one. A token the auth gateway refused is never sent again. When tx still stores
// it, it is removed there too, so no process that reads the store afterwards sends it.
func (c *Client) redeem(ctx context.Context, held, current *auth.Credential, redeem redeemFunc, tx *auth.Tx) error {
	if !redeem(held, current) {
		return nil
	}
	if c.wasRefused(current.RefreshToken) {
		return c.refused(current, tx)
	}
	resp, err := c.exchangeRefreshToken(ctx, current.RefreshToken)
	switch {
	case errors.As(err, new(refusalError)):
		return c.refused(current, tx)
	case errors.Is(err, auth.ErrNotLoggedIn):
		return errSessionExpired
	}
	if err != nil {
		return err
	}
	cred := c.credentialFrom(resp)
	// A refresh keeps the session, its user and its label.
	cred.SessionID = cmp.Or(cred.SessionID, current.SessionID)
	cred.UserID = cmp.Or(cred.UserID, current.UserID)
	cred.Label = current.Label
	// The server has spent current's refresh token, so this pair is the session's only
	// live one. Hold it before trying to store it.
	c.replace(cred)
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
// from tx while tx still stores it. If the file entry stays, this client's next refresh
// ends here too and tries again. A keychain item left once the entry is gone is read by
// nothing, and the next sign-in to the organization overwrites it.
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
