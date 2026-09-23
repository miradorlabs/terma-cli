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
	"sort"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// ErrNotLoggedIn is what every command surfaces when there is no usable credential.
// Callers turn it into "run terma login" rather than a raw file error.
var ErrNotLoggedIn = errors.New("not logged in")

// Credential is one profile's stored tokens. expires_at is the access token's own
// expiry, tracked so the CLI can refresh proactively instead of discovering the
// expiry as a failed request.
type Credential struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	SessionID    string    `json:"session_id,omitempty"`
	// AuthURL records the host that minted this credential. A token is only valid to
	// the environment that issued it, so sending one somewhere else is never useful —
	// and the 401 it earns looks like a broken login rather than a wrong endpoint.
	// Empty on credentials written before this field existed; those are not checked.
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
		"this profile is logged in against %s but is now pointed at %s — run `terma login` for this environment, or switch profiles with `terma config use <profile>`",
		e.IssuedBy, e.ConfiguredAs)
}

// CheckEnvironment reports whether the credential belongs to authURL.
func (c *Credential) CheckEnvironment(authURL string) error {
	if c.AuthURL == "" || authURL == "" || c.AuthURL == authURL {
		return nil
	}
	return &ErrWrongEnvironment{IssuedBy: c.AuthURL, ConfiguredAs: authURL}
}

// Expired reports whether the access token is spent. The skew means a token that
// will die mid-flight is treated as already dead, so a slow request cannot land
// with an expired credential.
func (c *Credential) Expired() bool {
	const skew = 60 * time.Second
	return c.ExpiresAt.IsZero() || time.Now().Add(skew).After(c.ExpiresAt)
}

// A credential is minted against one organization, so a profile that works in two
// holds two. They are kept side by side, keyed by organization, with one marked
// active: switching organizations is then a local change that reuses the session the
// user already approved, rather than a second browser handoff that leaves the first
// session live and forgotten server-side.
type profileCredentials struct {
	// Active is the organization id whose credential commands use.
	Active        string                 `json:"active,omitempty"`
	Organizations map[string]*Credential `json:"organizations"`
}

// legacyOrganization keys a credential that recorded no organization id — one written
// by a build from before the field existed. It can still be used and refreshed; it
// just cannot be told apart from another such credential, and there is at most one.
const legacyOrganization = "-"

func organizationKey(id string) string {
	if id == "" {
		return legacyOrganization
	}
	return id
}

// UnmarshalJSON accepts both shapes of a profile entry: the current one, and the
// single credential each profile held before organizations were kept side by side.
// The old shape is recognised by its access_token and becomes a one-entry map, so an
// existing credentials.json keeps working and is rewritten in the new shape on the
// next save.
func (p *profileCredentials) UnmarshalJSON(data []byte) error {
	type current profileCredentials
	var cur current
	if err := json.Unmarshal(data, &cur); err != nil {
		return err
	}
	if cur.Organizations != nil {
		*p = profileCredentials(cur)
		return nil
	}
	var legacy Credential
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	p.Organizations = map[string]*Credential{}
	if legacy.AccessToken == "" {
		return nil
	}
	key := organizationKey(legacy.OrganizationID)
	p.Active = key
	p.Organizations[key] = &legacy
	return nil
}

func (p *profileCredentials) active() *Credential {
	if p == nil || p.Organizations == nil {
		return nil
	}
	cred := p.Organizations[p.Active]
	if cred == nil || cred.AccessToken == "" {
		return nil
	}
	return cred
}

// put stores cred under its organization and reports the credential it replaced, if
// any — the caller may want to revoke that session now that nothing will use it.
//
// A credential can move keys: a legacy entry that recorded no organization learns its
// organization on the first refresh, and a rotated pair keeps its session id. Either
// way the old entry is the same session and is dropped rather than left behind as a
// second, stale copy — and if it was active, the active mark follows it.
func (p *profileCredentials) put(cred *Credential) (replaced *Credential) {
	if p.Organizations == nil {
		p.Organizations = map[string]*Credential{}
	}
	key := organizationKey(cred.OrganizationID)
	for other, existing := range p.Organizations {
		if other == key || existing == nil {
			continue
		}
		sameSession := cred.SessionID != "" && existing.SessionID == cred.SessionID
		if other != legacyOrganization && !sameSession {
			continue
		}
		delete(p.Organizations, other)
		if p.Active == other {
			p.Active = key
		}
	}
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
	cred := p.Organizations[organizationKey(organizationID)]
	if cred == nil || cred.AccessToken == "" {
		return nil, ErrNotLoggedIn
	}
	return cred, nil
}

// Credentials lists every credential a profile holds, the active one first and the
// rest in a stable order. Empty, not an error, for a profile that never signed in.
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

// SaveCredential stores a freshly minted credential and makes it the profile's active
// one. It returns the credential it displaced for the same organization, if there was
// one, so the caller can revoke that session rather than leave it live and orphaned.
func SaveCredential(profile string, cred *Credential) (replaced *Credential, err error) {
	err = mutateCredentialFile(func(file credentialFile) {
		p := file[profile]
		if p == nil {
			p = &profileCredentials{}
			file[profile] = p
		}
		replaced = p.put(cred)
		p.Active = organizationKey(cred.OrganizationID)
	})
	if replaced != nil && replaced.SessionID == cred.SessionID {
		replaced = nil
	}
	return replaced, err
}

// UpdateCredential persists a rotated credential in place — the refresh path. It
// never changes which organization is active: a refresh of a parked credential (say,
// while switching to it) must not switch the profile as a side effect.
func UpdateCredential(profile string, cred *Credential) error {
	return mutateCredentialFile(func(file credentialFile) {
		p := file[profile]
		if p == nil {
			p = &profileCredentials{}
			file[profile] = p
		}
		p.put(cred)
		if p.Active == "" {
			p.Active = organizationKey(cred.OrganizationID)
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
		key := organizationKey(organizationID)
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

// DeleteCredentialFor forgets one organization's credential. If it was the active one,
// another stored credential becomes active so the profile is not left pointing at
// nothing while still holding a usable session.
func DeleteCredentialFor(profile, organizationID string) error {
	return mutateCredentialFile(func(file credentialFile) {
		p := file[profile]
		if p == nil {
			return
		}
		key := organizationKey(organizationID)
		delete(p.Organizations, key)
		if p.Active != key {
			return
		}
		p.Active = ""
		remaining := make([]string, 0, len(p.Organizations))
		for k := range p.Organizations {
			remaining = append(remaining, k)
		}
		sort.Strings(remaining)
		if len(remaining) > 0 {
			p.Active = remaining[0]
		}
	})
}

// mutateCredentialFile runs mutate against the on-disk credential map under an
// exclusive cross-process lock, then persists the result. Locking the whole
// read-modify-write is what stops two concurrent `terma` processes from clobbering
// each other: without it, each reads the map, changes its own profile, and writes the
// whole thing back — and the second writer erases the first's unrelated entry.
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
