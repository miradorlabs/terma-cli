package cmd

import (
	"context"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/keystore"
)

// A repository the platform connected has its binding and hooks committed, and no
// `terma install` ran in it: this machine holds no key for its project. Its sessions are
// claimed — the repository opted in, and the developer signed in with `terma setup` —
// so the relay mints the project's key itself, once, with the signed-in credential, and
// stores it as the project's key (keystore.Set): the relay sends with it, and so does
// the spool, whose hook events were held for want of one. Minting runs off the export
// path; the session's parts wait in the hold meanwhile (no_key), which the next sweep
// releases. A failure — not signed in, refused — is not retried for keyMintBackoff.

// keyMintBackoff is how long a project whose key could not be minted waits before the
// relay tries again.
const keyMintBackoff = 10 * time.Minute

type relayKeyMinter struct {
	ctx context.Context
	cfg *config.Config

	mu       sync.Mutex
	inFlight map[string]bool
	failedAt map[string]time.Time
	// create mints; the API client when nil. A test replaces it.
	create func(ctx context.Context, cfg *config.Config, projectID string) (string, error)
}

func newRelayKeyMinter(ctx context.Context, cfg *config.Config) *relayKeyMinter {
	return &relayKeyMinter{ctx: ctx, cfg: cfg, inFlight: map[string]bool{}, failedAt: map[string]time.Time{}}
}

// mint starts minting projectID's key unless one is on the way, or failed lately.
func (m *relayKeyMinter) mint(projectID string) {
	if projectID == "" || m.cfg.APIKey != "" {
		return // a server key cannot mint another
	}
	m.mu.Lock()
	if m.inFlight[projectID] || time.Since(m.failedAt[projectID]) < keyMintBackoff {
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

func (m *relayKeyMinter) mintNow(projectID string) bool {
	if keystore.Get(projectID) != "" {
		return true // another terma stored one meanwhile
	}
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	create := m.create
	if create == nil {
		create = createProjectKey
	}
	key, err := create(ctx, m.cfg, projectID)
	if err != nil || key == "" {
		return false
	}
	return keystore.Set(projectID, key, keystore.HostsOf(m.cfg)) == nil
}

// createProjectKey mints a server key for projectID with the signed-in credential.
func createProjectKey(ctx context.Context, cfg *config.Config, projectID string) (string, error) {
	scoped := *cfg
	scoped.ProjectID = projectID
	client, err := newClient(&scoped)
	if err != nil {
		return "", err
	}
	key, _, err := client.CreateServerKey(ctx, projectID, "terma-relay@"+harness.Hostname(),
		"Created by the terma relay, for a repository connected in Terma")
	return key, err
}
