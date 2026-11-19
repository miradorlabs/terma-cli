package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// Tx is the credential file held under its cross-process lock. What it loads cannot
// change until the transaction ends. What it saves takes no second lock, which the holder
// would wait on forever.
type Tx struct {
	dir string
	// after runs once the lock is released, because sweeping stale keychain items takes
	// the lock again.
	after []func()
}

// lockWait bounds every wait for the credential lock. A holder that hangs then costs the
// waiter an error, not a stall. It is longer than a healthy holder keeps the lock:
//
//   - a refresh holds it through one exchange and at most two keychain writes: the pair it
//     holds and the pair it gets. The exchange, its retries included, ends within 40 s
//     (api's RefreshBudget); its marks (MarkUnsettled) add credentials.json writes;
//   - every other holder keeps it through one keychain write and a few deletes;
//   - Relocate takes it once per credential, never across all of them.
var lockWait = 2*secret.WriteTimeout + time.Minute

// lockedWrite is Locked for a write that has no deadline of its own.
func lockedWrite(dir string, fn func(tx *Tx) error) error {
	return Locked(context.Background(), dir, fn)
}

// Locked runs fn holding the credential lock, waiting for it until ctx is done or lockWait
// has passed, since another process may hold the lock through a network call.
func Locked(ctx context.Context, dir string, fn func(tx *Tx) error) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	unlock, err := lockCredentialFile(ctx, config.CredentialsPath(dir))
	cancel()
	if err != nil {
		return err
	}
	tx := &Tx{dir: dir}
	err = fn(tx)
	unlock()
	for _, f := range tx.after {
		f()
	}
	return err
}

// LoadFor is LoadCredentialFor inside the transaction, with one difference. A credential
// the file indexes but whose tokens cannot be read is an error, not ErrNotLoggedIn. The
// item may be missing only for a moment, and a writer that took it for signed out could
// leave a spent refresh token in the store.
func (tx *Tx) LoadFor(profile, organizationID string) (*Credential, error) {
	cred, err := LoadCredentialFor(tx.dir, profile, organizationID)
	if errors.Is(err, errTokensGone) {
		return nil, errors.New("credentials.json lists this sign-in, but its tokens could not be read from the system keychain")
	}
	return cred, err
}

// Update is UpdateCredential inside the transaction.
func (tx *Tx) Update(profile string, cred *Credential) error {
	inFile := config.InsecureStorage(tx.dir)
	var unindexed bool
	err := tx.mutate(func(file credentialFile) {
		p := file[profile]
		if p == nil {
			p = &profileCredentials{}
			file[profile] = p
		}
		old := p.Organizations[cred.OrganizationID]
		if old.present() && old.AccessToken != "" {
			inFile = true
		}
		kept := keep(tx.dir, profile, cred, old, inFile)
		unindexed = !old.present() && kept.AccessToken == ""
		p.put(kept)
		if p.Active == "" {
			p.Active = cred.OrganizationID
		}
	})
	tx.after = append(tx.after, func() { afterSave(tx.dir, profile, cred.OrganizationID, unindexed, err) })
	return err
}

// DeleteFor is DeleteCredentialFor inside the transaction, for one version of the
// credential: it removes organizationID's credential only while its refresh token is still
// refreshToken. One another process has rotated since, or a new sign-in, stays. Unlike
// DeleteCredentialFor it makes no other organization active: the developer is signed out
// of this one, not moved to another.
//
// It removes the entry in credentials.json and leaves the keychain item. Only the entry
// leads a reader to the item (hydrate), and the item holds only the refused tokens, which
// the next sign-in to the organization overwrites (keep). Deleting it could do harm: a
// keychain delete that times out may still run, after the lock is released, and remove the
// tokens a sign-in has just written under the same name.
func (tx *Tx) DeleteFor(profile, organizationID, refreshToken string) error {
	stored, err := tx.LoadFor(profile, organizationID)
	switch {
	case errors.Is(err, ErrNotLoggedIn):
		return nil
	case err != nil:
		return err
	case stored.RefreshToken != refreshToken:
		return nil
	}
	return tx.mutate(func(file credentialFile) {
		if p := file[profile]; p != nil {
			delete(p.Organizations, organizationID)
		}
	})
}

// MarkUnsettled records on organizationID's credential that a refresh of refreshToken,
// first sent at at, has no answer yet (Credential.RefreshUnsettledAt). An earlier mark of
// refreshToken stays, and a later one gives way: the first send is the one the server's
// grace runs from.
//
// It reads and writes credentials.json alone, never the keychain: the caller loaded
// refreshToken in this transaction (LoadFor), and nothing else changes the store while it
// holds the lock. The mark names its token (RefreshUnsettledFor), so one written beside
// another token, as when saving a rotation failed half way, counts for nothing. The entry
// gone, it writes nothing; it fails only when credentials.json cannot be read or written.
func (tx *Tx) MarkUnsettled(profile, organizationID, refreshToken string, at time.Time) error {
	sum := TokenSHA256(refreshToken)
	return tx.mutateEntry(profile, organizationID, refreshToken, func(c *Credential) bool {
		if c.RefreshUnsettledFor == sum && !c.RefreshUnsettledAt.IsZero() && !at.Before(c.RefreshUnsettledAt) {
			return false
		}
		c.RefreshUnsettledAt, c.RefreshUnsettledFor = at, sum
		return true
	})
}

// ExpireUnsettled marks refreshToken as never to be sent again (UnsettledExpired), as
// MarkUnsettled does.
func (tx *Tx) ExpireUnsettled(profile, organizationID, refreshToken string) error {
	return tx.MarkUnsettled(profile, organizationID, refreshToken, UnsettledExpired)
}

// ClearUnsettled takes back MarkUnsettled's mark at at, of refreshToken, once an answer
// shows the server spent nothing. Any other mark stays. Like MarkUnsettled it touches
// credentials.json alone.
func (tx *Tx) ClearUnsettled(profile, organizationID, refreshToken string, at time.Time) error {
	sum := TokenSHA256(refreshToken)
	return tx.mutateEntry(profile, organizationID, refreshToken, func(c *Credential) bool {
		if c.RefreshUnsettledFor != sum || !c.RefreshUnsettledAt.Equal(at) {
			return false
		}
		c.RefreshUnsettledAt, c.RefreshUnsettledFor = time.Time{}, ""
		return true
	})
}

// mutateEntry applies change to organizationID's entry in credentials.json, and writes the
// file when change reports it changed something. An entry gone, or one that keeps its
// tokens in the file and holds another refresh token than refreshToken, is left as it is.
//
// An entry whose tokens are in the keychain is not compared with refreshToken at all. That
// is safe only because every caller loaded refreshToken (LoadFor) or saved it (Update) in
// this same transaction, so nothing can have replaced it. A caller that did not must not
// use it.
func (tx *Tx) mutateEntry(profile, organizationID, refreshToken string, change func(*Credential) bool) error {
	file, err := loadCredentialFile(tx.dir)
	if err != nil {
		return err
	}
	c := file[profile].organizations()[organizationID]
	if !c.present() || c.RefreshToken != "" && c.RefreshToken != refreshToken || !change(c) {
		return nil
	}
	return saveCredentialFile(config.CredentialsPath(tx.dir), file)
}

func (tx *Tx) mutate(mutate func(credentialFile)) error {
	file, err := loadCredentialFile(tx.dir)
	if err != nil {
		return err
	}
	mutate(file)
	return saveCredentialFile(config.CredentialsPath(tx.dir), file)
}
