package api

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
)

const (
	tokenPath  = "/v1/auth/user/token"
	revokePath = "/v1/auth/user/revoke"
	// legacyRevokePath ends a session an older terma signed in, with its access token.
	legacyRevokePath = "/v1/auth/cli/revoke"
)

// refreshAttemptBudget is how long after the first attempt a refresh that got no clear
// answer (a 503, or no response) is retried. The server may have rotated the pair without
// returning it, and it forgives a retry of the spent token for only 60 seconds; after
// that, presenting it is reuse, which revokes the whole family, this machine's and any
// Terma Desktop session it was delegated from. So past the budget the sign-in is over.
//
// Variables, so tests can shorten them.
var refreshAttemptBudget = 50 * time.Second

// defaultRetryAfter is the wait before retrying a 503 that named none.
var defaultRetryAfter = 5 * time.Second

// errSignInAgain is a refresh the server will not, or can no longer safely, complete.
var errSignInAgain = fmt.Errorf("%w: the sign-in could not be renewed; run `terma setup` to sign in again", auth.ErrNotLoggedIn)

type tokenRequest struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code,omitempty"`
	CodeVerifier string `json:"code_verifier,omitempty"`
	// RedirectPort is a pointer so a delegated code's 0 is sent, and a refresh sends none.
	RedirectPort   *int   `json:"redirect_port,omitempty"`
	InstallationID string `json:"installation_id,omitempty"`
	RefreshToken   string `json:"refresh_token,omitempty"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	SessionID    string `json:"session_id"`
	FamilyID     string `json:"family_id"`
	User         struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

// ExchangeCode implements auth.TokenExchanger.
func (c *Client) ExchangeCode(ctx context.Context, code, verifier string, port int) (*auth.Credential, error) {
	return c.exchangeCode(ctx, code, verifier, port)
}

// ExchangeDelegatedCode implements auth.TokenExchanger for a code Terma Desktop delegated
// from its own user session for this process's challenge. No listener took it, so
// redirect_port is 0; the auth host accepts 0 only for a code its own record says was
// delegated.
func (c *Client) ExchangeDelegatedCode(ctx context.Context, code, verifier string) (*auth.Credential, error) {
	return c.exchangeCode(ctx, code, verifier, 0)
}

func (c *Client) exchangeCode(ctx context.Context, code, verifier string, port int) (*auth.Credential, error) {
	if c.installationID == "" {
		return nil, errors.New("signing in needs this installation's id")
	}
	resp, err := c.postToken(ctx, tokenRequest{
		GrantType:      "authorization_code",
		Code:           code,
		CodeVerifier:   verifier,
		RedirectPort:   &port,
		InstallationID: c.installationID,
	})
	if err != nil {
		// A session may have been made whose answer was lost: nothing here can take it up.
		if apiErr, ok := errors.AsType[*Error](err); ok && (apiErr.Superseded() || apiErr.StatusCode >= 500) {
			return nil, fmt.Errorf("the sign-in did not complete (%w): run `terma setup` again", err)
		}
		return nil, err
	}
	return c.credentialFrom(resp), nil
}

func (c *Client) postToken(ctx context.Context, body tokenRequest) (*tokenResponse, error) {
	resp, err := c.attemptAs(ctx, authHost, http.MethodPost, tokenPath, body, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out tokenResponse
	if err := decode(resp, &out); err != nil {
		if apiErr, ok := errors.AsType[*Error](err); ok && apiErr.SignInRetired() {
			return nil, fmt.Errorf("%w: %w: %w", auth.ErrSignInRetired, auth.ErrNotLoggedIn, apiErr)
		}
		if apiErr, ok := errors.AsType[*Error](err); ok && apiErr.Unauthenticated() {
			return nil, fmt.Errorf("%w: %w", auth.ErrNotLoggedIn, apiErr)
		}
		return nil, err
	}
	if out.AccessToken == "" || out.RefreshToken == "" || out.SessionID == "" {
		return nil, errors.New("token endpoint returned an incomplete sign-in")
	}
	return &out, nil
}

func (c *Client) credentialFrom(resp *tokenResponse) *auth.Credential {
	return &auth.Credential{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second),
		SessionID:    resp.SessionID,
		FamilyID:     resp.FamilyID,
		AuthURL:      c.authURL,
		UserEmail:    resp.User.Email,
		UserID:       resp.User.ID,
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
// The server revokes the whole session family if a rotated refresh token is redeemed
// again after its 60-second grace, so every step runs under the credential lock:
//
//   - reload the stored pair;
//   - take it if another process has already rotated past this client's pair;
//   - redeem only if redeem still asks for it;
//   - save the new pair before the lock is released.
//
// The lock is held through the exchange, its retries and the keychain writes. So the wait
// for it is auth's lockWait, which is longer than refreshAttemptBudget.
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
		stored, err := tx.Load(c.profile)
		switch {
		case err == nil && stored.SessionID == held.SessionID:
		case err == nil || errors.Is(err, auth.ErrNotLoggedIn):
			// The developer signed out, or signed in again, since this client loaded its
			// pair. A machine keeps one session: this one is over, and its token is never
			// presented again, since signing in again revokes it with that token.
			return fmt.Errorf("%w: this machine signed out or in again since", auth.ErrNotLoggedIn)
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

// keepRotation holds and stores the new pair a refresh of current returned.
func (c *Client) keepRotation(tx *auth.Tx, current, cred *auth.Credential) error {
	// A refresh keeps the session, its user and its label.
	cred.SessionID = cmp.Or(cred.SessionID, current.SessionID)
	cred.FamilyID = cmp.Or(cred.FamilyID, current.FamilyID)
	cred.UserID = cmp.Or(cred.UserID, current.UserID)
	cred.UserEmail = cmp.Or(cred.UserEmail, current.UserEmail)
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

// endSignIn gives up on current's refresh token, which must never be presented again, and
// forgets it while it is still the stored one.
func (c *Client) endSignIn(tx *auth.Tx, current *auth.Credential) error {
	if tx != nil {
		_ = tx.DeleteIf(c.profile, current.RefreshToken)
	}
	return errSignInAgain
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
