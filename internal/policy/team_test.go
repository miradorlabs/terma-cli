package policy

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// migrate marks cfg's machine migrated, as the new relay leaves it (config.Migrated).
func migrate(t *testing.T, cfg *config.Config) {
	t.Helper()
	if err := os.WriteFile(config.CredentialsV2Path(cfg.Dir), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Every refusal is classified by status, reason and context, never by message (C3).
func TestClassifyByStatusReasonAndContext(t *testing.T) {
	t.Parallel()
	refusal := func(status int, reason string) *api.Error {
		return &api.Error{StatusCode: status, Reason: reason, Message: "ORG_NOT_ACCESSIBLE TEAM_NOT_FOUND"}
	}
	for _, tc := range []struct {
		name string
		err  error
		want routing.ErrorClass
	}{
		{"refresh refused, organization not accessible", &api.RefreshError{Err: refusal(401, api.ReasonOrgNotAccessible)}, routing.OrgAccessLost},
		{"wrapped as a fetch", fmt.Errorf("fetch: %w", &api.RefreshError{Err: refusal(401, api.ReasonOrgNotAccessible)}), routing.OrgAccessLost},
		{"policy read, team not found", fmt.Errorf("fetch: %w", refusal(403, api.ReasonTeamNotFound)), routing.Terminal},
		{"team not found on a 404", refusal(404, api.ReasonTeamNotFound), routing.Transient},
		{"organization not accessible outside a refresh", refusal(401, api.ReasonOrgNotAccessible), routing.SignIn},
		{"organization not accessible on a 403", refusal(403, api.ReasonOrgNotAccessible), routing.Transient},
		{"refresh refused without a reason", &api.RefreshError{Err: refusal(401, "")}, routing.SignIn},
		{"any other 401", refusal(401, ""), routing.SignIn},
		{"no sign-in for the organization", auth.ErrNotLoggedIn, routing.SignIn},
		{"a sign-in of another environment", &auth.ErrWrongEnvironment{IssuedBy: "a", ConfiguredAs: "b"}, routing.SignIn},
		{"a 403 without a reason", refusal(403, ""), routing.Transient},
		{"a 5xx", refusal(503, ""), routing.Transient},
		{"a 429", refusal(429, ""), routing.Transient},
		{"a refresh that failed on a 5xx", &api.RefreshError{Err: refusal(502, "")}, routing.Transient},
		{"the network", errors.New("dial tcp: connection refused"), routing.Transient},
		{"a team of another environment", ErrOtherEnvironment, routing.Transient},
	} {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("%s: Classify = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A team is fetched with its own organization's sign-in, never the profile's active one,
// reads 2.x with wildcards, and is stamped as that team's; config.json stays as it was.
func TestFetchTeamUsesItsOrganizationsSignIn(t *testing.T) {
	var bearer, named string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer, named = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), r.URL.Query().Get("project_id")
		fmt.Fprint(w, `{"policy":{"version":"2.0","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":true},"per_repository":{"repositories":["github.com/acme/*"]}}},"revision":7,"updated_at":"2026-11-13T12:00:00Z"}`)
	}))
	defer srv.Close()
	cfg := setUp(t, "")
	cfg.AuthURL = srv.URL
	migrate(t, cfg)
	for _, o := range []string{"org_active", "org_b"} {
		if _, err := auth.SaveCredential(cfg.Dir, cfg.ProfileName, &auth.Credential{AccessToken: "ter_cli_" + o, AuthURL: srv.URL, OrganizationID: o, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	settings, _ := os.ReadFile(config.Path(cfg.Dir))
	teamB := routing.Team{EnrollmentID: "e-b", OrganizationID: "org_b", AuthURL: srv.URL, TeamID: "team-b", Epoch: 3}
	r, err := (Source{}).RefreshTeam(t.Context(), cfg, teamB)
	if err != nil || r.Policy == nil || r.Epoch != 3 || r.EnrollmentID != "e-b" || r.At.IsZero() {
		t.Fatalf("RefreshTeam = %+v, %v", r, err)
	}
	if bearer != "ter_cli_org_b" || named != "team-b" {
		t.Fatalf("asked with %q for %q, want org_b's sign-in for team-b", bearer, named)
	}
	p := *r.Policy
	if !p.Wildcards || p.TeamID != "team-b" || p.OrganizationID != "org_b" || p.AuthURL != srv.URL || p.Revision != 7 || !p.IncludePrompts || p.IncludeToolContent {
		t.Fatalf("policy = %+v", p)
	}
	if stored, ok, err := config.ReadPolicy(cfg.StateDir, "team-b"); err != nil || !ok || stored.Revision != 7 {
		t.Fatalf("stored %+v, %v, %v", stored, ok, err)
	}
	if after, _ := os.ReadFile(config.Path(cfg.Dir)); !bytes.Equal(after, settings) {
		t.Fatalf("a team refresh rewrote config.json:\n%s", after)
	}
}

// An organization this machine has no sign-in for needs one; a team of another environment
// than the profile's is neither fetched nor taken for a refusal.
func TestFetchTeamFailsClosed(t *testing.T) {
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		fmt.Fprint(w, `{"policy":null}`)
	}))
	defer srv.Close()
	cfg := setUp(t, "")
	cfg.AuthURL = srv.URL
	migrate(t, cfg)
	if _, err := auth.SaveCredential(cfg.Dir, cfg.ProfileName, &auth.Credential{AccessToken: "ter_cli_a", AuthURL: srv.URL, OrganizationID: "org_a", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	r, err := (Source{}).RefreshTeam(t.Context(), cfg, routing.Team{EnrollmentID: "e", OrganizationID: "org_b", AuthURL: srv.URL, TeamID: "team-b"})
	if err == nil || r.Failure != routing.SignIn || r.Policy != nil {
		t.Fatalf("no sign-in for org_b: %+v, %v", r, err)
	}
	r, err = (Source{}).RefreshTeam(t.Context(), cfg, routing.Team{EnrollmentID: "e", OrganizationID: "org_a", AuthURL: "https://auth.elsewhere", TeamID: "team-a"})
	if !errors.Is(err, ErrOtherEnvironment) || r.Failure != routing.Transient {
		t.Fatalf("another environment's team: %+v, %v", r, err)
	}
	if asked != 0 {
		t.Fatalf("asked %d times", asked)
	}
}

// A profile set up with a server key reads its one team with that team's v2 key.
func TestFetchTeamWithAServerKey(t *testing.T) {
	var bearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"policy":{"version":"1.1","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":false},"per_repository":{"repositories":["github.com/acme/api"]}}},"revision":1,"updated_at":"2026-11-13T12:00:00Z"}`)
	}))
	defer srv.Close()
	cfg := setUp(t, "")
	cfg.AuthURL, cfg.ServerKeySignIn, cfg.ServerKeyAuthURL = srv.URL, true, srv.URL
	migrate(t, cfg)
	if err := keystore.SetTeam(cfg.Dir, "e1", "team-a", "ter_srv_teamkey", keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	p, err := (Source{}).FetchTeam(t.Context(), cfg, routing.Team{EnrollmentID: "e1", OrganizationID: "org_a", AuthURL: srv.URL, TeamID: "team-a"})
	if err != nil || bearer != "Bearer ter_srv_teamkey" || p.TeamID != "team-a" || p.Wildcards {
		t.Fatalf("FetchTeam = %+v, %v, sent %q", p, err, bearer)
	}
}

// Under the offline stub a team's policy is the stub's, stamped as the team's.
func TestFetchTeamUnderTheStub(t *testing.T) {
	cfg := setUp(t, `{"mode":"repo","repositories":["github.com/acme/api"],"include_prompts":true}`)
	p, err := (Source{}).FetchTeam(t.Context(), cfg, routing.Team{EnrollmentID: "e", OrganizationID: org, AuthURL: authURL, TeamID: "team-x"})
	if err != nil || p.TeamID != "team-x" || p.OrganizationID != org || p.AuthURL != authURL || p.FetchedAt.IsZero() {
		t.Fatalf("FetchTeam = %+v, %v", p, err)
	}
}
