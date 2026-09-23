package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/miradorlabs/terma-cli/internal/auth"
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

// LoginResult is a completed sign-in. The organization and user names ride alongside
// the credential so `terma whoami` and the login summary can print them without
// another round trip.
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

// LastLogin returns the organization/user detail from the most recent exchange on
// this client, so the login command can print who it signed in as.
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
		var apiErr *APIError
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
	return c.refreshIfCurrent(ctx, gen)
}

// refreshIfCurrent redeems the refresh token only if no other goroutine has already
// rotated past the given generation. It serializes on refreshMu, so two racing callers
// produce exactly one token exchange: the first rotates and bumps the generation, and
// the second — now observing a newer generation — returns without redeeming the same
// refresh token again (which reuse detection would read as theft and log everyone out).
func (c *Client) refreshIfCurrent(ctx context.Context, gen uint64) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if c.currentGen() != gen {
		return nil
	}
	return c.refresh(ctx)
}

// refresh rotates the token pair and persists it. It must be called with refreshMu
// held (via refreshIfCurrent) so only one rotation is ever in flight. Persisting
// immediately matters: the server has already invalidated the old refresh token, so
// losing the new one to a crash would strand the session even though the user is still
// authorized.
func (c *Client) refresh(ctx context.Context) error {
	c.mu.Lock()
	if c.credential == nil || c.credential.RefreshToken == "" {
		c.mu.Unlock()
		return auth.ErrNotLoggedIn
	}
	refreshToken := c.credential.RefreshToken
	c.mu.Unlock()

	resp, err := c.postToken(ctx, tokenRequest{GrantType: "refresh_token", RefreshToken: refreshToken})
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return fmt.Errorf("%w: session expired, run `terma login`", auth.ErrNotLoggedIn)
		}
		return err
	}

	cred := c.credentialFrom(resp)
	c.mu.Lock()
	c.credential = cred
	c.tokenGen++
	c.mu.Unlock()

	if c.profile != "" {
		if err := auth.UpdateCredential(c.profile, cred); err != nil {
			return fmt.Errorf("persist refreshed credential: %w", err)
		}
	}
	return nil
}

// RevokeSession ends the calling session server-side. Called by `terma logout`
// before the local credential file is cleared.
func (c *Client) RevokeSession(ctx context.Context) error {
	return c.AuthPost(ctx, "/v1/auth/cli/revoke", nil, nil)
}
