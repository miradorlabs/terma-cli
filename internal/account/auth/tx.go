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
//   - a refresh holds it through one exchange (30 s by default) and at most two keychain
//     writes: the pair it holds and the pair it gets;
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

func (tx *Tx) mutate(mutate func(credentialFile)) error {
	file, err := loadCredentialFile(tx.dir)
	if err != nil {
		return err
	}
	mutate(file)
	return saveCredentialFile(config.CredentialsPath(tx.dir), file)
}
