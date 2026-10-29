package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// newTestClient builds a client against a stub server, its credential store in a temp dir.
func newTestClient(t *testing.T, url string, cred *auth.Credential, projectID string) *Client {
	t.Helper()
	return newSplitTestClient(t, url, url, cred, projectID)
}

func newSplitTestClient(t *testing.T, apiURL, authURL string, cred *auth.Credential, projectID string) *Client {
	t.Helper()
	dir := t.TempDir()
	cred.AuthURL = authURL
	if cred.OrganizationID == "" {
		cred.OrganizationID = "org-test"
	}

	if _, err := auth.SaveCredential(dir, config.DefaultProfile, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	client, err := New(&config.Config{
		Dir:         dir,
		ProfileName: config.DefaultProfile,
		APIURL:      apiURL,
		AuthURL:     authURL,
		ProjectID:   projectID,
	}, Options{Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func liveCredential() *auth.Credential {
	return &auth.Credential{
		AccessToken:    "mir_cli_live",
		OrganizationID: "org-test",
		RefreshToken:   "mir_clr_live",
		ExpiresAt:      time.Now().Add(time.Hour),
	}
}

func TestClient_SendsBearerAndProjectHeader(t *testing.T) {
	t.Parallel()
	var gotAuth, gotProject string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotProject = r.Header.Get(projectHeader)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL, liveCredential(), "project-123")
	if err := client.Get(context.Background(), "/v1/identity", nil, &struct{}{}); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if gotAuth != "Bearer mir_cli_live" {
		t.Errorf("Authorization = %q, want the access token", gotAuth)
	}
	if gotProject != "project-123" {
		t.Errorf("%s = %q, want project-123", projectHeader, gotProject)
	}
}

func TestClient_ServerKeyDoesNotSendProjectHeader(t *testing.T) {
	t.Parallel()
	var gotAuth, gotProject string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotProject = r.Header.Get(projectHeader)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client, err := New(&config.Config{
		ProfileName: config.DefaultProfile,
		APIURL:      srv.URL,
		AuthURL:     srv.URL,
		APIKey:      "mir_srv_abc",
		ProjectID:   "project-123",
	}, Options{Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := client.Get(context.Background(), "/v1/identity", nil, &struct{}{}); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if gotAuth != "Bearer mir_srv_abc" {
		t.Errorf("Authorization = %q, want the server key", gotAuth)
	}
	if gotProject != "" {
		t.Errorf("%s = %q, want it omitted under a server key", projectHeader, gotProject)
	}
}

func TestClient_RefreshesExpiredTokenBeforeRequesting(t *testing.T) {
	t.Parallel()
	var refreshes, reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			refreshes.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "mir_cli_rotated",
				"refresh_token": "mir_clr_rotated",
				"token_type":    "Bearer",
				"expires_in":    3600,
				"organization":  map[string]string{"id": "org-1", "name": "Acme"},
				"user":          map[string]string{"id": "user-1"},
			})
			return
		}
		reads.Add(1)
		if r.Header.Get("Authorization") != "Bearer mir_cli_rotated" {
			t.Errorf("read used %q, want the rotated token", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	expired := &auth.Credential{
		AccessToken:    "mir_cli_stale",
		OrganizationID: "org-1",
		RefreshToken:   "mir_clr_stale",
		ExpiresAt:      time.Now().Add(-time.Hour),
	}
	client := newTestClient(t, srv.URL, expired, "project-123")

	if err := client.Get(context.Background(), "/v1/identity", nil, &struct{}{}); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got := refreshes.Load(); got != 1 {
		t.Errorf("refreshes = %d, want 1", got)
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("reads = %d, want 1 (no wasted 401 round trip)", got)
	}

	saved, err := auth.LoadCredential(client.dir, config.DefaultProfile)
	if err != nil {
		t.Fatalf("LoadCredential: %v", err)
	}
	if saved.RefreshToken != "mir_clr_rotated" {
		t.Errorf("stored refresh token = %q, want the rotated one", saved.RefreshToken)
	}
	// A credential stored without the user's id gains it from the refresh.
	if saved.UserID != "user-1" {
		t.Errorf("stored user id = %q, want the refresh's user-1", saved.UserID)
	}
}

// A refresh that names no user keeps the session's: it is the same sign-in.
func TestClient_RefreshKeepsTheSessionsUser(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mir_cli_rotated", "refresh_token": "mir_clr_rotated",
				"token_type": "Bearer", "expires_in": 3600,
				"organization": map[string]string{"id": "org-1"},
			})
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	expired := &auth.Credential{AccessToken: "mir_cli_stale", OrganizationID: "org-1", RefreshToken: "mir_clr_stale",
		ExpiresAt: time.Now().Add(-time.Hour), UserID: "user-1"}
	client := newTestClient(t, srv.URL, expired, "project-123")
	if err := client.Get(context.Background(), "/v1/identity", nil, &struct{}{}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	saved, err := auth.LoadCredential(client.dir, config.DefaultProfile)
	if err != nil {
		t.Fatalf("LoadCredential: %v", err)
	}
	if saved.RefreshToken != "mir_clr_rotated" || saved.UserID != "user-1" {
		t.Errorf("stored %q / user %q, want the rotated pair and user-1 kept", saved.RefreshToken, saved.UserID)
	}
}

func TestClient_RetriesOnceAfterUnexpected401(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "mir_cli_rotated",
				"refresh_token": "mir_clr_rotated",
				"token_type":    "Bearer",
				"expires_in":    3600,
				"organization":  map[string]string{"id": "org-1"},
			})
			return
		}
		if reads.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"UNAUTHENTICATED","message":"invalid or expired CLI token"}}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL, liveCredential(), "project-123")

	var out map[string]any
	if err := client.Get(context.Background(), "/v1/identity", nil, &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out["ok"] != true {
		t.Errorf("expected the retried request to succeed, got %v", out)
	}
	if got := reads.Load(); got != 2 {
		t.Errorf("reads = %d, want exactly 2 (one retry, not a loop)", got)
	}
}

func TestClient_SurfacesGatewayErrorEnvelope(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"code":"INVALID_ARGUMENT","message":"missing X-Mirador-Project header","details":[{"request_id":"req-9"}]}}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL, liveCredential(), "")

	err := client.Get(context.Background(), "/v1/traces", nil, &struct{}{})
	if err == nil {
		t.Fatal("expected an error")
	}
	apiErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.Message != "missing X-Mirador-Project header" {
		t.Errorf("Message = %q", apiErr.Message)
	}
	if apiErr.RequestID != "req-9" {
		t.Errorf("RequestID = %q, want it carried through for support", apiErr.RequestID)
	}
}

func TestClient_DoesNotRefreshUnderAServerKey(t *testing.T) {
	t.Parallel()
	var tokenCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			tokenCalls.Add(1)
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"UNAUTHENTICATED","message":"invalid API key"}}`))
	}))
	defer srv.Close()

	client, err := New(&config.Config{
		ProfileName: config.DefaultProfile,
		APIURL:      srv.URL,
		AuthURL:     srv.URL,
		APIKey:      "mir_srv_abc",
	}, Options{Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := client.Get(context.Background(), "/v1/identity", nil, &struct{}{}); err == nil {
		t.Fatal("expected the 401 to surface")
	}
	if got := tokenCalls.Load(); got != 0 {
		t.Errorf("token endpoint called %d times under a server key, want 0", got)
	}
}

// TestClient_RoutesCredentialCallsToTheAuthHost pins that a refresh triggered by a
// data-plane read goes to the auth host.
func TestClient_RoutesCredentialCallsToTheAuthHost(t *testing.T) {
	t.Parallel()
	var dataPaths, authPaths []string

	dataSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dataPaths = append(dataPaths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer mir_cli_rotated" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"UNAUTHENTICATED","message":"stale"}}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer dataSrv.Close()

	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authPaths = append(authPaths, r.URL.Path)
		if r.URL.Path == tokenPath {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "mir_cli_rotated",
				"refresh_token": "mir_clr_rotated",
				"token_type":    "Bearer",
				"expires_in":    3600,
				"organization":  map[string]string{"id": "org-1"},
			})
			return
		}
		w.Write([]byte(`{"projects":[]}`))
	}))
	defer authSrv.Close()

	expired := &auth.Credential{
		AccessToken:  "mir_cli_stale",
		RefreshToken: "mir_clr_stale",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}
	client := newSplitTestClient(t, dataSrv.URL, authSrv.URL, expired, "project-123")

	if err := client.Get(context.Background(), "/v1/traces", nil, &struct{}{}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := client.AuthGet(context.Background(), "/v1/projects", nil, &struct{}{}); err != nil {
		t.Fatalf("AuthGet: %v", err)
	}

	for _, p := range dataPaths {
		if p == tokenPath {
			t.Fatalf("a credential call reached the data host: %v", dataPaths)
		}
	}
	if len(authPaths) == 0 || authPaths[0] != tokenPath {
		t.Errorf("the refresh should have gone to the auth host first, got %v", authPaths)
	}
	if !slices.Contains(authPaths, "/v1/projects") {
		t.Errorf("project listing should go to the auth host, got %v", authPaths)
	}
	if !slices.Contains(dataPaths, "/v1/traces") {
		t.Errorf("trace reads should go to the data host, got %v", dataPaths)
	}
}

// TestClient_RefusesACredentialFromAnotherDeployment refuses a token minted by another
// auth host before sending it, naming both hosts.
func TestClient_RefusesACredentialFromAnotherDeployment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cred := liveCredential()
	cred.AuthURL = "https://auth.other.example"
	if _, err := auth.SaveCredential(dir, config.DefaultProfile, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	_, err := New(&config.Config{
		Dir:         dir,
		ProfileName: config.DefaultProfile,
		APIURL:      "https://api.terma.ai",
		AuthURL:     "https://auth.terma.ai",
	}, Options{Version: "test"})
	if err == nil {
		t.Fatal("expected a credential from another deployment to be refused")
	}

	if _, ok := errors.AsType[*auth.ErrWrongEnvironment](err); !ok {
		t.Fatalf("expected ErrWrongEnvironment, got %T: %v", err, err)
	}
	for _, want := range []string{"auth.other.example", "auth.terma.ai"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q, got %v", want, err)
		}
	}
}

// A credential needs an issuing host before the client can send it.
func TestClient_AcceptsACredentialFromTheSameEnvironment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		authURL string
		wantErr bool
	}{
		{"same host", "https://auth.terma.ai", false},
		{"missing issuing host", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cred := liveCredential()
			cred.AuthURL = tc.authURL
			if _, err := auth.SaveCredential(dir, config.DefaultProfile, cred); err != nil {
				t.Fatalf("seed credential: %v", err)
			}
			if _, err := New(&config.Config{
				Dir:         dir,
				ProfileName: config.DefaultProfile,
				APIURL:      "https://api.terma.ai",
				AuthURL:     "https://auth.terma.ai",
			}, Options{Version: "test"}); (err != nil) != tc.wantErr {
				t.Fatalf("New error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

// TestClient_StampsTheIssuingHostOnLogin records the issuing host on a new credential.
func TestClient_StampsTheIssuingHostOnLogin(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "mir_cli_x", "refresh_token": "mir_clr_x",
			"token_type": "Bearer", "expires_in": 3600,
			"organization": map[string]string{"id": "org-1"},
			"user":         map[string]string{"id": "user-1", "email": "dev@example.com"},
		})
	}))
	defer srv.Close()

	client := NewAnonymous(srv.URL, "test")

	cred, err := client.ExchangeCode(context.Background(), "mir_cod_x", "verifier", 54321)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if cred.AuthURL != srv.URL {
		t.Errorf("AuthURL = %q, want the host that minted it (%q)", cred.AuthURL, srv.URL)
	}
	if cred.UserID != "user-1" {
		t.Errorf("UserID = %q, want the token response's user-1", cred.UserID)
	}
}

// An error without the gateway's envelope keeps its message.
func TestAPIErrorKeepsItsMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  Error
		want string
	}{
		{"envelope", Error{StatusCode: 403, Code: "forbidden", Message: "no access", RequestID: "req_1"}, "no access (forbidden, request_id=req_1)"},
		{"shared gateway project hint", Error{StatusCode: 400, Code: "INVALID_ARGUMENT", Message: "missing X-Mirador-Project header — run `mirador project use <project>` or pass --project", RequestID: "req_1"}, "no team selected — run `terma setup` or pass --team (INVALID_ARGUMENT, request_id=req_1)"},
		{"shared gateway project remedy", Error{StatusCode: 400, Code: "INVALID_ARGUMENT", Message: "no such project in this organization — run `mirador project list`"}, "no such project in this organization — run `terma setup` (INVALID_ARGUMENT)"},
		{"shared gateway sign-in remedy", Error{StatusCode: 403, Code: "PERMISSION_DENIED", Message: "listing organizations requires a user credential — run `mirador login`"}, "listing organizations requires a user credential — run `terma setup` (PERMISSION_DENIED)"},
		{"code only", Error{StatusCode: 404, Code: "not_found", Message: "no such session"}, "no such session (not_found)"},
		{"request id without a code", Error{StatusCode: 500, Message: "boom", RequestID: "req_2"}, "boom (status 500) (request_id=req_2)"},
		{"plain body", Error{StatusCode: 502, Message: "upstream connect error"}, "upstream connect error (status 502)"},
		{"stream error without a code", Error{Message: "stream closed"}, "stream closed"},
		{"nothing at all", Error{StatusCode: 500}, "request failed with status 500"},
		{"nothing, not even a status", Error{}, "request failed"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseErrorBoundsAPlainBody(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, want string
	}{
		{"html page", "<html><body>Bad Gateway</body></html>", "Bad Gateway (status 502)"},
		{"multi-line", "upstream timed out\nstack trace follows\n...", "upstream timed out (status 502)"},
		{"empty", "", "Bad Gateway (status 502)"},
	} {
		resp := &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader(tc.body))}
		if got := parseError(resp).Error(); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A pair another process rotated while this client held the old one is taken from the
// store, not redeemed again: the server revokes a session whose spent refresh token
// comes back.
func TestClient_TakesAPairAnotherProcessRotated(t *testing.T) {
	t.Parallel()
	var redemptions atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			redemptions.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer mir_cli_rotated" {
			t.Errorf("read used %q, want the pair the other process stored", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	expired := &auth.Credential{AccessToken: "mir_cli_stale", RefreshToken: "mir_clr_stale", SessionID: "session-1",
		ExpiresAt: time.Now().Add(-time.Hour)}
	client := newTestClient(t, srv.URL, expired, "project-123")
	rotated := *expired
	rotated.AccessToken, rotated.RefreshToken, rotated.ExpiresAt = "mir_cli_rotated", "mir_clr_rotated", time.Now().Add(time.Hour)
	if err := auth.UpdateCredential(client.dir, config.DefaultProfile, &rotated); err != nil {
		t.Fatal(err)
	}

	if err := client.Get(context.Background(), "/v1/identity", nil, &struct{}{}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := redemptions.Load(); got != 0 {
		t.Errorf("redemptions = %d, want none", got)
	}
}

// A session the store no longer holds (one a sign-in replaced, being revoked) still
// refreshes, but its pair never overwrites the stored one.
func TestClient_RefreshOfASupersededSessionStoresNothing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mir_cli_old_rotated", "refresh_token": "mir_clr_old_rotated", "expires_in": 3600,
				"session_id": "session-old", "organization": map[string]string{"id": "org-test"},
			})
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	old := &auth.Credential{AccessToken: "mir_cli_old", RefreshToken: "mir_clr_old", SessionID: "session-old",
		ExpiresAt: time.Now().Add(-time.Hour)}
	client := newTestClient(t, srv.URL, old, "")
	signedIn := &auth.Credential{AccessToken: "mir_cli_new", RefreshToken: "mir_clr_new", SessionID: "session-new",
		ExpiresAt: time.Now().Add(time.Hour), OrganizationID: "org-test", AuthURL: srv.URL}
	if _, err := auth.SaveCredential(client.dir, config.DefaultProfile, signedIn); err != nil {
		t.Fatal(err)
	}

	if err := client.Get(context.Background(), "/v1/identity", nil, &struct{}{}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := client.Credential().AccessToken; got != "mir_cli_old_rotated" {
		t.Errorf("client token = %q, want its own session rotated", got)
	}
	stored, err := auth.LoadCredential(client.dir, config.DefaultProfile)
	if err != nil || stored.RefreshToken != "mir_clr_new" {
		t.Fatalf("stored = %+v, %v; want the new sign-in untouched", stored, err)
	}
}

// Under the lock the stored pair decides, not the one held: read without the lock, that
// can pair a rotation's tokens with the expiry before it, while another process's write
// has reached the keychain and not yet credentials.json. Neither refresh may rotate a
// token nobody refused, or that has not expired.
func TestClient_DecidesFromTheStoredPair(t *testing.T) {
	t.Parallel()
	var redemptions atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redemptions.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	torn := &auth.Credential{AccessToken: "mir_cli_rotated", RefreshToken: "mir_clr_rotated", SessionID: "session-1",
		ExpiresAt: time.Now().Add(-time.Hour)}
	client := newTestClient(t, srv.URL, torn, "")
	whole := *torn
	whole.ExpiresAt = time.Now().Add(time.Hour)
	if err := auth.UpdateCredential(client.dir, config.DefaultProfile, &whole); err != nil {
		t.Fatal(err)
	}

	if err := client.RefreshRefused(context.Background(), TokenSHA256("mir_cli_before")); err != nil {
		t.Fatalf("RefreshRefused: %v", err)
	}
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := redemptions.Load(); got != 0 {
		t.Errorf("redemptions = %d, want none: the stored token is neither refused nor expired", got)
	}
	if got := client.Credential().ExpiresAt; !got.Equal(whole.ExpiresAt) {
		t.Errorf("client expiry = %v, want the stored %v", got, whole.ExpiresAt)
	}
}

// A token endpoint that never answers holds the credential lock no longer than the
// request timeout: the refresh fails, and the next writer takes the lock at once.
func TestClient_RefreshOfAHungEndpointLetsTheLockGo(t *testing.T) {
	t.Parallel()
	hung := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hung:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(hung)
	expired := &auth.Credential{AccessToken: "mir_cli_stale", RefreshToken: "mir_clr_stale", SessionID: "session-1",
		ExpiresAt: time.Now().Add(-time.Hour)}
	client := newTestClient(t, srv.URL, expired, "project-123")
	const timeout = 300 * time.Millisecond
	client.http.Timeout = timeout

	start := time.Now()
	if err := client.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh against a hung token endpoint succeeded")
	}
	if elapsed := time.Since(start); elapsed > timeout+2*time.Second {
		t.Fatalf("Refresh took %v against a hung token endpoint; want about its %v request timeout", elapsed, timeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := auth.Locked(ctx, client.dir, func(*auth.Tx) error { return nil }); err != nil {
		t.Fatalf("credential lock after the refresh gave up: %v", err)
	}
}

// A refresh waits for the credential lock longer than its request timeout: a healthy
// holder may keep it through its own exchange and keychain writes, and the refresh behind
// it then takes the pair as that holder left it.
func TestClient_RefreshWaitsOutAHolderPastTheRequestTimeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "mir_cli_rotated", "refresh_token": "mir_clr_rotated",
			"token_type": "Bearer", "expires_in": 3600,
		})
	}))
	defer srv.Close()
	expired := &auth.Credential{AccessToken: "mir_cli_stale", RefreshToken: "mir_clr_stale", SessionID: "session-1",
		ExpiresAt: time.Now().Add(-time.Hour)}
	client := newTestClient(t, srv.URL, expired, "project-123")
	const timeout = 200 * time.Millisecond
	client.http.Timeout = timeout

	held, done := make(chan struct{}), make(chan error)
	go func() {
		done <- auth.Locked(t.Context(), client.dir, func(*auth.Tx) error {
			close(held)
			time.Sleep(4 * timeout)
			return nil
		})
	}()
	<-held
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh behind a holder that keeps the lock past the request timeout: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := client.Credential().RefreshToken; got != "mir_clr_rotated" {
		t.Errorf("refresh token = %q, want the rotated one", got)
	}
}

// A credential saved before sessions had ids matches any session. So a client holding one
// takes a fresh sign-in's pair from the store rather than redeeming its own legacy token.
func TestClient_ALegacyCredentialTakesAFreshSignIn(t *testing.T) {
	t.Parallel()
	var redemptions atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redemptions.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	legacy := &auth.Credential{AccessToken: "mir_cli_legacy", RefreshToken: "mir_clr_legacy", ExpiresAt: time.Now().Add(-time.Hour)}
	client := newTestClient(t, srv.URL, legacy, "")
	fresh := &auth.Credential{AccessToken: "mir_cli_fresh", RefreshToken: "mir_clr_fresh", SessionID: "session-new",
		ExpiresAt: time.Now().Add(time.Hour), OrganizationID: "org-test", AuthURL: srv.URL}
	if _, err := auth.SaveCredential(client.dir, config.DefaultProfile, fresh); err != nil {
		t.Fatal(err)
	}

	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := redemptions.Load(); got != 0 {
		t.Errorf("redemptions = %d, want none: the fresh sign-in has not expired", got)
	}
	if got := client.Credential().RefreshToken; got != "mir_clr_fresh" {
		t.Errorf("client refresh token = %q, want the fresh sign-in's", got)
	}
}

// A caller waits for this process's refresh slot only as long as its own context allows.
// A sibling that holds the slot while it waits minutes for the credential lock must not
// hold it up.
func TestClient_RefreshWaitFollowsTheCallersContext(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "mir_cli_rotated", "refresh_token": "mir_clr_rotated", "expires_in": 3600,
		})
	}))
	defer srv.Close()
	expired := &auth.Credential{AccessToken: "mir_cli_stale", RefreshToken: "mir_clr_stale", SessionID: "session-1",
		ExpiresAt: time.Now().Add(-time.Hour)}
	client := newTestClient(t, srv.URL, expired, "")

	// Another process holds the credential lock. It lets go when the test is done, or
	// after a few seconds, so a caller stuck behind the sibling fails rather than hangs.
	release, held, holder := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		holder <- auth.Locked(t.Context(), client.dir, func(*auth.Tx) error {
			close(held)
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
			return nil
		})
	}()
	<-held
	sibling := make(chan error, 1)
	go func() { sibling <- client.Refresh(context.Background()) }()
	for deadline := time.Now().Add(5 * time.Second); len(client.refreshing) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the sibling refresh never took the refresh slot")
		}
	}

	const wait = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	start := time.Now()
	err := client.Refresh(ctx)
	elapsed := time.Since(start)
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Refresh behind a waiting sibling = %v, want its own deadline", err)
	}
	if elapsed > 10*wait {
		t.Fatalf("Refresh waited %v behind a sibling; want about its %v deadline", elapsed, wait)
	}
	if err := <-sibling; err != nil {
		t.Fatalf("the sibling refresh: %v", err)
	}
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
}

// A credential the file indexes but whose keychain item is missing, perhaps only for a
// moment, is not a sign-out. The refresh must fail without redeeming anything: a rotation
// it could not store would leave a spent refresh token in the store.
func TestClient_RefreshRefusesAnIndexedCredentialWithoutTokens(t *testing.T) {
	t.Parallel()
	var redemptions atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redemptions.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	expired := &auth.Credential{AccessToken: "mir_cli_stale", RefreshToken: "mir_clr_stale", SessionID: "session-1",
		ExpiresAt: time.Now().Add(-time.Hour)}
	client := newTestClient(t, srv.URL, expired, "")
	// The keychain item auth keeps for the default profile's org-test credential.
	if err := secret.Delete(client.dir, "credential/"+config.DefaultProfile+"/org-test"); err != nil {
		t.Fatalf("the credential's keychain item: %v", err)
	}

	err := client.Refresh(context.Background())
	if err == nil || errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("Refresh = %v, want an error that is not a sign-out", err)
	}
	if n := redemptions.Load(); n != 0 {
		t.Fatalf("%d redemptions; want none", n)
	}
	if got := client.Credential().RefreshToken; got != "mir_clr_stale" {
		t.Errorf("client refresh token = %q, want its pair untouched", got)
	}
}
