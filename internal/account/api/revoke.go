package api

import (
	"context"
	"net/http"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
)

// Enforcement is the server's answer to a revoke: ENFORCED when every gateway already
// refuses the revoked session's access token, PENDING when that takes up to a minute more.
type Enforcement string

// The answers a revoke gives.
const (
	Enforced Enforcement = "ENFORCED"
	Pending  Enforcement = "PENDING"
)

// RevokeRefreshToken ends the session refreshToken belongs to at the auth host authURL
// (RFC 7009): the token is the credential, so none other is sent. The caller deletes its
// stored copy whatever the answer says.
func RevokeRefreshToken(ctx context.Context, authURL, version, refreshToken string) (Enforcement, error) {
	c := NewAnonymous(authURL, version)
	var out struct {
		Enforcement Enforcement `json:"enforcement"`
	}
	resp, err := c.attemptAs(ctx, authHost, http.MethodPost, revokePath, struct {
		RefreshToken string `json:"refresh_token"`
	}{refreshToken}, false)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := decode(resp, &out); err != nil {
		return "", err
	}
	return out.Enforcement, nil
}

// RevokeLegacySession ends, best-effort, a session an older terma signed in to one
// organization, with its access token: such sessions are retired, and an expired token
// ends nothing, which only leaves the session to expire.
func RevokeLegacySession(ctx context.Context, authURL, version, accessToken string) error {
	c := NewAnonymous(authURL, version)
	c.credential = &auth.Credential{AccessToken: accessToken}
	resp, err := c.attemptAs(ctx, authHost, http.MethodPost, legacyRevokePath, nil, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp, nil)
}
