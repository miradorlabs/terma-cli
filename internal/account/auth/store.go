// Package auth implements the CLI side of the Terma PKCE loopback login: the
// browser handoff, the code exchange, credential storage, and transparent refresh.
//
// credentials.json indexes each profile's credentials; their tokens live in the system
// keychain (internal/account/secret), or in the file itself where there is none or
// `terma setup --insecure-storage` asked for it, as gh keeps its token.
package auth

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// ErrNotLoggedIn is returned when there is no usable credential.
var ErrNotLoggedIn = errors.New("not logged in")

// Credential is one profile's stored tokens for one organization.
type Credential struct {
	// The tokens are empty on disk when the keychain holds them.
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	SessionID    string    `json:"session_id,omitempty"`
	// AuthURL is the host that minted this credential, the only one it is valid to.
	AuthURL        string `json:"auth_url,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
	UserEmail      string `json:"user_email,omitempty"`
	// UserID is the Mirador user who signed in, as every token exchange names them: a
	// credential saved before it was kept gains it at its next refresh.
	UserID string `json:"user_id,omitempty"`
	// StaleKeychainItem marks a credential kept in the file while its earlier tokens may
	// still sit in the keychain, which could not remove them then; sign-out tries again.
	StaleKeychainItem bool `json:"stale_keychain_item,omitempty"`
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

// profileCredentials keeps one credential per organization, so switching organizations
// reuses an approved session instead of a second browser handoff.
type profileCredentials struct {
	Active        string                 `json:"active,omitempty"`
	Organizations map[string]*Credential `json:"organizations"`
}

func (p *profileCredentials) active() *Credential {
	if p == nil || p.Active == "" {
		return nil
	}
	cred := p.Organizations[p.Active]
	if !cred.present() {
		return nil
	}
	return cred
}

// present reports whether the file indexes c, whose tokens may be in the keychain.
func (c *Credential) present() bool { return c != nil && c.OrganizationID != "" }

func (p *profileCredentials) put(cred *Credential) {
	if p.Organizations == nil {
		p.Organizations = map[string]*Credential{}
	}
	p.Organizations[cred.OrganizationID] = cred
}

type credentialFile map[string]*profileCredentials

// LoadCredential returns the active credential for a profile, or ErrNotLoggedIn.
func LoadCredential(dir, profile string) (*Credential, error) {
	file, err := loadCredentialFile(dir)
	if err != nil {
		return nil, err
	}
	cred := file[profile].active()
	if cred == nil {
		return nil, ErrNotLoggedIn
	}
	if err := hydrate(dir, profile, cred); err != nil {
		return nil, err
	}
	return cred, nil
}

// LoadIdentity returns the profile's active credential without its tokens: who signed in,
// to which organization and auth host. It reads only credentials.json, never the keychain.
func LoadIdentity(dir, profile string) (*Credential, error) {
	file, err := loadCredentialFile(dir)
	if err != nil {
		return nil, err
	}
	cred := file[profile].active()
	if cred == nil {
		return nil, ErrNotLoggedIn
	}
	cred.AccessToken, cred.RefreshToken = "", ""
	return cred, nil
}

// StoredInFile reports whether the profile's active credential keeps its tokens in
// credentials.json rather than the system keychain.
func StoredInFile(dir, profile string) bool {
	file, err := loadCredentialFile(dir)
	if err != nil {
		return false
	}
	cred := file[profile].active()
	return cred != nil && cred.AccessToken != ""
}

// LoadCredentialFor returns the profile's credential for one organization, active or
// not, or ErrNotLoggedIn when the profile never signed into it.
func LoadCredentialFor(dir, profile, organizationID string) (*Credential, error) {
	file, err := loadCredentialFile(dir)
	if err != nil {
		return nil, err
	}
	p := file[profile]
	if p == nil {
		return nil, ErrNotLoggedIn
	}
	cred := p.Organizations[organizationID]
	if !cred.present() {
		return nil, ErrNotLoggedIn
	}
	if err := hydrate(dir, profile, cred); err != nil {
		return nil, err
	}
	return cred, nil
}

// Credentials lists every credential a profile holds, the active one first. A
// credential whose tokens are gone is left out; an unreachable keychain is an error.
func Credentials(dir, profile string) ([]*Credential, error) {
	file, err := loadCredentialFile(dir)
	if err != nil {
		return nil, err
	}
	p := file[profile]
	if p == nil {
		return nil, nil
	}
	keys := make([]string, 0, len(p.Organizations))
	for key, cred := range p.Organizations {
		if cred.present() {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if (keys[i] == p.Active) != (keys[j] == p.Active) {
			return keys[i] == p.Active
		}
		return keys[i] < keys[j]
	})
	out := make([]*Credential, 0, len(keys))
	for _, key := range keys {
		cred := p.Organizations[key]
		switch err := hydrate(dir, profile, cred); {
		case errors.Is(err, ErrNotLoggedIn):
			continue
		case err != nil:
			return nil, err
		}
		out = append(out, cred)
	}
	return out, nil
}

// SaveCredential stores and activates a credential, returning the session it displaced
// for the caller to revoke. Signing in decides where the tokens live: the system
// keychain, unless there is none or config.json asks for files (InsecureStorage).
func SaveCredential(dir, profile string, cred *Credential) (replaced *Credential, err error) {
	err = lockedWrite(dir, func(tx *Tx) error {
		replaced, err = tx.saveReplacing(profile, cred)
		return err
	})
	if replaced != nil && replaced.SessionID == cred.SessionID {
		replaced = nil
	}
	return replaced, err
}

func (tx *Tx) save(profile string, cred *Credential) error {
	_, err := tx.saveReplacing(profile, cred)
	return err
}

func (tx *Tx) saveReplacing(profile string, cred *Credential) (replaced *Credential, err error) {
	dir := tx.dir
	inFile := config.InsecureStorage(dir)
	var unindexed bool
	err = tx.mutate(func(file credentialFile) {
		p := file[profile]
		if p == nil {
			p = &profileCredentials{}
			file[profile] = p
		}
		// Read before the new tokens overwrite the keychain item.
		old := p.Organizations[cred.OrganizationID]
		var indexed *Credential
		if old.present() {
			copied := *old
			indexed = &copied
			if hydrate(dir, profile, old) == nil {
				replaced = old
			}
		}
		kept := keep(dir, profile, cred, indexed, inFile)
		unindexed = !old.present() && kept.AccessToken == ""
		p.put(kept)
		p.Active = cred.OrganizationID
	})
	tx.after = append(tx.after, func() { afterSave(dir, profile, cred.OrganizationID, unindexed, err) })
	return replaced, err
}

// ResaveCredential activates and saves a session read from the store earlier, as
// SaveCredential does. Under the lock it keeps the pair stored now, not cred's. Another
// process may have rotated the pair or signed in again since, and writing back the older
// pair would give the next refresh a spent refresh token. It returns the credential as
// saved.
func ResaveCredential(dir, profile string, cred *Credential) (*Credential, error) {
	saved := cred
	err := lockedWrite(dir, func(tx *Tx) error {
		switch stored, err := tx.LoadFor(profile, cred.OrganizationID); {
		case err == nil:
			stored.UserEmail = cmp.Or(stored.UserEmail, cred.UserEmail)
			stored.UserID = cmp.Or(stored.UserID, cred.UserID)
			saved = stored
		case !errors.Is(err, ErrNotLoggedIn):
			// Unreadable is not absent: what is stored may be newer than cred.
			return err
		}
		return tx.save(profile, saved)
	})
	return saved, err
}

// UpdateCredential persists a refreshed credential without changing which organization
// is active, keeping its tokens where they already are; a keychain that fails gets the
// new pair written to the file, so a rotated token is never lost.
func UpdateCredential(dir, profile string, cred *Credential) error {
	return lockedWrite(dir, func(tx *Tx) error { return tx.Update(profile, cred) })
}

// DeleteCredential forgets every credential a profile holds — logout.
func DeleteCredential(dir, profile string) error {
	var gone []*Credential
	err := mutateCredentialFile(dir, func(file credentialFile) {
		if p := file[profile]; p != nil {
			for _, c := range p.Organizations {
				if c.present() {
					gone = append(gone, c)
				}
			}
		}
		delete(file, profile)
	})
	if err != nil {
		return err
	}
	return forget(dir, profile, gone)
}

// DeleteCredentialFor forgets one organization's credential; if it was active, another becomes active.
func DeleteCredentialFor(dir, profile, organizationID string) error {
	var gone []*Credential
	err := mutateCredentialFile(dir, func(file credentialFile) {
		p := file[profile]
		if p == nil {
			return
		}
		key := organizationID
		if c := p.Organizations[key]; c.present() {
			gone = append(gone, c)
		}
		delete(p.Organizations, key)
		if p.Active != key {
			return
		}
		p.Active = ""
		remaining := make([]string, 0, len(p.Organizations))
		for k := range p.Organizations {
			remaining = append(remaining, k)
		}
		slices.Sort(remaining)
		if len(remaining) > 0 {
			p.Active = remaining[0]
		}
	})
	if err != nil {
		return err
	}
	return forget(dir, profile, gone)
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
	// 0600: this file holds live access and refresh tokens.
	return config.WriteFileAtomic(path, append(data, '\n'), 0o600)
}
