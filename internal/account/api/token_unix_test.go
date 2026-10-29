//go:build unix

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// A rotation the store failed to take leaves it holding the refresh token the server
// spent: the next refresh must redeem the client's own live one, not take the stored
// pair back, which the server's reuse detection would answer by revoking the session.
func TestClient_KeepsARotationTheStoreMissed(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	spent := map[string]bool{}
	revoked := false
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req tokenRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		defer mu.Unlock()
		if spent[req.RefreshToken] {
			revoked = true
		}
		if revoked {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		spent[req.RefreshToken] = true
		n++
		// Each pair is spent at once, so the next call refreshes again.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("mir_cli_%d", n), "refresh_token": fmt.Sprintf("mir_clr_%d", n), "expires_in": 30,
			"session_id": "session-1", "organization": map[string]string{"id": "org-test"},
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	// Tokens in credentials.json, where a failed write leaves the spent pair.
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SaveCredential(dir, config.DefaultProfile, &auth.Credential{AccessToken: "mir_cli_0", RefreshToken: "mir_clr_0",
		SessionID: "session-1", OrganizationID: "org-test", AuthURL: srv.URL, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	client, err := New(&config.Config{Dir: dir, ProfileName: config.DefaultProfile, APIURL: srv.URL, AuthURL: srv.URL}, Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := client.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh stored its pair in a read-only directory")
	}
	if got := client.Credential().RefreshToken; got != "mir_clr_1" {
		t.Fatalf("client refresh token = %q, want the rotated mir_clr_1", got)
	}

	for _, mode := range []os.FileMode{0o500, 0o700} {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		_ = client.Refresh(context.Background())
		mu.Lock()
		gone := revoked
		mu.Unlock()
		if gone {
			t.Fatalf("directory %o: the refresh redeemed a spent refresh token and the session was revoked", mode)
		}
	}
	stored, err := auth.LoadCredential(dir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	if want := client.Credential().RefreshToken; stored.RefreshToken != want || want != "mir_clr_3" {
		t.Errorf("stored %q, client %q; want both the live mir_clr_3", stored.RefreshToken, want)
	}
}
