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
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	cfg := &config.Config{Policy: config.DefaultPolicy()}
	var calls atomic.Int32
	fail := atomic.Bool{}
	create := func(context.Context, *config.Config, string) (string, error) {
		calls.Add(1)
		if fail.Load() {
			return "", errors.New("refused")
		}
		return mintedKey, nil
	}
	m := NewKeyMinter(t.Context(), cfg, create)
	m.Mint("p1")
	m.Mint("p1") // in flight or done: one mint
	waitUntil(t, func() bool { return keystore.Get("p1") == mintedKey })
	m.Mint("p1")
	time.Sleep(50 * time.Millisecond)
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
	m.Mint("p2")
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("a failed project was asked again inside its backoff: %d calls", n)
	}

	NewKeyMinter(t.Context(), &config.Config{APIKey: "ter_srv_x"}, create).Mint("p3")
	time.Sleep(50 * time.Millisecond)
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
