package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// Agents keep presenting the relay token they were launched with until they restart, and
// a desktop app may run for weeks. So teardown keeps the token aside for the sign-in it
// served, and setup under that same sign-in restores it instead of minting another: a
// teardown and setup refuse nothing an agent still running sends. Removing the token
// still stops collection, since without it no relay runs and no hook claims a session.

// NewToken is a fresh relay token.
func NewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// maxReplaced bounds the tokens a moved relay keeps for teardown (Move.Replaced).
const maxReplaced = 8

// rotateToken gives the relay of stateDir, whose directory is dir, a new token and returns
// it, keeping the one it replaced beside the move (Move.Replaced): an exporter the relay
// could not re-point still holds it, and teardown finds what it wrote by it.
func rotateToken(stateDir, dir string) (string, error) {
	old, _ := Token(stateDir)
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	if err := config.WriteFileAtomic(claim.TokenPath(stateDir), []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	if m, ok := LastMove(dir); ok && old != "" {
		m.Replaced = append(m.Replaced, old)
		m.Replaced = m.Replaced[max(0, len(m.Replaced)-maxReplaced):]
		writeMove(dir, m)
	}
	return token, nil
}

// RetiredTokenFile is the token teardown removed and the sign-in it served, in the
// state directory.
const RetiredTokenFile = "retired-relay-token.json"

type retiredToken struct {
	Token          string    `json:"token"`
	OrganizationID string    `json:"organization_id"`
	AuthURL        string    `json:"auth_url"`
	RetiredAt      time.Time `json:"retired_at"`
}

// Identity is the sign-in a relay token serves: cfg's organization, as the relay resolves
// it (Prepare), and its auth service.
func Identity(cfg *config.Config) (org, authURL string) {
	org = cfg.OrganizationID
	if org == "" {
		if cred, err := auth.LoadIdentity(cfg.Dir, cfg.ProfileName); err == nil && cred.CheckEnvironment(cfg.AuthURL) == nil {
			org = cred.OrganizationID
		}
	}
	return org, cfg.AuthURL
}

// RetireToken removes the relay's token under the state directory stateDir, keeping it
// aside for org at authURL; without an organization it is only removed. With no token there
// is nothing to do.
func RetireToken(stateDir, org, authURL string) error {
	token, err := Token(stateDir)
	if err != nil {
		return nil
	}
	if org != "" && authURL != "" && token != "" {
		data, err := json.Marshal(retiredToken{Token: token, OrganizationID: org, AuthURL: authURL, RetiredAt: time.Now().UTC()})
		if err != nil {
			return err
		}
		if err := config.WriteFileAtomic(filepath.Join(stateDir, RetiredTokenFile), append(data, '\n'), 0o600); err != nil {
			return err
		}
	}
	if err := os.Remove(claim.TokenPath(stateDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ReviveToken restores the token RetireToken kept for org at authURL as the relay's, and
// returns it. A token kept for another sign-in is discarded, never restored.
func ReviveToken(stateDir, org, authURL string) (string, bool) {
	retired := filepath.Join(stateDir, RetiredTokenFile)
	data, err := os.ReadFile(retired)
	if err != nil {
		return "", false
	}
	var r retiredToken
	ok := json.Unmarshal(data, &r) == nil && org != "" && r.OrganizationID == org && r.AuthURL == authURL &&
		r.Token != "" && !strings.ContainsAny(r.Token, " \t\r\n/")
	if !ok {
		_ = os.Remove(retired)
		return "", false
	}
	path := claim.TokenPath(stateDir)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil ||
		config.WriteFileAtomic(path, []byte(r.Token+"\n"), 0o600) != nil {
		return "", false
	}
	_ = os.Remove(retired)
	return r.Token, true
}

// DiscardRetiredToken forgets a retired token: signing out ends the sign-in it served.
func DiscardRetiredToken(stateDir string) error {
	if err := os.Remove(filepath.Join(stateDir, RetiredTokenFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
