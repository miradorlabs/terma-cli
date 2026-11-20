package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// AccountKeyLen is the length of an account key, in hex digits.
const AccountKeyLen = 16

// AccountKey names the person and environment a sign-in belongs to, without naming either:
// the start of the SHA-256 of the auth host and the user id. What terma queues is bound to
// the account that collected it, so a backlog is never sent under another person's name or
// to another environment. "" for no user.
func AccountKey(authURL, userID string) string {
	if authURL == "" || userID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(authURL + "\n" + userID))
	return hex.EncodeToString(sum[:])[:AccountKeyLen]
}

// Account is the account key of the sign-in a profile holds for authURL: "" when signed
// out or signed in to another environment. It reads only user-credentials.json, never the
// keychain, so a hook can afford it.
func Account(dir, profile, authURL string) string {
	cred, err := LoadIdentity(dir, profile)
	if err != nil || cred.CheckEnvironment(authURL) != nil {
		return ""
	}
	return AccountKey(cred.AuthURL, cred.UserID)
}

// ErrSignInRetired is the server's answer that it no longer accepts this kind of sign-in:
// only a newer terma, signed in again, sends again.
var ErrSignInRetired = errors.New("this sign-in is retired: update terma and run `terma login`")

// MarkRetired records, on the profile's stored sign-in whose refresh token is refreshToken,
// that the server refused it as retired, for doctor and status to say what fixes it. ""
// marks whatever sign-in is stored. A sign-in stored since is not marked.
func MarkRetired(dir, profile, refreshToken string) error {
	return lockedWrite(dir, func(tx *Tx) error { return tx.MarkRetired(profile, refreshToken) })
}

// MarkRetired is MarkRetired inside the transaction.
func (tx *Tx) MarkRetired(profile, refreshToken string) error {
	if refreshToken != "" {
		stored, err := tx.Load(profile)
		if err != nil || stored.RefreshToken != refreshToken {
			return nil
		}
	}
	return tx.mutate(func(file credentialFile) {
		if c := file.get(profile); c != nil && c.RetiredAt.IsZero() {
			c.RetiredAt = time.Now().UTC()
		}
	})
}

// Retired reports whether the server refused the profile's sign-in as retired.
func Retired(dir, profile string) bool {
	cred, err := LoadIdentity(dir, profile)
	return err == nil && !cred.RetiredAt.IsZero()
}
