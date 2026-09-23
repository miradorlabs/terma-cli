package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/auth"
)

// TestClient_RefusesToFollowRedirects guards the redirect policy. Following a redirect
// would let a 307/308 replay the request body — an auth code, PKCE verifier, or refresh
// token — to the new location, and a same-host https→http downgrade would put the
// bearer token on the wire in cleartext. The API never redirects, so a redirect must be
// a hard error, not a silently followed hop.
func TestClient_RefusesToFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL, liveCredential(), "project-123")

	err := client.Get(context.Background(), "/v1/traces", nil, &struct{}{})
	if err == nil {
		t.Fatal("expected the redirect to be refused, got nil")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("error = %v, want it to name the refused redirect", err)
	}
}

// TestClient_ConcurrentExpiredRequestsRefreshOnce is the regression guard for the
// refresh race: many goroutines sharing one Client, all seeing an expired token, must
// redeem the single-use refresh token exactly once. A second redemption of the same
// rotated token is what the server's reuse detection reads as theft, revoking the whole
// session — so more than one token exchange here would be a real, user-facing bug.
func TestClient_ConcurrentExpiredRequestsRefreshOnce(t *testing.T) {
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			refreshes.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "mir_cli_rotated",
				"refresh_token": "mir_clr_rotated",
				"token_type":    "Bearer",
				"expires_in":    3600,
				"organization":  map[string]string{"id": "org-1"},
			})
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	expired := &auth.Credential{
		AccessToken:  "mir_cli_stale",
		RefreshToken: "mir_clr_stale",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}
	client := newTestClient(t, srv.URL, expired, "project-123")

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			errs <- client.Get(context.Background(), "/v1/identity", nil, &struct{}{})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Get failed: %v", err)
		}
	}

	if got := refreshes.Load(); got != 1 {
		t.Errorf("token exchanges = %d, want exactly 1 across %d concurrent requests", got, n)
	}
}

// TestClient_Concurrent401sRefreshOnce covers the other refresh trigger: a token the
// client believed was live is rejected by every in-flight request at once. The
// generation guard must still collapse those into a single refresh-and-retry.
func TestClient_Concurrent401sRefreshOnce(t *testing.T) {
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			refreshes.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "mir_cli_rotated",
				"refresh_token": "mir_clr_rotated",
				"token_type":    "Bearer",
				"expires_in":    3600,
				"organization":  map[string]string{"id": "org-1"},
			})
			return
		}
		// The originally-live token is now rejected; only the rotated one is accepted.
		if r.Header.Get("Authorization") != "Bearer mir_cli_rotated" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"UNAUTHENTICATED","message":"stale"}}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL, liveCredential(), "project-123")

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			errs <- client.Get(context.Background(), "/v1/identity", nil, &struct{}{})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Get failed: %v", err)
		}
	}

	if got := refreshes.Load(); got != 1 {
		t.Errorf("token exchanges = %d, want exactly 1 despite %d concurrent 401s", got, n)
	}
}
