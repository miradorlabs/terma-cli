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
//   - a refresh holds it through its exchanges, retried after a 503 for up to 50 s after the
//     first, plus one more (30 s by default), and at most three keychain writes: the pair
//     it holds, the mark of a refresh in doubt, and the pair it gets;
//   - every other holder keeps it through one keychain write and a few deletes;
//   - Relocate takes it once per profile, never across all of them.
var lockWait = 3*secret.WriteTimeout + 2*time.Minute

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

// Load is LoadCredential inside the transaction, with one difference. A sign-in the file
// indexes but whose tokens cannot be read is an error, not ErrNotLoggedIn. The item may be
// missing only for a moment, and a writer that took it for signed out could leave a spent
// refresh token in the store.
func (tx *Tx) Load(profile string) (*Credential, error) {
	cred, err := LoadCredential(tx.dir, profile)
	if errors.Is(err, errTokensGone) {
		return nil, errors.New("user-credentials.json lists this sign-in, but its tokens could not be read from the system keychain")
	}
	return cred, err
}

// Update is UpdateCredential inside the transaction.
func (tx *Tx) Update(profile string, cred *Credential) error {
	inFile := config.InsecureStorage(tx.dir)
	var unindexed bool
	err := tx.mutate(func(file credentialFile) {
		old := file.get(profile)
		if old != nil && old.AccessToken != "" {
			inFile = true
		}
		kept := keep(tx.dir, profile, cred, old, inFile)
		unindexed = old == nil && kept.AccessToken == ""
		file[profile] = kept
	})
	tx.after = append(tx.after, func() { afterSave(tx.dir, profile, unindexed, err) })
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
