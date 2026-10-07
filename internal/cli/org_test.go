package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/spf13/cobra"
)

// fakeAuth is an auth host with two organizations and one user; `ter_cli_<org>` is a live
// token for that organization, and it counts what the CLI does.
type fakeAuth struct {
	srv        *httptest.Server
	revokes    atomic.Int32
	whoamis    atomic.Int32
	keysMint   atomic.Int32
	deadToken  string
	policyBody string
	policies   atomic.Int32
	orgLists   atomic.Int32
	// orgs is what /v1/organizations lists, fakeOrgs when nil.
	orgs []organization
}

var fakeOrgs = []organization{
	{ID: "11111111-1111-4111-8111-111111111111", Name: "Acme", Role: "admin"},
	{ID: "22222222-2222-4222-8222-222222222222", Name: "Beta Labs", Role: "member"},
}

func orgA() organization { return fakeOrgs[0] }
func orgB() organization { return fakeOrgs[1] }

func newFakeAuth(t *testing.T) *fakeAuth {
	t.Helper()
	f := &fakeAuth{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		org := ""
		for _, o := range fakeOrgs {
			if token == "ter_cli_"+o.ID {
				org = o.ID
			}
		}
		if org == "" || token == f.deadToken {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"code":"UNAUTHENTICATED","message":"invalid token"}`)
			return
		}
		switch r.URL.Path {
		case "/v1/whoami":
			f.whoamis.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"organization_id": org, "auth_type": "cli_token", "user_id": "u-1", "email": "dev@example.com",
			})
		case "/v1/organizations":
			f.orgLists.Add(1)
			orgs := f.orgs
			if orgs == nil {
				orgs = fakeOrgs
			}
			_ = json.NewEncoder(w).Encode(listOrganizationsResponse{Organizations: orgs})
		case "/v1/projects":
			_ = json.NewEncoder(w).Encode(listProjectsResponse{Projects: projectsIn(org)})
		case "/v1/policy":
			f.policies.Add(1)
			if r.URL.Query().Get("project_id") == "" {
				http.Error(w, "missing project_id", http.StatusBadRequest)
				return
			}
			if f.policyBody != "" {
				fmt.Fprint(w, f.policyBody)
			} else {
				fmt.Fprint(w, `{"policy":null}`)
			}
		case "/v1/auth/cli/revoke":
			f.revokes.Add(1)
			fmt.Fprint(w, `{}`)
		case "/v1/api-keys/server":
			f.keysMint.Add(1)
			var body createServerKeyRequestShape
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"key":        "ter_srv_" + strings.ReplaceAll(body.ProjectID, "-", "")[:24],
				"server_key": map[string]any{"id": "k-1", "project_id": body.ProjectID, "name": body.Name, "key_prefix": "ter_srv_"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type createServerKeyRequestShape struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
}

// Acme has two projects, so a picker would be needed; Beta Labs one.
func projectsIn(org string) []project {
	switch org {
	case orgA().ID:
		return []project{
			{ID: "aaaaaaaa-0000-4000-8000-000000000001", Name: "Acme Web", OrganizationID: org},
			{ID: "aaaaaaaa-0000-4000-8000-000000000002", Name: "Acme API", OrganizationID: org},
		}
	case orgB().ID:
		return []project{{ID: "bbbbbbbb-0000-4000-8000-000000000001", Name: "Beta Core", OrganizationID: org}}
	}
	return nil
}

func authSandbox(t *testing.T, f *fakeAuth) {
	t.Helper()
	useConfigDir(t, t.TempDir())
	t.Setenv("TERMA_API_URL", f.srv.URL)
	t.Setenv("TERMA_AUTH_URL", f.srv.URL)
	t.Setenv("TERMA_APP_URL", f.srv.URL)
	t.Setenv("TERMA_OTLP_URL", f.srv.URL)
	t.Setenv("TERMA_API_KEY", "")
	t.Setenv("TERMA_ENV", "")
	t.Setenv("TERMA_PROFILE", "")
	t.Chdir(t.TempDir())
}

func storedSession(f *fakeAuth, org organization) *auth.Credential {
	return &auth.Credential{
		AccessToken:    "ter_cli_" + org.ID,
		RefreshToken:   "ter_clr_" + org.ID,
		ExpiresAt:      time.Now().Add(time.Hour),
		SessionID:      "s-" + org.ID[:8],
		AuthURL:        f.srv.URL,
		OrganizationID: org.ID,
		UserEmail:      "dev@example.com",
	}
}

func TestParseOrgRef(t *testing.T) {
	if ref := parseOrgRef(orgA().ID); ref.ID != orgA().ID || ref.Name != "" {
		t.Fatalf("a UUID is an id: %+v", ref)
	}
	if ref := parseOrgRef("  Acme "); ref.Name != "Acme" || ref.ID != "" {
		t.Fatalf("anything else is a name: %+v", ref)
	}
	if !parseOrgRef("acme").matches(orgA().ID, "Acme") || parseOrgRef("acme").matches(orgB().ID, "Beta Labs") {
		t.Fatal("names match case-insensitively and nothing else")
	}
	if !parseOrgRef("").empty() {
		t.Fatal("blank is empty")
	}
}

func TestMatchOrganization(t *testing.T) {
	if o, err := matchOrganization(fakeOrgs, orgB().ID); err != nil || o.Name != "Beta Labs" {
		t.Fatalf("by id: %+v, %v", o, err)
	}
	if o, err := matchOrganization(fakeOrgs, "beta"); err != nil || o.ID != orgB().ID {
		t.Fatalf("by unique prefix: %+v, %v", o, err)
	}
	if _, err := matchOrganization(fakeOrgs, "zeta"); err == nil || !strings.Contains(err.Error(), "no organization matches") {
		t.Fatalf("unknown: %v", err)
	}
	ambiguous := append([]organization{{ID: "3", Name: "Acme Two"}}, fakeOrgs...)
	if o, err := matchOrganization(ambiguous, "acme"); err != nil || o.Name != "Acme" {
		t.Fatalf("an exact name wins over a longer one that shares the prefix: %+v, %v", o, err)
	}
	if _, err := matchOrganization(ambiguous, "ac"); err == nil || !strings.Contains(err.Error(), "several") {
		t.Fatalf("an ambiguous prefix must not guess: %v", err)
	}
}

// signInHere runs the sign-in `terma setup` starts with, under a deadline that cuts a
// browser handoff short; it returns what it printed.
func signInHere(t *testing.T, opts signInOptions, wait time.Duration) (*signInResult, string, error) {
	t.Helper()
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	res, err := testApp.signIn(cmd, cfg, opts)
	return res, out.String(), err
}

// A working stored session is verified and reused; no browser opens.
func TestSignInReusesAWorkingSession(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}

	res, out, err := signInHere(t, signInOptions{noBrowser: true}, 10*time.Second)
	if err != nil {
		t.Fatalf("sign-in: %v\n%s", err, out)
	}
	if !res.reused || strings.Contains(out, "Open this URL") {
		t.Fatalf("sign-in should reuse the stored session without a browser:\n%s", out)
	}
	if f.whoamis.Load() != 1 {
		t.Fatalf("the session should be verified exactly once, got %d whoami calls", f.whoamis.Load())
	}
	file, _ := config.LoadFile(testApp.dir)
	if p := file.Profiles[config.DefaultProfile]; p == nil || p.OrganizationID != orgA().ID {
		t.Fatalf("profile should record the organization: %+v", file.Profiles)
	}
}

// `terma setup --org` switches between stored sessions by name or id without a browser,
// and selects no team.
func TestSignInWithOrgSwitchesBetweenStoredSessions(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	for _, o := range fakeOrgs {
		if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f, o)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
		p.SelectOrganization(orgA().ID, "Acme")
	}); err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct {
		ref  string
		want organization
	}{{"beta labs", orgB()}, {"beta", orgB()}, {orgA().ID, orgA()}} {
		res, out, err := signInHere(t, signInOptions{org: parseOrgRef(step.ref), noBrowser: true}, 10*time.Second)
		if err != nil {
			t.Fatalf("--org %s: %v\n%s", step.ref, err, out)
		}
		if !res.reused || strings.Contains(out, "Open this URL") {
			t.Fatalf("--org %s should reuse the stored session:\n%s", step.ref, out)
		}
		active, err := auth.LoadCredential(testApp.dir, config.DefaultProfile)
		if err != nil || active.OrganizationID != step.want.ID {
			t.Fatalf("--org %s: active credential %+v, %v; want %s", step.ref, active, err, step.want.Name)
		}
		file, _ := config.LoadFile(testApp.dir)
		if p := file.Profiles[config.DefaultProfile]; p.OrganizationID != step.want.ID {
			t.Fatalf("--org %s: profile %+v", step.ref, p)
		}
	}
}

// A session the server no longer honours is dropped and the switch falls through to the
// browser; the other organization's session is untouched.
func TestSignInDropsADeadSession(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	for _, o := range fakeOrgs {
		if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f, o)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}
	f.deadToken = "ter_cli_" + orgB().ID

	_, out, err := signInHere(t, signInOptions{org: parseOrgRef("Beta Labs"), noBrowser: true}, 2*time.Second)
	if err == nil {
		t.Fatalf("a dead session should fall through to the browser, which the deadline cuts short:\n%s", out)
	}
	if !strings.Contains(out, "Open this URL") {
		t.Fatalf("the browser handoff should have started:\n%s", out)
	}
	if _, err := auth.LoadCredentialFor(testApp.dir, config.DefaultProfile, orgB().ID); err != auth.ErrNotLoggedIn {
		t.Fatalf("the dead Beta Labs session should be forgotten, got %v", err)
	}
	if active, _ := auth.LoadCredential(testApp.dir, config.DefaultProfile); active == nil || active.OrganizationID != orgA().ID {
		t.Fatalf("Acme should still be active: %+v", active)
	}
}

// `terma teardown --sign-out` revokes every stored session server-side.
func TestSignOutRevokesEveryStoredSession(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	for _, o := range fakeOrgs {
		if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f, o)); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := testApp.signOut(cmd); err != nil {
		t.Fatalf("sign out: %v\n%s", err, &out)
	}
	if !strings.Contains(out.String(), "Signed out of 2 organizations") {
		t.Fatalf("sign-out should say how many sessions it ended:\n%s", &out)
	}
	if got := f.revokes.Load(); got != 2 {
		t.Fatalf("every stored session must be revoked server-side, got %d revokes", got)
	}
	if creds, _ := auth.Credentials(testApp.dir, config.DefaultProfile); len(creds) != 0 {
		t.Fatalf("credentials should be gone: %+v", creds)
	}
}
