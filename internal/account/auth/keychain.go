package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

func itemName(profile, organizationID string) string {
	return "credential/" + profile + "/" + organizationID
}

type tokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// errTokensGone means the file indexes a credential whose keychain item is missing or
// unreadable. Readers treat it as signed out. A writer must not: see Tx.LoadFor.
var errTokensGone = fmt.Errorf("%w: the system keychain has no tokens for this sign-in", ErrNotLoggedIn)

// hydrate reads from the keychain the tokens of a credential the file holds without them.
// A keychain that cannot be reached is an error, never ErrNotLoggedIn.
func hydrate(dir, profile string, c *Credential) error {
	if c.AccessToken != "" {
		return nil
	}
	v, err := secret.Get(dir, itemName(profile, c.OrganizationID))
	if errors.Is(err, secret.ErrNotFound) {
		return errTokensGone
	}
	if err != nil {
		return err
	}
	var pair tokenPair
	if json.Unmarshal([]byte(v), &pair) != nil || pair.AccessToken == "" {
		return errTokensGone
	}
	c.AccessToken, c.RefreshToken = pair.AccessToken, pair.RefreshToken
	return nil
}

// keep stores cred's tokens in the keychain and returns the copy credentials.json holds.
// With inFile, or when the keychain fails, the tokens stay in that copy instead. If the
// keychain item may still hold older tokens, the copy marks it stale. That happens when
// old kept its tokens there, or when a write timed out and may still land. sweepStale
// removes a stale item once the file is on disk, never before.
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
// now keeps secrets: the keychain, or credentials.json under InsecureStorage. A credential
// the keychain will not take or give up stays where it is. Each credential moves under its
// own lock. A keychain write may wait a minute on a prompt, so one lock held through all
// of them could keep other writers waiting past lockWait.
func Relocate(dir string) error {
	file, err := loadCredentialFile(dir)
	if err != nil || len(file) == 0 {
		return err
	}
	inFile := config.InsecureStorage(dir)
	for profile, p := range file {
		for org := range p.organizations() {
			if err := relocate(dir, profile, org, inFile); err != nil {
				return err
			}
			// Let a waiter in before the next credential: see flock.PollMax.
			time.Sleep(relocateYield)
		}
	}
	sweepStale(dir)
	return nil
}

// relocateYield is how long Relocate leaves the lock free between credentials. Ten polls
// leave room for a waiter whose timer fires late on a busy machine.
const relocateYield = 10 * flock.PollMax

// relocate is Relocate for one credential, as the file holds it once locked.
func relocate(dir, profile, org string, inFile bool) error {
	moved := false
	err := mutateCredentialFile(dir, func(file credentialFile) {
		p := file[profile]
		c := p.organizations()[org]
		switch {
		case !c.present():
		case !inFile && c.AccessToken != "":
			kept := keep(dir, profile, c, c, false)
			p.Organizations[org] = kept
			moved = kept.AccessToken == ""
		case inFile && c.AccessToken == "":
			indexed := *c
			if hydrate(dir, profile, c) == nil {
				p.Organizations[org] = keep(dir, profile, c, &indexed, true)
			}
		}
	})
	if err != nil && moved {
		// The file still holds these tokens: the copy the keychain took would be lost track of.
		_ = secret.Delete(dir, itemName(profile, org))
	}
	return err
}
