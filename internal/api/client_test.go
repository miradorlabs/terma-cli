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

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// newTestClient builds a client against a stub server with a CLI credential,
// pointing the credential store at a temp dir so a refresh cannot touch the
// developer's real ~/.config/terma.
func newTestClient(t *testing.T, url string, cred *auth.Credential, projectID string) *Client {
	t.Helper()
	return newSplitTestClient(t, url, url, cred, projectID)
}

// newSplitTestClient points the two surfaces at (possibly) different servers, which is
// how they are deployed: api.mirador.org and auth.mirador.org.
func newSplitTestClient(t *testing.T, apiURL, authURL string, cred *auth.Credential, projectID string) *Client {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	cred.AuthURL = authURL
	if cred.OrganizationID == "" {
		cred.OrganizationID = "org-test"
	}

	if _, err := auth.SaveCredential(config.DefaultProfile, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	client, err := New(&config.Config{
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
	// Without this header every project-scoped read is a 400 — it is the whole
	// mechanism by which a project-less credential picks a project.
	if gotProject != "project-123" {
		t.Errorf("%s = %q, want project-123", projectHeader, gotProject)
	}
}

func TestClient_ServerKeyDoesNotSendProjectHeader(t *testing.T) {
	var gotAuth, gotProject string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotProject = r.Header.Get(projectHeader)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
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
	// The key's grant already fixes the project; sending a header would imply a
	// scope the key does not have.
	if gotProject != "" {
		t.Errorf("%s = %q, want it omitted under a server key", projectHeader, gotProject)
	}
}

func TestClient_RefreshesExpiredTokenBeforeRequesting(t *testing.T) {
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

	// A known-expired token refreshes up front, so the read costs one request
	// rather than a guaranteed 401 followed by a retry.
	if got := refreshes.Load(); got != 1 {
		t.Errorf("refreshes = %d, want 1", got)
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("reads = %d, want 1 (no wasted 401 round trip)", got)
	}

	// The rotated pair must be persisted: the server already invalidated the old
	// refresh token, so losing the new one would strand the session.
	saved, err := auth.LoadCredential(config.DefaultProfile)
	if err != nil {
		t.Fatalf("LoadCredential: %v", err)
	}
	if saved.RefreshToken != "mir_clr_rotated" {
		t.Errorf("stored refresh token = %q, want the rotated one", saved.RefreshToken)
	}
}

func TestClient_RetriesOnceAfterUnexpected401(t *testing.T) {
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
		// First read rejects a token the client believed was live — what a
		// revocation or an out-of-band rotation looks like.
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
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	// The gateway's message names the fix, so it has to reach the user intact.
	if apiErr.Message != "missing X-Mirador-Project header" {
		t.Errorf("Message = %q", apiErr.Message)
	}
	if apiErr.RequestID != "req-9" {
		t.Errorf("RequestID = %q, want it carried through for support", apiErr.RequestID)
	}
}

func TestClient_DoesNotRefreshUnderAServerKey(t *testing.T) {
	var tokenCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			tokenCalls.Add(1)
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"UNAUTHENTICATED","message":"invalid API key"}}`))
	}))
	defer srv.Close()

	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
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
	// A server key has nothing to refresh; attempting it would be a pointless
	// round trip and could mask a genuinely bad key.
	if got := tokenCalls.Load(); got != 0 {
		t.Errorf("token endpoint called %d times under a server key, want 0", got)
	}
}

// TestClient_RoutesCredentialCallsToTheAuthHost pins the two-host split: a refresh must
// go to the auth surface even though it was triggered by a data-plane read. Sending it
// to the data host would leak a refresh token to a service that cannot honour it and
// would fail every login on a real deployment.
func TestClient_RoutesCredentialCallsToTheAuthHost(t *testing.T) {
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

// TestClient_RefusesACredentialFromAnotherDeployment covers pointing the CLI at one
// deployment, logging in, then pointing it at another. Without this the second auth host
// answers 401 and the message reads like a broken login rather than a wrong endpoint —
// and the first deployment's token has been handed to the second on the way.
func TestClient_RefusesACredentialFromAnotherDeployment(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	cred := liveCredential()
	cred.AuthURL = "https://auth.other.example"
	if _, err := auth.SaveCredential(config.DefaultProfile, cred); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	_, err := New(&config.Config{
		ProfileName: config.DefaultProfile,
		APIURL:      "https://api.mirador.org",
		AuthURL:     "https://auth.mirador.org",
	}, Options{Version: "test"})
	if err == nil {
		t.Fatal("expected a credential from another deployment to be refused")
	}

	if _, ok := errors.AsType[*auth.ErrWrongEnvironment](err); !ok {
		t.Fatalf("expected ErrWrongEnvironment, got %T: %v", err, err)
	}
	// Both hosts must appear, or the message does not tell you what to fix.
	for _, want := range []string{"auth.other.example", "auth.mirador.org"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q, got %v", want, err)
		}
	}
}

// A credential needs an issuing host before the client can send it.
func TestClient_AcceptsACredentialFromTheSameEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		authURL string
		wantErr bool
	}{
		{"same host", "https://auth.mirador.org", false},
		{"missing issuing host", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
			cred := liveCredential()
			cred.AuthURL = tc.authURL
			if _, err := auth.SaveCredential(config.DefaultProfile, cred); err != nil {
				t.Fatalf("seed credential: %v", err)
			}
			if _, err := New(&config.Config{
				ProfileName: config.DefaultProfile,
				APIURL:      "https://api.mirador.org",
				AuthURL:     "https://auth.mirador.org",
			}, Options{Version: "test"}); (err != nil) != tc.wantErr {
				t.Fatalf("New error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

// TestClient_StampsTheIssuingHostOnLogin is what makes the guard above possible.
func TestClient_StampsTheIssuingHostOnLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "mir_cli_x", "refresh_token": "mir_clr_x",
			"token_type": "Bearer", "expires_in": 3600,
			"organization": map[string]string{"id": "org-1"},
		})
	}))
	defer srv.Close()

	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	client := NewAnonymous(srv.URL, "test")

	cred, err := client.ExchangeCode(context.Background(), "mir_cod_x", "verifier", 54321)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if cred.AuthURL != srv.URL {
		t.Errorf("AuthURL = %q, want the host that minted it (%q)", cred.AuthURL, srv.URL)
	}
}

// parseError fills Message from a plain body or the status text, and Error() used to
// discard it unless the gateway had also sent a code or a request id — so a proxy's
// 502 read "request failed with status 502" and a stream error with no code read
// "request failed with status 0".
func TestAPIErrorKeepsItsMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  APIError
		want string
	}{
		{"envelope", APIError{StatusCode: 403, Code: "forbidden", Message: "no access", RequestID: "req_1"}, "no access (forbidden, request_id=req_1)"},
		{"shared gateway project hint", APIError{StatusCode: 400, Code: "INVALID_ARGUMENT", Message: "missing X-Mirador-Project header — run `mirador project use <project>` or pass --project", RequestID: "req_1"}, "no project selected — run `terma install` in this repository or pass --project (INVALID_ARGUMENT, request_id=req_1)"},
		{"code only", APIError{StatusCode: 404, Code: "not_found", Message: "no such session"}, "no such session (not_found)"},
		{"request id without a code", APIError{StatusCode: 500, Message: "boom", RequestID: "req_2"}, "boom (status 500) (request_id=req_2)"},
		{"plain body", APIError{StatusCode: 502, Message: "upstream connect error"}, "upstream connect error (status 502)"},
		{"stream error without a code", APIError{Message: "stream closed"}, "stream closed"},
		{"nothing at all", APIError{StatusCode: 500}, "request failed with status 500"},
		{"nothing, not even a status", APIError{}, "request failed"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseErrorBoundsAPlainBody(t *testing.T) {
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
