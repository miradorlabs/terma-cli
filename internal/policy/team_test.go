package policy

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// Every refusal is classified by status, AIP-193 reason, and typed error — never by
// message text.
func TestClassifyByStatusAndReason(t *testing.T) {
	t.Parallel()
	// Decoy message carries both reason names to verify Message is never inspected.
	refusal := func(status int, reason string) *api.Error {
		return &api.Error{StatusCode: status, Reason: reason, Message: "ORG_NOT_ACCESSIBLE PROJECT_NOT_FOUND SIGN_IN_RETIRED"}
	}
	for _, tc := range []struct {
		name string
		err  error
		want routing.ErrorClass
	}{
		{"nil error", nil, ""},
		{"403 ORG_NOT_ACCESSIBLE", refusal(403, api.ReasonOrgNotAccessible), routing.OrgAccessLost},
		{"wrapped 403 ORG_NOT_ACCESSIBLE", fmt.Errorf("fetch: %w", refusal(403, api.ReasonOrgNotAccessible)), routing.OrgAccessLost},
		{"403 PROJECT_NOT_FOUND", fmt.Errorf("fetch: %w", refusal(403, api.ReasonProjectNotFound)), routing.Terminal},
		{"404 PROJECT_NOT_FOUND is transient", refusal(404, api.ReasonProjectNotFound), routing.Transient},
		{"403 AUTHORIZATION_UNAVAILABLE is transient", refusal(403, api.ReasonAuthorizationUnavailable), routing.Transient},
		{"bare 403 is transient", refusal(403, ""), routing.Transient},
		{"401 with ORG_NOT_ACCESSIBLE is sign_in", refusal(401, api.ReasonOrgNotAccessible), routing.SignIn},
		{"bare 401 is sign_in", refusal(401, ""), routing.SignIn},
		{"SIGN_IN_RETIRED error", auth.ErrSignInRetired, routing.SignIn},
		{"403 SIGN_IN_RETIRED api error", refusal(403, api.ReasonSignInRetired), routing.SignIn},
		{"no login on file", auth.ErrNotLoggedIn, routing.SignIn},
		{"login from another environment", &auth.ErrWrongEnvironment{IssuedBy: "a", ConfiguredAs: "b"}, routing.SignIn},
		{"account mismatch is sign_in", ErrAccountMismatch, routing.SignIn},
		{"5xx is transient", refusal(503, ""), routing.Transient},
		{"429 is transient", refusal(429, ""), routing.Transient},
		{"network error is transient", errors.New("dial tcp: connection refused"), routing.Transient},
		{"team from another environment is transient", ErrOtherEnvironment, routing.Transient},
	} {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("%s: Classify = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// FetchTeam and RefreshTeam read any organization's team using the profile's single user
// token, parse 2.x wildcards, stamp the team/org/authURL, and defer policy file writes so
// a stale-epoch refresh never overwrites policies/<team>.json before ApplyRefresh runs.
func TestFetchTeamUsesSingleUserToken(t *testing.T) {
	var mu sync.Mutex
	var bearer, named string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearer = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		named = r.URL.Query().Get("project_id")
		mu.Unlock()
		fmt.Fprint(w, `{"policy":{"version":"2.0","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":true},"per_repository":{"repositories":["github.com/acme/*"]}}},"revision":7,"updated_at":"2026-11-13T12:00:00Z"}`)
	}))
	defer srv.Close()

	cfg := setUp(t, "")
	cfg.AuthURL = srv.URL
	cred := &auth.Credential{
		AccessToken: "user-token-fixture",
		AuthURL:     srv.URL,
		SessionID:   "sess-1",
		UserID:      "user-1",
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	if _, err := auth.SaveCredential(cfg.Dir, cfg.ProfileName, cred); err != nil {
		t.Fatal(err)
	}
	cfg.Account = cred.Account()
	settings, _ := os.ReadFile(config.Path(cfg.Dir))

	// Seed routing.State and an existing policy file at revision 5 to verify RefreshTeam
	// does not overwrite the policy file prior to a committed routing.Update.
	seedPol := config.Policy{Mode: config.ModeRepo, TeamID: "team-b", OrganizationID: "org_b", AuthURL: srv.URL, Revision: 5, FetchedAt: time.Now().UTC()}
	var enrID string
	st, err := routing.Create(cfg.StateDir, cfg.ProfileName, cfg.Account, func(s *routing.State) error {
		var selErr error
		enrID, selErr = s.Select(routing.Team{OrganizationID: "org_b", AuthURL: srv.URL, TeamID: "team-b", Policy: seedPol})
		return selErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := routing.StoreTeamPolicy(cfg.StateDir, *st.Team(enrID)); err != nil {
		t.Fatal(err)
	}

	r, err := (Source{}).RefreshTeam(t.Context(), cfg, *st.Team(enrID))
	if err != nil || r.Policy == nil || r.Epoch != 0 || r.EnrollmentID != enrID || r.At.IsZero() {
		t.Fatalf("RefreshTeam = %+v, %v", r, err)
	}
	mu.Lock()
	gotBearer, gotNamed := bearer, named
	mu.Unlock()
	if gotBearer != "user-token-fixture" || gotNamed != "team-b" {
		t.Fatalf("asked with %q for %q, want user token for team-b", gotBearer, gotNamed)
	}
	p := *r.Policy
	if !p.Wildcards || p.TeamID != "team-b" || p.OrganizationID != "org_b" || p.AuthURL != srv.URL || p.Revision != 7 || !p.IncludePrompts || p.IncludeToolContent {
		t.Fatalf("policy = %+v", p)
	}
	// Policy file stays at revision 5 until routing.Update commits the epoch-checked ApplyRefresh.
	if stored, ok, err := config.ReadPolicy(cfg.StateDir, "team-b"); err != nil || !ok || stored.Revision != 5 {
		t.Fatalf("RefreshTeam prematurely overwrote policy file: %+v, %v, %v", stored, ok, err)
	}
	var changed bool
	committed, err := routing.Update(cfg.StateDir, cfg.ProfileName, st.Expect(), func(s *routing.State) error {
		var applyErr error
		changed, applyErr = s.ApplyRefresh(r)
		return applyErr
	})
	if err != nil || !changed {
		t.Fatalf("routing.Update(ApplyRefresh) = %v, %v", changed, err)
	}
	if err := routing.StoreTeamPolicy(cfg.StateDir, *committed.Team(enrID)); err != nil {
		t.Fatal(err)
	}
	if stored, ok, err := config.ReadPolicy(cfg.StateDir, "team-b"); err != nil || !ok || stored.Revision != 7 {
		t.Fatalf("StoreTeamPolicy did not write policy: %+v, %v, %v", stored, ok, err)
	}
	if after, _ := os.ReadFile(config.Path(cfg.Dir)); !bytes.Equal(after, settings) {
		t.Fatalf("StoreTeamPolicy rewrote config.json:\n%s", after)
	}
}

// RefreshTeam surfaces per-request 403 ORG_NOT_ACCESSIBLE as OrgAccessLost, 403
// PROJECT_NOT_FOUND as Terminal, and fails closed on missing login, wrong environment,
// empty expected account, or account mismatch without hitting the server.
func TestFetchTeamFailsClosed(t *testing.T) {
	var asked atomic.Int32
	var mu sync.Mutex
	reason := "ORG_NOT_ACCESSIBLE"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		mu.Lock()
		cur := reason
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"error":{"code":"PERMISSION_DENIED","reason":%q,"message":"no access"}}`, cur)
	}))
	defer srv.Close()

	cfg := setUp(t, "")
	cfg.AuthURL = srv.URL
	tm := routing.Team{EnrollmentID: "e", OrganizationID: "org_b", AuthURL: srv.URL, TeamID: "team-b"}

	// No login -> SignIn, zero HTTP calls.
	r, err := (Source{}).RefreshTeam(t.Context(), cfg, tm)
	if err == nil || r.Failure != routing.SignIn || r.Policy != nil || asked.Load() != 0 {
		t.Fatalf("signed out: %+v, %v, asked=%d", r, err, asked.Load())
	}

	// Save Alice's login.
	alice := &auth.Credential{
		AccessToken: "alice-token-fixture",
		AuthURL:     srv.URL,
		SessionID:   "sess-a",
		UserID:      "alice",
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	if _, err := auth.SaveCredential(cfg.Dir, cfg.ProfileName, alice); err != nil {
		t.Fatal(err)
	}

	// Team from another environment -> ErrOtherEnvironment / Transient, zero HTTP calls.
	otherEnvTeam := routing.Team{EnrollmentID: "e", OrganizationID: "org_a", AuthURL: "https://auth.elsewhere", TeamID: "team-a"}
	r, err = (Source{}).RefreshTeam(t.Context(), cfg, otherEnvTeam)
	if !errors.Is(err, ErrOtherEnvironment) || r.Failure != routing.Transient || asked.Load() != 0 {
		t.Fatalf("other environment team: %+v, %v, asked=%d", r, err, asked.Load())
	}

	// Empty cfg.Account -> ErrAccountMismatch / SignIn, zero HTTP calls.
	cfg.Account = ""
	r, err = (Source{}).RefreshTeam(t.Context(), cfg, tm)
	if !errors.Is(err, ErrAccountMismatch) || r.Failure != routing.SignIn || asked.Load() != 0 {
		t.Fatalf("empty account: %+v, %v, asked=%d", r, err, asked.Load())
	}

	// Account mismatch -> ErrAccountMismatch / SignIn, zero HTTP calls.
	cfg.Account = auth.AccountKey(srv.URL, "bob")
	r, err = (Source{}).RefreshTeam(t.Context(), cfg, tm)
	if !errors.Is(err, ErrAccountMismatch) || r.Failure != routing.SignIn || asked.Load() != 0 {
		t.Fatalf("account mismatch: %+v, %v, asked=%d", r, err, asked.Load())
	}

	// Matching account receives 403 ORG_NOT_ACCESSIBLE -> OrgAccessLost.
	cfg.Account = alice.Account()
	r, err = (Source{}).RefreshTeam(t.Context(), cfg, tm)
	if err == nil || r.Failure != routing.OrgAccessLost || asked.Load() != 1 {
		t.Fatalf("403 ORG_NOT_ACCESSIBLE: %+v, %v, asked=%d", r, err, asked.Load())
	}

	// Matching account receives 403 PROJECT_NOT_FOUND -> Terminal.
	mu.Lock()
	reason = "PROJECT_NOT_FOUND"
	mu.Unlock()
	r, err = (Source{}).RefreshTeam(t.Context(), cfg, tm)
	if err == nil || r.Failure != routing.Terminal || asked.Load() != 2 {
		t.Fatalf("403 PROJECT_NOT_FOUND: %+v, %v, asked=%d", r, err, asked.Load())
	}

	// Stored credential from another environment (even with matching account key) -> SignIn, zero HTTP calls.
	otherCred := &auth.Credential{
		AccessToken: "other-env-token",
		AuthURL:     "https://auth.elsewhere",
		SessionID:   "sess-o",
		UserID:      "alice",
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	if _, err := auth.SaveCredential(cfg.Dir, cfg.ProfileName, otherCred); err != nil {
		t.Fatal(err)
	}
	cfg.Account = otherCred.Account()
	r, err = (Source{}).RefreshTeam(t.Context(), cfg, tm)
	if err == nil || r.Failure != routing.SignIn || asked.Load() != 2 {
		t.Fatalf("wrong-environment credential: %+v, %v, asked=%d", r, err, asked.Load())
	}
}

// HTTP 404 with PROJECT_NOT_FOUND is classified as Transient (only 403 PROJECT_NOT_FOUND
// is Terminal) end to end through RefreshTeam.
func TestFetchTeam404ProjectNotFoundIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"NOT_FOUND","reason":"PROJECT_NOT_FOUND","message":"not found"}}`)
	}))
	defer srv.Close()

	cfg := setUp(t, "")
	cfg.AuthURL = srv.URL
	cred := &auth.Credential{
		AccessToken: "user-token-fixture",
		AuthURL:     srv.URL,
		SessionID:   "sess-404",
		UserID:      "alice",
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	if _, err := auth.SaveCredential(cfg.Dir, cfg.ProfileName, cred); err != nil {
		t.Fatal(err)
	}
	cfg.Account = cred.Account()
	if !routing.ValidAccount(cfg.Account) {
		t.Fatalf("routing.ValidAccount rejected auth.AccountKey %q", cfg.Account)
	}
	r, err := (Source{}).RefreshTeam(t.Context(), cfg, routing.Team{EnrollmentID: "e", OrganizationID: "org_b", AuthURL: srv.URL, TeamID: "team-b"})
	if err == nil || r.Failure != routing.Transient {
		t.Fatalf("404 PROJECT_NOT_FOUND: %+v, %v, want Transient", r, err)
	}
}

// A server-key profile fetches only its configured team using keys.json, omits project_id,
// and refuses another team or another auth host.
func TestFetchTeamWithServerKey(t *testing.T) {
	var mu sync.Mutex
	var bearer, queryProject string
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		mu.Lock()
		bearer = r.Header.Get("Authorization")
		queryProject = r.URL.Query().Get("project_id")
		mu.Unlock()
		fmt.Fprint(w, `{"policy":{"version":"1.1","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":false},"per_repository":{"repositories":["github.com/acme/api"]}}},"revision":1,"updated_at":"2026-11-13T12:00:00Z"}`)
	}))
	defer srv.Close()

	key := serverkey.Display + "team"
	cfg := setUp(t, "")
	cfg.AuthURL, cfg.ServerKeySignIn, cfg.ServerKeyAuthURL, cfg.Team = srv.URL, true, srv.URL, "team-a"
	if err := keystore.Set(cfg.Dir, "team-a", key, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}

	// Another team is refused before any HTTP request.
	if _, err := (Source{}).FetchTeam(t.Context(), cfg, routing.Team{EnrollmentID: "e2", OrganizationID: "org_a", AuthURL: srv.URL, TeamID: "team-b"}); err == nil || asked.Load() != 0 {
		t.Fatalf("other team on server-key profile: err=%v, asked=%d", err, asked.Load())
	}

	// Configured team succeeds with keys.json and no project_id query parameter.
	p, err := (Source{}).FetchTeam(t.Context(), cfg, routing.Team{EnrollmentID: "e1", OrganizationID: "org_a", AuthURL: srv.URL, TeamID: "team-a"})
	mu.Lock()
	gotBearer, gotQuery := bearer, queryProject
	mu.Unlock()
	if err != nil || gotBearer != "Bearer "+key || gotQuery != "" || p.TeamID != "team-a" || p.Wildcards {
		t.Fatalf("FetchTeam = %+v, %v, bearer=%q, query=%q", p, err, gotBearer, gotQuery)
	}
}

// Under the offline stub a team's policy is the stub's, stamped as the team's.
func TestFetchTeamUnderStub(t *testing.T) {
	cfg := setUp(t, `{"mode":"repo","repositories":["github.com/acme/api"],"include_prompts":true}`)
	p, err := (Source{}).FetchTeam(t.Context(), cfg, routing.Team{EnrollmentID: "e", OrganizationID: org, AuthURL: authURL, TeamID: "team-x"})
	if err != nil || p.TeamID != "team-x" || p.OrganizationID != org || p.AuthURL != authURL || p.FetchedAt.IsZero() {
		t.Fatalf("FetchTeam = %+v, %v", p, err)
	}
}
