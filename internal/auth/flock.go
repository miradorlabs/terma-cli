package auth

import (
	"context"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

// lockCredentialFile takes an exclusive advisory lock keyed to the credential file, so
// two terma processes serialize their read-modify-write of it rather than racing.
// It blocks until the lock is available and returns a function that releases it.
//
// The lock lives on a sidecar `.lock` file rather than the credential file itself: the
// atomic write renames a fresh temp file over the credential path, which would drop a
// lock held on the original inode. On platforms without flock (notably Windows) it
// excludes nobody; the in-process refresh guard still applies.
func lockCredentialFile(path string) (func(), error) {
	unlock, err := flock.Lock(context.Background(), path+".lock")
	if err != nil {
		return nil, fmt.Errorf("lock credentials: %w", err)
	}
	return unlock, nil
}
