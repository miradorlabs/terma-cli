package auth

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// itemName is the keychain item that holds a profile's sign-in tokens.
func itemName(profile string) string {
	return "user-credential/" + profile
}

type tokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// SessionID names the session the tokens are: the file that indexes them is written
	// after the item, so a read between the two writes must not pair one session's index
	// with another's tokens. "" (an item from before it) is taken for the indexed one.
	SessionID string `json:"session_id,omitempty"`
}

// errTokensGone means the file indexes a credential whose keychain item is missing or
// unreadable. Readers treat it as signed out. A writer must not: see Tx.Load.
var errTokensGone = fmt.Errorf("%w: the system keychain has no tokens for this sign-in", ErrNotLoggedIn)

// hydrate reads from the keychain the tokens of a credential the file holds without them.
// A keychain that cannot be reached is an error, never ErrNotLoggedIn.
func hydrate(dir, profile string, c *Credential) error {
	if c.AccessToken != "" {
		c.dropForeignMark()
		return nil
	}
	v, err := secret.Get(dir, itemName(profile))
	if errors.Is(err, secret.ErrNotFound) {
		return errTokensGone
	}
	if err != nil {
		return err
	}
	var pair tokenPair
	if json.Unmarshal([]byte(v), &pair) != nil || pair.AccessToken == "" ||
		pair.SessionID != "" && pair.SessionID != c.SessionID {
		return errTokensGone
	}
	c.AccessToken, c.RefreshToken = pair.AccessToken, pair.RefreshToken
	c.dropForeignMark()
	return nil
}

// keep stores cred's tokens in the keychain and returns the copy user-credentials.json
// holds. With inFile, or when the keychain fails, the tokens stay in that copy instead. If
// the keychain item may still hold older tokens, the copy marks it stale. That happens
// when old kept its tokens there, or when a write timed out and may still land. sweepStale
// removes a stale item once the file is on disk, never before.
func keep(dir, profile string, cred, old *Credential, inFile bool) *Credential {
	c := *cred
	c.StaleKeychainItem = old.present() && (old.AccessToken == "" || old.StaleKeychainItem)
	if !inFile {
		pair, _ := json.Marshal(tokenPair{AccessToken: cred.AccessToken, RefreshToken: cred.RefreshToken, SessionID: cred.SessionID})
		err := secret.Set(dir, itemName(profile), string(pair))
		if err == nil {
			c.AccessToken, c.RefreshToken, c.StaleKeychainItem = "", "", false
			return &c
		}
		c.StaleKeychainItem = c.StaleKeychainItem || secret.MayLand(err)
	}
	return &c
}

// sweepStale removes the keychain items marked stale, now that user-credentials.json holds
// their tokens on disk, and clears the mark of each one it removes.
func sweepStale(dir string) {
	file, err := loadCredentialFile(dir)
	if err != nil || !file.anyStale() {
		return
	}
	_ = mutateCredentialFile(dir, func(file credentialFile) {
		for profile, c := range file {
			if c.present() && c.StaleKeychainItem && c.AccessToken != "" && secret.Delete(dir, itemName(profile)) == nil {
				c.StaleKeychainItem = false
			}
		}
	})
}

func (f credentialFile) anyStale() bool {
	for _, c := range f {
		if c.present() && c.StaleKeychainItem {
			return true
		}
	}
	return false
}

// forget removes from the keychain the tokens of a profile's sign-in, which the file no
// longer indexes.
func forget(dir, profile string, c *Credential) error {
	if c == nil || c.AccessToken != "" && !c.StaleKeychainItem {
		return nil
	}
	if err := secret.Delete(dir, itemName(profile)); err != nil {
		return fmt.Errorf("could not remove the sign-in from the system keychain: %w", err)
	}
	return nil
}

// afterSave finishes a write of a profile's credential: once the file is on disk it
// sweeps stale items; when the save failed, it takes back tokens the keychain gained for
// a credential the file never indexed, which nothing could otherwise find.
func afterSave(dir, profile string, unindexed bool, err error) {
	switch {
	case err == nil:
		sweepStale(dir)
	case unindexed:
		_ = secret.Delete(dir, itemName(profile))
	}
}

// Relocate moves every profile's sign-in tokens to where config.json now keeps secrets:
// the keychain, or user-credentials.json under InsecureStorage. A sign-in the keychain
// will not take or give up stays where it is.
func Relocate(dir string) error {
	file, err := loadCredentialFile(dir)
	if err != nil || len(file) == 0 {
		return err
	}
	inFile := config.InsecureStorage(dir)
	for profile := range file {
		if err := relocate(dir, profile, inFile); err != nil {
			return err
		}
	}
	sweepStale(dir)
	return nil
}

// relocate is Relocate for one profile, as the file holds it once locked.
func relocate(dir, profile string, inFile bool) error {
	moved := false
	err := mutateCredentialFile(dir, func(file credentialFile) {
		c := file.get(profile)
		switch {
		case c == nil:
		case !inFile && c.AccessToken != "":
			kept := keep(dir, profile, c, c, false)
			file[profile] = kept
			moved = kept.AccessToken == ""
		case inFile && c.AccessToken == "":
			indexed := *c
			if hydrate(dir, profile, c) == nil {
				file[profile] = keep(dir, profile, c, &indexed, true)
			}
		}
	})
	if err != nil && moved {
		// The file still holds these tokens: the copy the keychain took would be lost track of.
		_ = secret.Delete(dir, itemName(profile))
	}
	return err
}
