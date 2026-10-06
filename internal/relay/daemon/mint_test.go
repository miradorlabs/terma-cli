package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
)

const mintedKey = "ter_srv_minted0123456789abcdefghijklmnopqrstuv"

// A missing key is minted once and stored; after a failure none is asked for until the backoff passes.
func TestKeyMinter(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	cfg := &config.Config{Dir: configDir, Policy: config.DefaultPolicy()}
	var calls atomic.Int32
	fail := atomic.Bool{}
	create := func(context.Context, *config.Config, string) (string, error) {
		calls.Add(1)
		if fail.Load() {
			return "", errors.New("refused")
		}
		return mintedKey, nil
	}
	// settled is whether m has no mint of projectID in flight: once it holds, any mint a
	// call started has finished and been counted.
	settled := func(m *KeyMinter, projectID string) func() bool {
		return func() bool {
			m.mu.Lock()
			defer m.mu.Unlock()
			return !m.inFlight[projectID]
		}
	}
	m := NewKeyMinter(t.Context(), cfg, create)
	m.Mint("p1")
	m.Mint("p1") // in flight or done: one mint
	waitUntil(t, func() bool { k, _ := keystore.Get(configDir, "p1"); return k == mintedKey })
	m.Mint("p1") // finds the stored key: no second mint
	waitUntil(t, settled(m, "p1"))
	if n := calls.Load(); n != 1 {
		t.Fatalf("minted %d times for one project", n)
	}

	fail.Store(true)
	m.Mint("p2")
	waitUntil(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.failedAt["p2"].IsZero()
	})
	m.Mint("p2") // inside its backoff
	waitUntil(t, settled(m, "p2"))
	if n := calls.Load(); n != 2 {
		t.Fatalf("a failed project was asked again inside its backoff: %d calls", n)
	}

	server := NewKeyMinter(t.Context(), &config.Config{Dir: configDir, APIKey: "ter_srv_x"}, create)
	server.Mint("p3")
	waitUntil(t, settled(server, "p3"))
	if n := calls.Load(); n != 2 {
		t.Fatal("a server key tried to mint another")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}
