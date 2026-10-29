//go:build unix

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// Separate terma processes near expiry, and a setup run again beside them, share one
// redemption of the refresh token: two would make the server revoke the session.
func TestAuthToken_ConcurrentProcessesRedeemOnce(t *testing.T) {
	terma := termaBinary(t)
	srv := newTokenServer(t)
	srv.delay = 300 * time.Millisecond
	dir := signInForToken(t, srv, time.Now().Add(30*time.Second), grantTeam)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "TERMA_CONFIG_DIR="+dir, "TERMA_STATE_DIR="+t.TempDir())
	// A binary's first start can be slow enough (macOS checks it) that the processes would
	// not overlap.
	if out, err := exec.Command(terma, "version").CombinedOutput(); err != nil {
		t.Fatalf("terma version: %v\n%s", err, out)
	}

	const processes = 8
	type result struct {
		stdout, stderr bytes.Buffer
		err            error
	}
	results := make([]result, processes)
	var wg sync.WaitGroup
	for i := range results {
		cmd := exec.Command(terma, "auth", "token", "--json")
		cmd.Env, cmd.Stdout, cmd.Stderr = env, &results[i].stdout, &results[i].stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		wg.Go(func() { results[i].err = cmd.Wait() })
	}
	var setupErr error
	wg.Go(func() {
		// Setup reads the pair while the first redemption is still in flight.
		select {
		case <-srv.redeeming:
		case <-time.After(10 * time.Second):
		}
		if _, ok, err := testApp.reuseStoredSession(t.Context(), cfg, orgRef{}); err != nil || !ok {
			setupErr = err
			if err == nil {
				setupErr = auth.ErrNotLoggedIn
			}
		}
	})
	wg.Wait()

	access, refresh, redemptions, fails, revoked := srv.state()
	if redemptions != 1 || fails != 0 || revoked {
		t.Fatalf("redemptions = %d, refused = %d, revoked = %v; want one redemption and a live session", redemptions, fails, revoked)
	}
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("process %d: %v\nstdout: %s\nstderr: %s", i, r.err, r.stdout.String(), r.stderr.String())
		}
		assertTokenGrant(t, r.stdout.String(), access)
	}
	if setupErr != nil {
		t.Fatalf("setup beside them: %v", setupErr)
	}
	stored, err := auth.LoadCredential(dir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != refresh {
		t.Fatalf("stored refresh token %q, want the live one %q", stored.RefreshToken, refresh)
	}
}
