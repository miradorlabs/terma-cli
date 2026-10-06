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

	"github.com/miradorlabs/terma-cli/internal/account/auth"
)

// TestClient_RefusesToFollowRedirects makes a redirect a hard error, since following it
// would replay tokens to the new location.
func TestClient_RefusesToFollowRedirects(t *testing.T) {
	t.Parallel()
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

// TestClient_ConcurrentExpiredRequestsRefreshOnce proves goroutines sharing an expired token
// redeem the single-use refresh token exactly once.
func TestClient_ConcurrentExpiredRequestsRefreshOnce(t *testing.T) {
	t.Parallel()
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

// TestClient_Concurrent401sRefreshOnce collapses simultaneous 401s into one refresh.
func TestClient_Concurrent401sRefreshOnce(t *testing.T) {
	t.Parallel()
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
