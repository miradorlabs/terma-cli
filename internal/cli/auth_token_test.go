package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

const (
	grantOrg  = "11111111-1111-4111-8111-111111111111"
	grantTeam = "22222222-2222-4222-8222-222222222222"
)

// tokenServer is the auth host as its reuse detection behaves: redeeming a refresh token
// that was already rotated revokes the whole session, and every token with it.
type tokenServer struct {
	*httptest.Server
	// delay holds each redemption open, so racing refreshes overlap.
	delay time.Duration
	// redeeming is closed when the first redemption arrives.
	redeeming chan struct{}
	arrived   sync.Once

	mu                 sync.Mutex
	access, refresh    string
	spent              map[string]bool
	revoked            bool
	redemptions, fails int
	// refuseProjects answers /v1/projects 401 even to a valid token.
	refuseProjects bool
}

func newTokenServer(t *testing.T) *tokenServer {
	t.Helper()
	s := &tokenServer{access: "mir_cli_0", refresh: "mir_clr_0", spent: map[string]bool{}, redeeming: make(chan struct{})}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *tokenServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/auth/cli/token" {
		s.redeem(w, r)
		return
	}
	s.mu.Lock()
	ok := !s.revoked && r.Header.Get("Authorization") == "Bearer "+s.access &&
		(!s.refuseProjects || r.URL.Path != "/v1/projects")
	s.mu.Unlock()
	if !ok {
		unauthorized(w)
		return
	}
	switch r.URL.Path {
	case "/v1/projects":
		_ = json.NewEncoder(w).Encode(map[string]any{"projects": []map[string]string{
			{"id": grantTeam, "name": "Platform", "organization_id": grantOrg},
			{"id": "33333333-3333-4333-8333-333333333333", "name": "Growth", "organization_id": grantOrg},
		}})
	case "/v1/whoami":
		_ = json.NewEncoder(w).Encode(map[string]string{"organization_id": grantOrg, "user_id": "user-1", "email": "dev@example.com"})
	case "/v1/organizations":
		_ = json.NewEncoder(w).Encode(map[string]any{"organizations": []map[string]string{{"id": grantOrg, "name": "Acme"}}})
	default:
		http.NotFound(w, r)
	}
}

func (s *tokenServer) redeem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.arrived.Do(func() { close(s.redeeming) })
	time.Sleep(s.delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spent[req.RefreshToken] {
		s.revoked = true
	}
	if s.revoked || req.RefreshToken != s.refresh {
		s.fails++
		unauthorized(w)
		return
	}
	s.spent[req.RefreshToken] = true
	s.redemptions++
	s.access, s.refresh = fmt.Sprintf("mir_cli_%d", s.redemptions), fmt.Sprintf("mir_clr_%d", s.redemptions)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": s.access, "refresh_token": s.refresh, "token_type": "Bearer", "expires_in": 3600,
		"session_id": "session-1", "organization": map[string]string{"id": grantOrg, "name": "Acme"},
		"user": map[string]string{"id": "user-1", "email": "dev@example.com"},
	})
}

// rotate stands for another process's refresh, which the server saw and the store did not.
func (s *tokenServer) rotate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spent[s.refresh] = true
	s.redemptions++
	s.access, s.refresh = fmt.Sprintf("mir_cli_%d", s.redemptions), fmt.Sprintf("mir_clr_%d", s.redemptions)
}

func (s *tokenServer) state() (access, refresh string, redemptions, fails int, revoked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.access, s.refresh, s.redemptions, s.fails, s.revoked
}

func unauthorized(w http.ResponseWriter) {
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHENTICATED","message":"invalid or expired CLI token"}}`))
}

// signInForToken points the commands at srv and stores the server's current pair as the
// profile's sign-in, expiring at expires, with team chosen. The tokens are kept in
// credentials.json, which a built terma reads as this test does; the keychain it would
// use is the developer's own.
func signInForToken(t *testing.T, srv *tokenServer, expires time.Time, team string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	useConfigDir(t, dir)
	t.Setenv("TERMA_AUTH_URL", srv.URL)
	t.Setenv("TERMA_API_URL", srv.URL)
	t.Setenv("TERMA_API_KEY", "")
	t.Setenv("TERMA_TEAM_ID", "")
	t.Setenv("TERMA_MCP_URL", "")
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(dir, config.DefaultProfile, func(p *config.Profile) {
		p.OrganizationID, p.Team = grantOrg, team
	}); err != nil {
		t.Fatal(err)
	}
	access, refresh, _, _, _ := srv.state()
	if _, err := auth.SaveCredential(dir, config.DefaultProfile, &auth.Credential{
		AccessToken: access, RefreshToken: refresh, ExpiresAt: expires, SessionID: "session-1",
		AuthURL: srv.URL, OrganizationID: grantOrg, UserID: "user-1",
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// assertTokenGrant checks stdout is exactly one grant, carrying want, and nothing of a
// refresh token.
func assertTokenGrant(t *testing.T, stdout, want string) tokenGrant {
	t.Helper()
	if strings.Contains(stdout, "mir_clr_") || strings.Contains(stdout, "refresh") {
		t.Fatalf("stdout carries a refresh token:\n%s", stdout)
	}
	var grant tokenGrant
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&grant); err != nil {
		t.Fatalf("stdout is not a grant: %v\n%s", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout holds more than one object:\n%s", stdout)
	}
	if grant.AccessToken != want {
		t.Errorf("access_token = %q, want %q", grant.AccessToken, want)
	}
	if _, err := time.Parse(time.RFC3339, grant.ExpiresAt); err != nil || !strings.HasSuffix(grant.ExpiresAt, "Z") {
		t.Errorf("expires_at = %q, want RFC 3339 in UTC", grant.ExpiresAt)
	}
	if grant.OrganizationID != grantOrg || grant.TeamID != grantTeam || len(grant.Teams) != 2 || grant.Teams[0].Name != "Platform" {
		t.Errorf("grant = %+v, want the organization, the chosen team and both teams", grant)
	}
	return grant
}

// assertTokenError checks the command failed with stdout exactly one error of code,
// naming `terma setup`.
func assertTokenError(t *testing.T, stdout string, err error, code string) {
	t.Helper()
	assertTokenFailure(t, stdout, err, code, "terma setup")
}

// assertTokenFailure checks the command failed with stdout exactly one error of code,
// whose message names fix.
func assertTokenFailure(t *testing.T, stdout string, err error, code, fix string) {
	t.Helper()
	if c, ok := exitCodeOf(err); !ok || c == 0 {
		t.Fatalf("err = %v, want a non-zero exit", err)
	}
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil || dec.More() {
		t.Fatalf("stdout is not one error object (%v):\n%s", err, stdout)
	}
	if out.Error.Code != code || !strings.Contains(out.Error.Message, fix) {
		t.Errorf("error = %+v, want code %q and a message naming %s", out.Error, code, fix)
	}
}

func TestAuthToken_PrintsTheStoredTokenWhileFresh(t *testing.T) {
	t.Setenv("TERMA_ENV", config.EnvDev)
	srv := newTokenServer(t)
	signInForToken(t, srv, time.Now().Add(time.Hour), grantTeam)

	stdout, stderr, err := termaRun{}.exec(t, "auth", "token", "--json")
	if err != nil {
		t.Fatalf("auth token: %v\n%s", err, stderr)
	}
	grant := assertTokenGrant(t, stdout, "mir_cli_0")
	if grant.MCPURL != "https://mcp-dev.terma.ai/mcp" {
		t.Errorf("mcp_url = %q, want dev's under TERMA_ENV=dev", grant.MCPURL)
	}
	if _, _, redemptions, _, _ := srv.state(); redemptions != 0 {
		t.Errorf("redemptions = %d, want none for a fresh token", redemptions)
	}
}

func TestAuthToken_RefreshesAnExpiringToken(t *testing.T) {
	srv := newTokenServer(t)
	dir := signInForToken(t, srv, time.Now().Add(30*time.Second), grantTeam)

	stdout, stderr, err := termaRun{}.exec(t, "auth", "token", "--json")
	if err != nil {
		t.Fatalf("auth token: %v\n%s", err, stderr)
	}
	assertTokenGrant(t, stdout, "mir_cli_1")
	stored, err := auth.LoadCredential(dir, config.DefaultProfile)
	if err != nil || stored.RefreshToken != "mir_clr_1" {
		t.Fatalf("stored = %+v, %v; want the rotated pair persisted", stored, err)
	}
}

// --force-refresh rotates only while the stored token is the one refused: a second caller
// that saw the same refusal gets the first one's rotation.
func TestAuthToken_ForceRefreshRotatesOnlyTheRefusedToken(t *testing.T) {
	srv := newTokenServer(t)
	signInForToken(t, srv, time.Now().Add(time.Hour), grantTeam)
	refused := api.TokenSHA256("mir_cli_0")

	stdout, _, err := termaRun{}.exec(t, "auth", "token", "--json", "--force-refresh", "--refused-token-sha256="+api.TokenSHA256("mir_cli_other"))
	if err != nil {
		t.Fatal(err)
	}
	assertTokenGrant(t, stdout, "mir_cli_0")
	if _, _, redemptions, _, _ := srv.state(); redemptions != 0 {
		t.Fatalf("redemptions = %d after a refusal of another token, want none", redemptions)
	}

	for range 2 {
		stdout, stderr, err := termaRun{}.exec(t, "auth", "token", "--json", "--force-refresh", "--refused-token-sha256="+strings.ToUpper(refused))
		if err != nil {
			t.Fatalf("auth token --force-refresh: %v\n%s", err, stderr)
		}
		assertTokenGrant(t, stdout, "mir_cli_1")
	}
	if _, _, redemptions, _, revoked := srv.state(); redemptions != 1 || revoked {
		t.Fatalf("redemptions = %d, revoked = %v; want one rotation for the one refusal", redemptions, revoked)
	}
}

// Arguments this terma does not take fail as the contract says every failure does: one
// error object on stdout, which a bridge built for another terma can read.
func TestAuthToken_RejectsMalformedFlags(t *testing.T) {
	srv := newTokenServer(t)
	signInForToken(t, srv, time.Now().Add(time.Hour), grantTeam)
	for _, args := range [][]string{
		{"auth", "token"},
		{"auth", "token", "--json", "--force-refresh"},
		{"auth", "token", "--json", "--refused-token-sha256=" + api.TokenSHA256("x")},
		{"auth", "token", "--json", "--force-refresh", "--refused-token-sha256=mir_cli_0"},
		{"auth", "token", "--json", "--refused-token=" + api.TokenSHA256("x")},
		{"auth", "token", "--json", "--force-refresh=maybe"},
		{"auth", "token", "--json", "extra"},
	} {
		stdout, _, err := termaRun{}.exec(t, args...)
		if strings.Contains(stdout, "mir_cli_") {
			t.Errorf("%v printed a token: %s", args, stdout)
		}
		assertTokenFailure(t, stdout, err, "unavailable", "terma update")
	}
}

// A bad TERMA_MCP_URL fails the one command that hands it out, naming it.
func TestAuthToken_RefusesACleartextMCPURL(t *testing.T) {
	srv := newTokenServer(t)
	signInForToken(t, srv, time.Now().Add(time.Hour), grantTeam)
	t.Setenv("TERMA_MCP_URL", "http://mcp.example/mcp")
	stdout, _, err := termaRun{}.exec(t, "auth", "token", "--json")
	assertTokenFailure(t, stdout, err, "unavailable", "TERMA_MCP_URL")
	if _, _, err := (termaRun{}).exec(t, "config", "show"); err != nil {
		t.Errorf("config show with a bad TERMA_MCP_URL: %v; want only auth token to fail", err)
	}
}

func TestAuthToken_ErrorCodes(t *testing.T) {
	t.Run("signed out", func(t *testing.T) {
		srv := newTokenServer(t)
		dir := signInForToken(t, srv, time.Now().Add(time.Hour), grantTeam)
		if err := auth.DeleteCredential(dir, config.DefaultProfile); err != nil {
			t.Fatal(err)
		}
		stdout, _, err := termaRun{}.exec(t, "auth", "token", "--json")
		assertTokenError(t, stdout, err, "signed_out")
	})
	t.Run("server key", func(t *testing.T) {
		srv := newTokenServer(t)
		signInForToken(t, srv, time.Now().Add(time.Hour), grantTeam)
		t.Setenv("TERMA_API_KEY", testServerKey)
		stdout, _, err := termaRun{}.exec(t, "auth", "token", "--json")
		assertTokenError(t, stdout, err, "signed_out")
		if !strings.Contains(stdout, "server key cannot be used for the MCP") || strings.Contains(stdout, testServerKey) {
			t.Errorf("stdout = %s, want it to say a server key is no MCP credential, without the key", stdout)
		}
	})
	t.Run("revoked", func(t *testing.T) {
		srv := newTokenServer(t)
		signInForToken(t, srv, time.Now().Add(-time.Minute), grantTeam)
		srv.mu.Lock()
		srv.revoked = true
		srv.mu.Unlock()
		stdout, _, err := termaRun{}.exec(t, "auth", "token", "--json")
		assertTokenError(t, stdout, err, "revoked")
	})
	t.Run("rejected after a refresh", func(t *testing.T) {
		srv := newTokenServer(t)
		signInForToken(t, srv, time.Now().Add(time.Hour), grantTeam)
		srv.mu.Lock()
		srv.refuseProjects = true
		srv.mu.Unlock()
		stdout, _, err := termaRun{}.exec(t, "auth", "token", "--json")
		assertTokenError(t, stdout, err, "revoked")
	})
	t.Run("no team", func(t *testing.T) {
		srv := newTokenServer(t)
		signInForToken(t, srv, time.Now().Add(time.Hour), "")
		stdout, _, err := termaRun{}.exec(t, "auth", "token", "--json")
		assertTokenError(t, stdout, err, "no_team")
	})
	t.Run("team gone", func(t *testing.T) {
		srv := newTokenServer(t)
		signInForToken(t, srv, time.Now().Add(time.Hour), "44444444-4444-4444-8444-444444444444")
		stdout, _, err := termaRun{}.exec(t, "auth", "token", "--json")
		assertTokenError(t, stdout, err, "no_team")
	})
	t.Run("unavailable", func(t *testing.T) {
		srv := newTokenServer(t)
		signInForToken(t, srv, time.Now().Add(-time.Minute), grantTeam)
		srv.Close()
		stdout, _, err := termaRun{}.exec(t, "auth", "token", "--json")
		if c, ok := exitCodeOf(err); !ok || c == 0 || !strings.Contains(stdout, `"code":"unavailable"`) {
			t.Errorf("err = %v, stdout %s; want unavailable", err, stdout)
		}
	})
}

// A setup run again, reusing the stored session, must not write back the pair it read
// once another process has rotated it: the next refresh would redeem a spent token and
// the server would revoke the session.
func TestReuseStoredSessionKeepsAPairRotatedSince(t *testing.T) {
	srv := newTokenServer(t)
	dir := signInForToken(t, srv, time.Now().Add(time.Hour), grantTeam)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	stale, err := auth.LoadCredential(dir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	// Another process rotates the pair after setup read it, while the read access token
	// still works.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/whoami" && r.Header.Get("Authorization") == "Bearer "+stale.AccessToken {
			srv.rotate()
			access, refresh, _, _, _ := srv.state()
			if err := auth.UpdateCredential(dir, config.DefaultProfile, &auth.Credential{
				AccessToken: access, RefreshToken: refresh, ExpiresAt: time.Now().Add(time.Hour), SessionID: "session-1",
				AuthURL: srv.URL, OrganizationID: grantOrg, UserID: "user-1",
			}); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"organization_id": grantOrg, "user_id": "user-1", "email": "dev@example.com"})
			return
		}
		srv.serve(w, r)
	})

	res, ok, err := testApp.reuseStoredSession(t.Context(), cfg, orgRef{})
	if err != nil || !ok {
		t.Fatalf("reuseStoredSession = %v, %v", ok, err)
	}
	stored, err := auth.LoadCredential(dir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "mir_clr_1" || res.cred.RefreshToken != "mir_clr_1" {
		t.Fatalf("stored %q, returned %q; want the pair rotated since, mir_clr_1", stored.RefreshToken, res.cred.RefreshToken)
	}
	if stored.UserEmail != "dev@example.com" {
		t.Errorf("stored email = %q, want what whoami taught setup kept", stored.UserEmail)
	}

	// The refresh token stored is the live one: redeeming it rotates, and revokes nothing.
	stdout, stderr, err := termaRun{}.exec(t, "auth", "token", "--json", "--force-refresh", "--refused-token-sha256="+api.TokenSHA256("mir_cli_1"))
	if err != nil {
		t.Fatalf("auth token after setup: %v\n%s", err, stderr)
	}
	assertTokenGrant(t, stdout, "mir_cli_2")
	if _, _, _, fails, revoked := srv.state(); fails != 0 || revoked {
		t.Fatalf("fails = %d, revoked = %v; setup handed the refresh a spent token", fails, revoked)
	}
}
