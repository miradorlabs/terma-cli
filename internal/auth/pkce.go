package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// PKCE holds one login attempt's proof-of-possession pair. The verifier never
// leaves this process; only the challenge travels through the browser, so a code
// intercepted in the URL bar or a browser history entry is not redeemable.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE generates an RFC 7636 S256 pair. 32 random bytes hex-encode to 64
// characters, inside the spec's 43-128 range.
func NewPKCE() (*PKCE, error) {
	verifier, err := randomHex(32)
	if err != nil {
		return nil, fmt.Errorf("generate code verifier: %w", err)
	}
	sum := sha256.Sum256([]byte(verifier))
	return &PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// newState generates the CSRF value echoed back through the redirect. The CLI
// refuses a callback whose state does not match, so another page cannot drive a
// code into this listener.
func newState() (string, error) {
	return randomHex(16)
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
