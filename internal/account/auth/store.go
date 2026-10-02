// Package auth implements the CLI side of the Terma PKCE loopback login: the
// browser handoff, the code exchange, credential storage, and transparent refresh.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// ErrNotLoggedIn is returned when there is no usable credential.
var ErrNotLoggedIn = errors.New("not logged in")

// Credential is one profile's stored tokens for one organization.
type Credential struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	SessionID    string    `json:"session_id,omitempty"`
	// AuthURL is the host that minted this credential, the only one it is valid to.
	AuthURL        string `json:"auth_url,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
	UserEmail      string `json:"user_email,omitempty"`
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
	if cred == nil || cred.AccessToken == "" || cred.OrganizationID == "" {
		return nil
	}
	return cred
}

// put stores cred under its organization and reports the credential it replaced.
func (p *profileCredentials) put(cred *Credential) (replaced *Credential) {
	if p.Organizations == nil {
		p.Organizations = map[string]*Credential{}
	}
	key := cred.OrganizationID
	replaced = p.Organizations[key]
	p.Organizations[key] = cred
	return replaced
}

type credentialFile map[string]*profileCredentials

// LoadCredential returns the active credential for a profile, or ErrNotLoggedIn.
func LoadCredential(profile string) (*Credential, error) {
	file, err := loadCredentialFile()
	if err != nil {
		return nil, err
	}
	cred := file[profile].active()
	if cred == nil {
		return nil, ErrNotLoggedIn
	}
	return cred, nil
}

// LoadCredentialFor returns the profile's credential for one organization, active or
// not, or ErrNotLoggedIn when the profile never signed into it.
func LoadCredentialFor(profile, organizationID string) (*Credential, error) {
	file, err := loadCredentialFile()
	if err != nil {
		return nil, err
	}
	p := file[profile]
	if p == nil {
		return nil, ErrNotLoggedIn
	}
	cred := p.Organizations[organizationID]
	if cred == nil || cred.AccessToken == "" || cred.OrganizationID == "" {
		return nil, ErrNotLoggedIn
	}
	return cred, nil
}

// Credentials lists every credential a profile holds, the active one first.
func Credentials(profile string) ([]*Credential, error) {
	file, err := loadCredentialFile()
	if err != nil {
		return nil, err
	}
	p := file[profile]
	if p == nil {
		return nil, nil
	}
	keys := make([]string, 0, len(p.Organizations))
	for key, cred := range p.Organizations {
		if cred != nil && cred.AccessToken != "" {
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
		out = append(out, p.Organizations[key])
	}
	return out, nil
}

// SaveCredential stores and activates a new credential, returning the session it
// displaced for the caller to revoke.
func SaveCredential(profile string, cred *Credential) (replaced *Credential, err error) {
	err = mutateCredentialFile(func(file credentialFile) {
		p := file[profile]
		if p == nil {
			p = &profileCredentials{}
			file[profile] = p
		}
		replaced = p.put(cred)
		p.Active = cred.OrganizationID
	})
	if replaced != nil && replaced.SessionID == cred.SessionID {
		replaced = nil
	}
	return replaced, err
}

// UpdateCredential persists a refreshed credential without changing which organization is active.
func UpdateCredential(profile string, cred *Credential) error {
	return mutateCredentialFile(func(file credentialFile) {
		p := file[profile]
		if p == nil {
			p = &profileCredentials{}
			file[profile] = p
		}
		p.put(cred)
		if p.Active == "" {
			p.Active = cred.OrganizationID
		}
	})
}

// UseOrganization makes a stored credential the active one and returns it, or
// ErrNotLoggedIn when the profile holds none for that organization.
func UseOrganization(profile, organizationID string) (*Credential, error) {
	var cred *Credential
	err := mutateCredentialFile(func(file credentialFile) {
		p := file[profile]
		if p == nil {
			return
		}
		key := organizationID
		if c := p.Organizations[key]; c != nil && c.AccessToken != "" {
			cred = c
			p.Active = key
		}
	})
	if err != nil {
		return nil, err
	}
	if cred == nil {
		return nil, ErrNotLoggedIn
	}
	return cred, nil
}

// DeleteCredential forgets every credential a profile holds — logout.
func DeleteCredential(profile string) error {
	return mutateCredentialFile(func(file credentialFile) {
		delete(file, profile)
	})
}

// DeleteCredentialFor forgets one organization's credential; if it was active, another becomes active.
func DeleteCredentialFor(profile, organizationID string) error {
	return mutateCredentialFile(func(file credentialFile) {
		p := file[profile]
		if p == nil {
			return
		}
		key := organizationID
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
}

// mutateCredentialFile runs mutate under a cross-process lock: unlocked, a second
// concurrent writer erases the first's entry.
func mutateCredentialFile(mutate func(credentialFile)) error {
	path, err := config.CredentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	unlock, err := lockCredentialFile(path)
	if err != nil {
		return err
	}
	defer unlock()

	file, err := loadCredentialFile()
	if err != nil {
		return err
	}
	mutate(file)
	return saveCredentialFile(file)
}

func loadCredentialFile() (credentialFile, error) {
	path, err := config.CredentialsPath()
	if err != nil {
		return nil, err
	}
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

func saveCredentialFile(file credentialFile) error {
	path, err := config.CredentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	// 0600: this file holds live access and refresh tokens.
	return config.WriteFileAtomic(path, append(data, '\n'), 0o600)
}
