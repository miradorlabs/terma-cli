package auth

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

func itemName(profile, organizationID string) string {
	return "credential/" + profile + "/" + organizationID
}

type tokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// hydrate reads from the keychain the tokens of a credential the file holds without them.
// A keychain that cannot be reached is an error, never ErrNotLoggedIn.
func hydrate(dir, profile string, c *Credential) error {
	if c.AccessToken != "" {
		return nil
	}
	v, err := secret.Get(dir, itemName(profile, c.OrganizationID))
	if errors.Is(err, secret.ErrNotFound) {
		return ErrNotLoggedIn
	}
	if err != nil {
		return err
	}
	var pair tokenPair
	if json.Unmarshal([]byte(v), &pair) != nil || pair.AccessToken == "" {
		return ErrNotLoggedIn
	}
	c.AccessToken, c.RefreshToken = pair.AccessToken, pair.RefreshToken
	return nil
}

// keep stores cred's tokens in the keychain, unless inFile or the keychain fails, and
// returns the copy credentials.json holds. Tokens that land in the file while an item may
// hold earlier ones (old kept them there, or a write timed out and may yet land) mark
// the item stale; sweepStale removes it once the file is on disk, never before.
func keep(dir, profile string, cred, old *Credential, inFile bool) *Credential {
	c := *cred
	c.StaleKeychainItem = old.present() && (old.AccessToken == "" || old.StaleKeychainItem)
	if !inFile {
		pair, _ := json.Marshal(tokenPair{AccessToken: cred.AccessToken, RefreshToken: cred.RefreshToken})
		err := secret.Set(dir, itemName(profile, cred.OrganizationID), string(pair))
		if err == nil {
			c.AccessToken, c.RefreshToken, c.StaleKeychainItem = "", "", false
			return &c
		}
		c.StaleKeychainItem = c.StaleKeychainItem || secret.MayLand(err)
	}
	return &c
}

// sweepStale removes the keychain items marked stale, now that credentials.json holds
// their tokens on disk, and clears the mark of each one it removes.
func sweepStale(dir string) {
	file, err := loadCredentialFile(dir)
	if err != nil || !file.anyStale() {
		return
	}
	_ = mutateCredentialFile(dir, func(file credentialFile) {
		for profile, p := range file {
			for _, c := range p.organizations() {
				if c.StaleKeychainItem && c.AccessToken != "" && secret.Delete(dir, itemName(profile, c.OrganizationID)) == nil {
					c.StaleKeychainItem = false
				}
			}
		}
	})
}

func (f credentialFile) anyStale() bool {
	for _, p := range f {
		for _, c := range p.organizations() {
			if c.StaleKeychainItem {
				return true
			}
		}
	}
	return false
}

func (p *profileCredentials) organizations() map[string]*Credential {
	if p == nil {
		return nil
	}
	return p.Organizations
}

// forget removes from the keychain the tokens of the credentials given, which the file
// no longer indexes.
func forget(dir, profile string, creds []*Credential) error {
	var errs []error
	for _, c := range creds {
		if c.AccessToken != "" && !c.StaleKeychainItem {
			continue
		}
		if err := secret.Delete(dir, itemName(profile, c.OrganizationID)); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("could not remove %d credentials from the system keychain: %w", len(errs), errors.Join(errs...))
	}
	return nil
}

// afterSave finishes a write of organizationID's credential: once the file is on disk it
// sweeps stale items; when the save failed, it takes back tokens the keychain gained for
// a credential the file never indexed, which nothing could otherwise find.
func afterSave(dir, profile, organizationID string, unindexed bool, err error) {
	switch {
	case err == nil:
		sweepStale(dir)
	case unindexed:
		_ = secret.Delete(dir, itemName(profile, organizationID))
	}
}

// Relocate moves every stored credential's tokens, in every profile, to where config.json
// now keeps secrets: into the keychain, or into credentials.json under InsecureStorage.
// A credential the keychain will not take or give up stays where it is.
func Relocate(dir string) error {
	if file, err := loadCredentialFile(dir); err != nil || len(file) == 0 {
		return err
	}
	inFile := config.InsecureStorage(dir)
	var moved []string
	err := mutateCredentialFile(dir, func(file credentialFile) {
		for profile, p := range file {
			for org, c := range p.organizations() {
				switch {
				case !c.present():
				case !inFile && c.AccessToken != "":
					kept := keep(dir, profile, c, c, false)
					p.Organizations[org] = kept
					if kept.AccessToken == "" {
						moved = append(moved, itemName(profile, org))
					}
				case inFile && c.AccessToken == "":
					indexed := *c
					if hydrate(dir, profile, c) == nil {
						p.Organizations[org] = keep(dir, profile, c, &indexed, true)
					}
				}
			}
		}
	})
	if err != nil {
		// The file still holds these tokens: the copies the keychain took would be lost track of.
		for _, item := range moved {
			_ = secret.Delete(dir, item)
		}
		return err
	}
	sweepStale(dir)
	return nil
}
