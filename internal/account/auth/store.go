// Package auth implements the CLI side of the Terma PKCE loopback login: the
// browser handoff, the code exchange, credential storage, and transparent refresh.
//
// A profile holds one sign-in: a user session of the person who signed in, which reads
// every organization and team they belong to. user-credentials.json indexes each
// profile's; its tokens live in the system keychain (internal/account/secret), or in the
// file itself where there is none or `terma setup --insecure-storage` asked for it, as gh
// keeps its token. A terma before user tokens kept one sign-in per organization in
// credentials.json; signing in deletes them (legacy.go).
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// ErrNotLoggedIn is returned when there is no usable credential.
var ErrNotLoggedIn = errors.New("not logged in")

// Credential is one profile's sign-in: a user session's tokens.
type Credential struct {
	// The tokens are empty on disk when the keychain holds them.
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	SessionID    string    `json:"session_id"`
	// FamilyID is the session's own id, or the id of the Terma Desktop session that
	// delegated it.
	FamilyID string `json:"family_id,omitempty"`
	// AuthURL is the host that minted this credential, the only one it is valid to.
	AuthURL   string `json:"auth_url"`
	UserEmail string `json:"user_email,omitempty"`
	// UserID is the Mirador user who signed in.
	UserID string `json:"user_id"`
	// Label is the session label a desktop grant asked for (DesktopLabel), kept so doctor
	// can say the sign-in came from Terma Desktop; "" for a browser sign-in.
	Label string `json:"label,omitempty"`
	// RefreshInDoubtSince is when a refresh of RefreshToken was first sent without an
	// answer that said whether it rotated (a 503 or a lost response). The server forgives a
	// retry of a spent token for 60 seconds and treats a later one as reuse, which revokes
	// the whole family, so no process presents it again once refreshAttemptBudget has
	// passed: the sign-in is over instead. Zero when no refresh is in doubt.
	RefreshInDoubtSince time.Time `json:"refresh_in_doubt_since,omitzero"`
	// RefreshInDoubtFor is the SHA-256, in hex, of the refresh token RefreshInDoubtSince
	// was recorded for. A mark beside another token (a rotation the keychain took and the
	// file then missed) is not that token's, and is read as none. "" (a mark from before
	// it) counts for whatever token is stored.
	RefreshInDoubtFor string `json:"refresh_in_doubt_for,omitempty"`
	// StaleKeychainItem marks a credential kept in the file while its earlier tokens may
	// still sit in the keychain, which could not remove them then; sign-out tries again.
	StaleKeychainItem bool `json:"stale_keychain_item,omitempty"`
}

// dropForeignMark forgets a refresh mark recorded for another refresh token than c's.
func (c *Credential) dropForeignMark() {
	if c.RefreshInDoubtFor != "" && c.RefreshInDoubtFor != tokenHash(c.RefreshToken) {
		c.RefreshInDoubtSince, c.RefreshInDoubtFor = time.Time{}, ""
	}
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ErrWrongEnvironment reports a credential issued by a different auth host than the
// one currently configured.
type ErrWrongEnvironment struct {
	IssuedBy     string
	ConfiguredAs string
}

func (e *ErrWrongEnvironment) Error() string {
	return fmt.Sprintf(
		"this profile is logged in against %s but is now pointed at %s — run `terma setup` to sign in for this environment",
		e.IssuedBy, e.ConfiguredAs)
}

// CheckEnvironment reports whether the credential belongs to authURL.
func (c *Credential) CheckEnvironment(authURL string) error {
	if c.AuthURL == "" {
		return ErrNotLoggedIn
	}
	if c.AuthURL == authURL {
		return nil
	}
	return &ErrWrongEnvironment{IssuedBy: c.AuthURL, ConfiguredAs: authURL}
}

// Expired reports whether the access token is spent, or will be within a minute.
func (c *Credential) Expired() bool {
	const skew = 60 * time.Second
	return c.ExpiresAt.IsZero() || time.Now().Add(skew).After(c.ExpiresAt)
}

// present reports whether the file indexes c, whose tokens may be in the keychain.
func (c *Credential) present() bool { return c != nil && c.SessionID != "" }

// credentialFile is user-credentials.json: each profile's sign-in.
type credentialFile map[string]*Credential

func (f credentialFile) get(profile string) *Credential {
	c := f[profile]
	if !c.present() {
		return nil
	}
	return c
}

// LoadCredential returns a profile's sign-in, or ErrNotLoggedIn.
func LoadCredential(dir, profile string) (*Credential, error) {
	file, err := loadCredentialFile(dir)
	if err != nil {
		return nil, err
	}
	cred := file.get(profile)
	if cred == nil {
		return nil, ErrNotLoggedIn
	}
	if err := hydrate(dir, profile, cred); err != nil {
		return nil, err
	}
	return cred, nil
}

// LoadIdentity returns the profile's sign-in without its tokens: who signed in, to which
// auth host. It reads only user-credentials.json, never the keychain.
func LoadIdentity(dir, profile string) (*Credential, error) {
	file, err := loadCredentialFile(dir)
	if err != nil {
		return nil, err
	}
	cred := file.get(profile)
	if cred == nil {
		return nil, ErrNotLoggedIn
	}
	cred.AccessToken, cred.RefreshToken = "", ""
	return cred, nil
}

// StoredInFile reports whether the profile's sign-in keeps its tokens in
// user-credentials.json rather than the system keychain.
func StoredInFile(dir, profile string) bool {
	file, err := loadCredentialFile(dir)
	if err != nil {
		return false
	}
	cred := file.get(profile)
	return cred != nil && cred.AccessToken != ""
}

// SaveCredential stores a new sign-in as the profile's, returning the one it replaced
// for the caller to revoke, once the new one is stored: a machine keeps one session, and a
// crash in between leaves the old one live but unused rather than the machine signed out.
// Signing in decides where the tokens live: the system keychain, unless there is none or
// config.json asks for files (InsecureStorage).
func SaveCredential(dir, profile string, cred *Credential) (replaced *Credential, err error) {
	err = lockedWrite(dir, func(tx *Tx) error {
		replaced, err = tx.saveReplacing(profile, cred, config.InsecureStorage(dir))
		return err
	})
	if replaced != nil && replaced.SessionID == cred.SessionID {
		replaced = nil
	}
	return replaced, err
}

// saveReplacing stores cred as profile's sign-in, keeping its tokens in the file when
// inFile, and returns the sign-in it replaced, with its tokens when they can be read.
func (tx *Tx) saveReplacing(profile string, cred *Credential, inFile bool) (replaced *Credential, err error) {
	dir := tx.dir
	var unindexed bool
	// overwritten is the sign-in whose keychain item the new tokens replaced: a file that
	// then fails to save still indexes it, so its tokens go back.
	var overwritten *Credential
	var unread error
	err = tx.mutateIf(func(file credentialFile) bool {
		// Read before the new tokens overwrite the keychain item.
		old := file.get(profile)
		var indexed *Credential
		if old != nil {
			copied := *old
			indexed = &copied
			switch err := hydrate(dir, profile, old); {
			case err == nil:
				replaced = old
			case !errors.Is(err, errTokensGone) && !inFile && old.SessionID != cred.SessionID:
				// The keychain did not answer: its item may still hold the only copy of
				// another live session's tokens, which the new ones must not overwrite
				// unread. (Kept in the file, the new ones overwrite nothing.)
				unread = err
				return false
			}
		}
		kept := keep(dir, profile, cred, indexed, inFile)
		unindexed = old == nil && kept.AccessToken == ""
		if replaced != nil && indexed.AccessToken == "" {
			overwritten = replaced
		}
		file[profile] = kept
		return true
	})
	if err == nil && unread != nil {
		err = fmt.Errorf("read the sign-in it replaces from the system keychain: %w", unread)
	}
	if err != nil && overwritten != nil {
		pair, _ := json.Marshal(tokenPair{AccessToken: overwritten.AccessToken, RefreshToken: overwritten.RefreshToken, SessionID: overwritten.SessionID})
		_ = secret.Set(dir, itemName(profile), string(pair))
	}
	tx.after = append(tx.after, func() { afterSave(dir, profile, unindexed, err) })
	return replaced, err
}

// UpdateCredential persists a refreshed credential, keeping its tokens where they already
// are; a keychain that fails gets the new pair written to the file, so a rotated token is
// never lost.
func UpdateCredential(dir, profile string, cred *Credential) error {
	return lockedWrite(dir, func(tx *Tx) error { return tx.Update(profile, cred) })
}

// DeleteCredential forgets a profile's sign-in — logout.
func DeleteCredential(dir, profile string) error {
	var gone *Credential
	err := mutateCredentialFile(dir, func(file credentialFile) {
		gone = file.get(profile)
		delete(file, profile)
	})
	if err != nil {
		return err
	}
	return forget(dir, profile, gone)
}

// DeleteCredentialIf forgets a profile's sign-in only while its refresh token is still
// refreshToken: a sign-in another process stored since is not this caller's to forget.
func DeleteCredentialIf(dir, profile, refreshToken string) error {
	return lockedWrite(dir, func(tx *Tx) error { return tx.DeleteIf(profile, refreshToken) })
}

// DeleteIf is DeleteCredentialIf inside the transaction. It removes the sign-in from
// user-credentials.json alone, as v0.3.1's DeleteFor did: a keychain delete that timed out
// can still land after the lock is released, and would then remove the tokens a new
// sign-in just wrote under the same item name. The item left is overwritten by the next
// sign-in to the keychain. Signing out, which means to remove it, uses SignOutIf.
func (tx *Tx) DeleteIf(profile, refreshToken string) error {
	_, err := tx.deleteIf(profile, refreshToken)
	return err
}

// SignOutIf is DeleteIf for signing out: the sign-in's keychain item goes too.
func (tx *Tx) SignOutIf(profile, refreshToken string) error {
	gone, err := tx.deleteIf(profile, refreshToken)
	if err != nil {
		return err
	}
	return forget(tx.dir, profile, gone)
}

func (tx *Tx) deleteIf(profile, refreshToken string) (*Credential, error) {
	stored, err := tx.Load(profile)
	switch {
	case errors.Is(err, ErrNotLoggedIn):
		return nil, nil
	case err != nil:
		return nil, err
	case stored.RefreshToken != refreshToken:
		return nil, nil
	}
	var gone *Credential
	err = tx.mutate(func(file credentialFile) {
		gone = file.get(profile)
		delete(file, profile)
	})
	return gone, err
}

// mutateCredentialFile runs mutate under a cross-process lock: unlocked, a second
// concurrent writer erases the first's entry.
func mutateCredentialFile(dir string, mutate func(credentialFile)) error {
	return lockedWrite(dir, func(tx *Tx) error { return tx.mutate(mutate) })
}

func loadCredentialFile(dir string) (credentialFile, error) {
	path := config.CredentialsPath(dir)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return credentialFile{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	var file credentialFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if file == nil {
		file = credentialFile{}
	}
	return file, nil
}

func saveCredentialFile(path string, file credentialFile) error {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	// 0600: this file may hold live access and refresh tokens.
	return writeCredentialFile(path, append(data, '\n'), 0o600)
}

// writeCredentialFile writes user-credentials.json; a test can make it fail alone.
var writeCredentialFile = config.WriteFileAtomic
