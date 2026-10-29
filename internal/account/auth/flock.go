package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

// lockCredentialFile locks a sidecar, since the atomic rename over the credential file
// would drop a lock held on its inode.
func lockCredentialFile(ctx context.Context, path string) (func(), error) {
	unlock, err := flock.Lock(ctx, path+".lock")
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("lock credentials: another terma process held them past the deadline: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("lock credentials: %w", err)
	}
	return unlock, nil
}
