package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// refreshIfCurrent redeems the refresh token only if no other goroutine has rotated past
// gen, so racing callers make exactly one exchange.
func (c *Client) refreshIfCurrent(ctx context.Context, gen uint64) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if c.currentGen() != gen {
		return nil
	}
	return c.refresh(ctx)
}

// refresh rotates the token pair and persists it at once, since the server has already
// invalidated the old refresh token; call it with refreshMu held.
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
			return fmt.Errorf("%w: session expired, run `terma setup`", auth.ErrNotLoggedIn)
		}
		return err
	}

	cred := c.credentialFrom(resp)
	c.mu.Lock()
	c.credential = cred
	c.tokenGen++
	c.mu.Unlock()

	if c.profile != "" {
		if err := auth.UpdateCredential(c.dir, c.profile, cred); err != nil {
			return fmt.Errorf("persist refreshed credential: %w", err)
		}
	}
	return nil
}

// RevokeSession ends the calling session server-side.
func (c *Client) RevokeSession(ctx context.Context) error {
	return c.AuthPost(ctx, "/v1/auth/cli/revoke", nil, nil)
}
