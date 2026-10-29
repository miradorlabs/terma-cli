package auth

import (
	"context"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

// lockCredentialFile locks a sidecar, since the atomic rename over the credential file
// would drop a lock held on its inode.
func lockCredentialFile(ctx context.Context, path string) (func(), error) {
	unlock, err := flock.Lock(ctx, path+".lock")
	if err != nil {
		return nil, fmt.Errorf("lock credentials: %w", err)
	}
	return unlock, nil
}
