package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// AccountKeyLen is the length of an account key, in hex digits.
const AccountKeyLen = 16

// ValidAccountKey reports whether a is an account key: AccountKeyLen lowercase hex digits.
func ValidAccountKey(a string) bool {
	return len(a) == AccountKeyLen && strings.Trim(a, "0123456789abcdef") == ""
}

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

// Account is the account key of c's person and environment (AccountKey).
func (c *Credential) Account() string { return AccountKey(c.AuthURL, c.UserID) }

// Account is the account key of the sign-in a profile holds: "" when signed out,
// sendas.AccountUnreadable when user-credentials.json is there but can't be read, or its
// sign-in has no user id to key it by. The account does not depend on authURL: a sign-in for
// another environment keeps its own, which no token here and no server key sends for, so
// what is collected under it is that person's, never no one's. It reads only that file,
// never the keychain, so a hook can afford it.
func Account(dir, profile, authURL string) string {
	account, _ := AccountAt(dir, profile)
	return account
}

// ErrSignInRetired is the server's answer that it no longer accepts this kind of sign-in:
// only a newer terma, signed in again, sends again.
var ErrSignInRetired = errors.New("this sign-in is retired: run `terma update`, then `terma setup`")

// MarkRetired records, on the profile's stored sign-in of session, that the server refused
// it as retired, for doctor and status to say what fixes it. "" marks whatever sign-in is
// stored. A sign-in of another session, stored since, is not marked. It edits
// user-credentials.json alone, never the keychain, so a token another process rotated
// since, within the same session, or a locked keychain, still lets it be recorded.
func MarkRetired(dir, profile, session string) error {
	return lockedWrite(dir, func(tx *Tx) error { return tx.MarkRetired(profile, session) })
}

// MarkRetired is MarkRetired inside the transaction.
func (tx *Tx) MarkRetired(profile, session string) error {
	return tx.mutate(func(file credentialFile) {
		if c := file.get(profile); c != nil && c.RetiredAt.IsZero() && (session == "" || c.SessionID == session) {
			c.RetiredAt = time.Now().UTC()
		}
	})
}

// Retired reports whether the server refused the profile's sign-in as retired.
func Retired(dir, profile string) bool {
	cred, err := LoadIdentity(dir, profile)
	return err == nil && !cred.RetiredAt.IsZero()
}
