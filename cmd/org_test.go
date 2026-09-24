package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// fakeAuth is an auth host that knows two organizations and one user. A bearer token
// names the session it belongs to: `ter_cli_<org>` is live for that organization,
// anything else is a stranger. It counts what the CLI does so a test can say "no
// browser, no new session" with evidence.
type fakeAuth struct {
	srv       *httptest.Server
	revokes   atomic.Int32
	whoamis   atomic.Int32
	keysMint  atomic.Int32
	deadToken string
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
			_ = json.NewEncoder(w).Encode(listOrganizationsResponse{Organizations: fakeOrgs})
		case "/v1/projects":
			_ = json.NewEncoder(w).Encode(listProjectsResponse{Projects: projectsIn(org)})
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

// Each organization has projects of its own; Acme has two so a picker would be
// needed, Beta Labs one so it is selected outright.
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

// authSandbox points the CLI at the fake host with a scratch config dir.
func authSandbox(t *testing.T, f *fakeAuth) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_API_URL", f.srv.URL)
	t.Setenv("TERMA_AUTH_URL", f.srv.URL)
	t.Setenv("TERMA_APP_URL", f.srv.URL)
	t.Setenv("TERMA_OTLP_URL", f.srv.URL)
	t.Setenv("TERMA_API_KEY", "")
	t.Setenv("TERMA_ENV", "")
	t.Setenv("TERMA_PROFILE", "")
	t.Chdir(t.TempDir())
	flags = globalFlags{}
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

// The complaint this fixes: `terma login` minted a fresh session every time, leaving
// the old ones live. A working stored session is verified and reused; nothing opens.
func TestLoginReusesAWorkingSession(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}

	out, err := within(10*time.Second).combined(t, "login", "--no-browser")
	if err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	if !strings.Contains(out, "existing session reused") || strings.Contains(out, "Open this URL") {
		t.Fatalf("login should reuse the stored session without a browser:\n%s", out)
	}
	if f.whoamis.Load() != 1 {
		t.Fatalf("the session should be verified exactly once, got %d whoami calls", f.whoamis.Load())
	}
	file, _ := config.LoadFile()
	if p := file.Profiles[config.DefaultProfile]; p == nil || p.OrganizationID != orgA().ID {
		t.Fatalf("profile should record the organization: %+v", file.Profiles)
	}
}

// --force is the way to get a new session on purpose. Off a terminal and without a
// browser, that is a wait at the loopback listener — which the deadline ends — but
// the point here is that the stored session was not reused.
func TestLoginForceSkipsReuse(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}
	out, err := within(2*time.Second).combined(t, "login", "--no-browser", "--force")
	if err == nil {
		t.Fatalf("expected the browser handoff to be cut short:\n%s", out)
	}
	if !strings.Contains(out, "Open this URL") || f.whoamis.Load() != 0 {
		t.Fatalf("--force should go straight to the browser:\n%s", out)
	}
}

func TestOrgUseSwitchesAccountsWithoutSelectingProjects(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	for _, o := range fakeOrgs {
		if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, o)); err != nil {
			t.Fatal(err)
		}
	}
	// Working in Acme on Acme API.
	if _, err := auth.UseOrganization(config.DefaultProfile, orgA().ID); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(config.DefaultProfile, func(p *config.Profile) {
		p.SelectOrganization(orgA().ID, "Acme")
	}); err != nil {
		t.Fatal(err)
	}

	out, err := within(10*time.Second).combined(t, "org", "use", "beta")
	if err != nil {
		t.Fatalf("org use: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Now using Beta Labs") || !strings.Contains(out, "existing session reused") {
		t.Fatalf("switch should reuse the stored Beta Labs session:\n%s", out)
	}
	// Even an organization with one project must not select it globally.
	if strings.Contains(out, "Project:") {
		t.Fatalf("org use should not select a project:\n%s", out)
	}
	if strings.Contains(out, "Open this URL") {
		t.Fatalf("no browser should open for a stored session:\n%s", out)
	}
	active, err := auth.LoadCredential(config.DefaultProfile)
	if err != nil || active.OrganizationID != orgB().ID {
		t.Fatalf("active credential should be Beta Labs: %+v, %v", active, err)
	}

	// Back to Acme: no old project is restored.
	out, err = within(10*time.Second).combined(t, "org", "use", orgA().ID)
	if err != nil {
		t.Fatalf("org use back: %v\n%s", err, out)
	}
	if strings.Contains(out, "Project:") || strings.Contains(out, "restored") {
		t.Fatalf("switching back should not restore a project:\n%s", out)
	}
	file, _ := config.LoadFile()
	p := file.Profiles[config.DefaultProfile]
	if p.OrganizationID != orgA().ID {
		t.Fatalf("profile after switching back: %+v", p)
	}

	// Already there: says so, changes nothing.
	out, err = within(10*time.Second).combined(t, "org", "use", "Acme")
	if err != nil || !strings.Contains(out, "Already using Acme") {
		t.Fatalf("re-selecting the current organization: %v\n%s", err, out)
	}
}

// A stored session the server no longer honours is dropped, not retried forever, and
// the switch falls through to the browser for that organization. The active session
// for the other organization is untouched.
func TestOrgUseDropsADeadSession(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	for _, o := range fakeOrgs {
		if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, o)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := auth.UseOrganization(config.DefaultProfile, orgA().ID); err != nil {
		t.Fatal(err)
	}
	f.deadToken = "ter_cli_" + orgB().ID

	out, err := within(2*time.Second).combined(t, "org", "use", "Beta Labs", "--no-browser")
	if err == nil {
		t.Fatalf("a dead session should fall through to the browser, which the deadline cuts short:\n%s", out)
	}
	if !strings.Contains(out, "Open this URL") {
		t.Fatalf("the browser handoff should have started:\n%s", out)
	}
	if _, err := auth.LoadCredentialFor(config.DefaultProfile, orgB().ID); err != auth.ErrNotLoggedIn {
		t.Fatalf("the dead Beta Labs session should be forgotten, got %v", err)
	}
	if active, _ := auth.LoadCredential(config.DefaultProfile); active == nil || active.OrganizationID != orgA().ID {
		t.Fatalf("Acme should still be active: %+v", active)
	}
}

func TestLoginWithOrgResolvesTheNameThroughAStoredSession(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	for _, o := range fakeOrgs {
		if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, o)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := auth.UseOrganization(config.DefaultProfile, orgA().ID); err != nil {
		t.Fatal(err)
	}
	out, err := within(10*time.Second).combined(t, "login", "--org", "beta labs")
	if err != nil {
		t.Fatalf("login --org: %v\n%s", err, out)
	}
	if !strings.Contains(out, "in Beta Labs") || !strings.Contains(out, "existing session reused") {
		t.Fatalf("login --org should switch to the stored Beta Labs session:\n%s", out)
	}
}

func TestLogoutRevokesEveryStoredSession(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	for _, o := range fakeOrgs {
		if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, o)); err != nil {
			t.Fatal(err)
		}
	}
	out, err := within(10*time.Second).combined(t, "logout")
	if err != nil {
		t.Fatalf("logout: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Logged out of 2 organizations") {
		t.Fatalf("logout should say how many sessions it ended:\n%s", out)
	}
	if got := f.revokes.Load(); got != 2 {
		t.Fatalf("every stored session must be revoked server-side, got %d revokes", got)
	}
	if creds, _ := auth.Credentials(config.DefaultProfile); len(creds) != 0 {
		t.Fatalf("credentials should be gone: %+v", creds)
	}
}

func TestOrgListMarksSignedInOrganizations(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}
	out, err := within(10*time.Second).combined(t, "org", "list", "-o", "table")
	if err != nil {
		t.Fatalf("org list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "SIGNED IN") {
		t.Fatalf("the table should have a signed-in column:\n%s", out)
	}
	var acme, beta string
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.Contains(line, "Acme"):
			acme = line
		case strings.Contains(line, "Beta Labs"):
			beta = line
		}
	}
	if !strings.Contains(acme, "yes") || strings.Contains(beta, "yes") {
		t.Fatalf("only Acme has a session:\n%s", out)
	}
}
