package api

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
)

const tokenPath = "/v1/auth/cli/token"

type tokenRequest struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code,omitempty"`
	CodeVerifier string `json:"code_verifier,omitempty"`
	RedirectPort int    `json:"redirect_port,omitempty"`
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
	resp, err := c.postToken(ctx, tokenRequest{
		GrantType:    "authorization_code",
		Code:         code,
		CodeVerifier: verifier,
		RedirectPort: port,
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
	resp, err := c.attempt(ctx, authHost, http.MethodPost, tokenPath, nil, nil, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out tokenResponse
	if err := decode(resp, &out); err != nil {
		var apiErr *Error
		if errors.As(err, &apiErr) && apiErr.Unauthenticated() {
			return nil, fmt.Errorf("%w: %s", auth.ErrNotLoggedIn, apiErr.Message)
		}
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, errors.New("token endpoint returned no access token")
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
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if c.currentGen() != gen {
		return nil
	}
	return c.refresh(ctx, redeem)
}

// Fresh returns the credential with an access token that is not about to expire,
// refreshing it first when it is.
func (c *Client) Fresh(ctx context.Context) (*auth.Credential, error) {
	if err := c.refreshIfNeeded(ctx); err != nil {
		return nil, err
	}
	return c.Credential(), nil
}

// RefreshRefused rotates the pair although its access token has not expired, because a
// server refused it, but only while the stored access token is still that one, whose
// SHA-256 is refusedSHA256 in hex: every process that saw the one refusal shares a
// single rotation.
func (c *Client) RefreshRefused(ctx context.Context, refusedSHA256 string) (*auth.Credential, error) {
	if !c.canRefresh() {
		return nil, auth.ErrNotLoggedIn
	}
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	err := c.refresh(ctx, func(_, current *auth.Credential) bool {
		return TokenSHA256(current.AccessToken) == strings.ToLower(refusedSHA256) || current.Expired()
	})
	if err != nil {
		return nil, err
	}
	return c.Credential(), nil
}

// TokenSHA256 is how a token is named where the token itself must not appear: its
// SHA-256, in lowercase hex.
func TokenSHA256(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// redeemFunc decides, given the credential this client held and the pair as it now
// stands, which another process may have rotated, whether to redeem the refresh token.
type redeemFunc func(held, current *auth.Credential) bool

// refresh brings the token pair up to date as one transaction across terma processes,
// since the server's reuse detection revokes the whole session when a rotated refresh
// token is redeemed twice: under the credential lock it reloads the stored pair, takes it
// when another process has already rotated past this client's, redeems only if redeem
// still asks for it, and persists the new pair before the lock is let go. The lock is
// held through the exchange, so waiting for it is bounded as the exchange is. Call it
// with refreshMu held.
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
	lockCtx, cancel := context.WithTimeout(ctx, c.http.Timeout)
	defer cancel()
	return auth.Locked(lockCtx, c.dir, func(tx *auth.Tx) error {
		stored, err := tx.LoadFor(c.profile, held.OrganizationID)
		switch {
		case err == nil && sameSession(stored, held):
		case err == nil || errors.Is(err, auth.ErrNotLoggedIn):
			// Signed out, or signed in afresh, since: this session is no longer the
			// stored one, so a rotation stays in memory and replaces nothing.
			return c.redeem(ctx, held, held, redeem, nil)
		default:
			return err
		}
		current := held
		if stored.AccessToken != held.AccessToken {
			current = stored
			c.replace(stored)
		}
		return c.redeem(ctx, held, current, redeem, tx)
	})
}

// redeem exchanges current's refresh token if redeem asks, persisting the new pair in tx
// when there is one.
func (c *Client) redeem(ctx context.Context, held, current *auth.Credential, redeem redeemFunc, tx *auth.Tx) error {
	if !redeem(held, current) {
		return nil
	}
	resp, err := c.postToken(ctx, tokenRequest{GrantType: "refresh_token", RefreshToken: current.RefreshToken})
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return fmt.Errorf("%w: session expired, run `terma setup`", auth.ErrNotLoggedIn)
		}
		return err
	}
	cred := c.credentialFrom(resp)
	// A refresh keeps the session and its user.
	cred.SessionID = cmp.Or(cred.SessionID, current.SessionID)
	cred.UserID = cmp.Or(cred.UserID, current.UserID)
	c.replace(cred)
	if tx != nil {
		if err := tx.Update(c.profile, cred); err != nil {
			return fmt.Errorf("persist refreshed credential: %w", err)
		}
	}
	return nil
}

// replace makes cred the client's credential.
func (c *Client) replace(cred *auth.Credential) {
	c.mu.Lock()
	c.credential = cred
	c.tokenGen++
	c.mu.Unlock()
}

// sameSession reports whether two credentials are one sign-in, possibly rotated; one
// saved before sessions were named is taken to be.
func sameSession(a, b *auth.Credential) bool {
	return a.SessionID == "" || b.SessionID == "" || a.SessionID == b.SessionID
}

// RevokeSession ends the calling session server-side.
func (c *Client) RevokeSession(ctx context.Context) error {
	return c.AuthPost(ctx, "/v1/auth/cli/revoke", nil, nil)
}
