package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// A repository the platform connected has no key on this machine until the relay mints one
// with the signed-in credential, off the export path, while the session's parts wait.

// KeyMintBackoff is how long a project whose key could not be minted waits before a retry.
const KeyMintBackoff = 10 * time.Minute

// KeyMinter mints the keys of claimed projects this machine holds none for.
type KeyMinter struct {
	ctx context.Context
	cfg *config.Config

	mu       sync.Mutex
	inFlight map[string]bool
	failedAt map[string]time.Time
	create   func(ctx context.Context, cfg *config.Config, projectID string) (string, error)
}

// NewKeyMinter mints with create, under cfg's sign-in, until ctx ends.
func NewKeyMinter(ctx context.Context, cfg *config.Config, create func(ctx context.Context, cfg *config.Config, projectID string) (string, error)) *KeyMinter {
	return &KeyMinter{ctx: ctx, cfg: cfg, create: create, inFlight: map[string]bool{}, failedAt: map[string]time.Time{}}
}

// Mint starts minting projectID's key unless one is on the way, or failed lately.
func (m *KeyMinter) Mint(projectID string) {
	if projectID == "" || m.cfg.APIKey != "" {
		return // a server key cannot mint another
	}
	m.mu.Lock()
	if m.inFlight[projectID] || time.Since(m.failedAt[projectID]) < KeyMintBackoff {
		m.mu.Unlock()
		return
	}
	m.inFlight[projectID] = true
	m.mu.Unlock()
	go func() {
		ok := m.mintNow(projectID)
		m.mu.Lock()
		delete(m.inFlight, projectID)
		if ok {
			delete(m.failedAt, projectID)
		} else {
			m.failedAt[projectID] = time.Now()
		}
		m.mu.Unlock()
	}()
}

func (m *KeyMinter) mintNow(projectID string) bool {
	if keystore.Get(projectID) != "" {
		return true // another terma stored one meanwhile
	}
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	key, err := m.create(ctx, m.cfg, projectID)
	if err != nil || key == "" {
		return false
	}
	return keystore.Set(projectID, key, keystore.HostsOf(m.cfg)) == nil
}
