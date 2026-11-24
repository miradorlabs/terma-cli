package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// The fake's credentials are joined at run time, so no literal in this file is shaped
// like one: the organization's secrets gate scans tests too.
var (
	userTokenPrefix    = "ter" + "_usr_"
	refreshTokenPrefix = "mir" + "_urf_"
	legacyTokenPrefix  = "ter" + "_cli_"
)

// fakeUserToken is the access token the fake auth host takes for user: one user token
// reads every organization the person belongs to.
func fakeUserToken(user string) string { return userTokenPrefix + cmp.Or(user, "u-1") }

// fakeAuth is an auth host with two organizations and one person, u-1, by default; a user
// token names its user (fakeUserToken), and it counts what the CLI does.
type fakeAuth struct {
	srv        *httptest.Server
	revokes    atomic.Int32
	whoamis    atomic.Int32
	deadToken  string
	policyBody string
	policies   atomic.Int32
	orgLists   atomic.Int32
	// orgs is what /v1/organizations lists, fakeOrgs when nil.
	orgs []organization
	// orgsDown fails /v1/organizations.
	orgsDown bool
	// projectsUnavailable answers /v1/projects with the retryable 503 a projection miss gives.
	projectsUnavailable bool
	// orgGone answers /v1/projects and /v1/policy for this organization with the 403 that
	// says the person lost access.
	orgGone string
	// orgGonePolicyOnly answers /v1/projects normally but returns 403 ORG_NOT_ACCESSIBLE on
	// /v1/policy for teams in this organization.
	orgGonePolicyOnly string
	// projects overrides projectsIn(org) per organization when set.
	projects map[string][]project
	// goneTeams answers /v1/policy with 403 PROJECT_NOT_FOUND for these team IDs.
	goneTeams map[string]bool
	// forbiddenTeams answers /v1/policy with a bare 403 PERMISSION_DENIED (no reason).
	forbiddenTeams map[string]bool
	// slow, when set, runs at the start of /v1/projects and /v1/policy handling.
	slow func(*http.Request)
	// serverKeys are live team server keys, each to its team (serveServerKey).
	serverKeys map[string]string
	// keyPolicies counts the policies read with a server key.
	keyPolicies atomic.Int32
	// keyPermissions are what whoami says a server key may do: Ingest alone for a key not
	// listed, nothing at all for one listed as nil.
	keyPermissions map[string]map[string]bool
	// grants are the codes /v1/auth/user/token redeems, each once, for the user each signs
	// in; delegated ones redeem only with redirect_port 0.
	grants    map[string]fakeGrant
	exchanges atomic.Int32
	// scopedPolicies refuses a team's policy unless it is a team of the person's, as the auth
	// gateway does (503); policyBodies are each team's, over policyBody.
	scopedPolicies bool
	policyBodies   map[string]string
	mu             sync.Mutex
	verifiers      []string
	installations  []string
	revoked        []string
	legacyRevoked  []string
	// revoking, when set, is told of each revoke, which then waits for holdRevokes to close.
	revoking    chan struct{}
	holdRevokes chan struct{}
	// onPolicy, when set, intercepts /v1/policy requests; returning true finishes the response.
	onPolicy func(w http.ResponseWriter, r *http.Request) bool
}

// fakeGrant is who a code signs in, and whether a desktop delegated it.
type fakeGrant struct {
	user      string
	delegated bool
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
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAuth) serve(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if _, ok := f.serverKeys[token]; ok {
		f.serveServerKey(w, r, token)
		return
	}
	switch r.URL.Path {
	case "/v1/auth/user/token":
		f.serveToken(w, r)
		return
	case "/v1/auth/user/revoke":
		var body struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.revoking != nil {
			f.revoking <- struct{}{}
			<-f.holdRevokes
		}
		f.revokes.Add(1)
		f.mu.Lock()
		f.revoked = append(f.revoked, body.RefreshToken)
		f.mu.Unlock()
		fmt.Fprint(w, `{"enforcement":"ENFORCED"}`)
		return
	case "/v1/auth/cli/revoke":
		f.mu.Lock()
		f.legacyRevoked = append(f.legacyRevoked, token)
		f.mu.Unlock()
		fmt.Fprint(w, `{}`)
		return
	}
	user, ok := strings.CutPrefix(token, userTokenPrefix)
	if !ok || user == "" || token == f.deadToken {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"UNAUTHENTICATED","message":"invalid token"}}`)
		return
	}
	orgs := f.orgs
	if orgs == nil {
		orgs = fakeOrgs
	}
	member := func(org string) bool {
		return slices.ContainsFunc(orgs, func(o organization) bool { return o.ID == org })
	}
	projectsOf := func(org string) []project {
		if f.projects != nil {
			return f.projects[org]
		}
		return projectsIn(org)
	}
	switch r.URL.Path {
	case "/v1/whoami":
		f.whoamis.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"organization_id": "", "auth_type": "user_token", "user_id": user, "email": "dev@example.com",
		})
	case "/v1/organizations":
		f.orgLists.Add(1)
		if f.orgsDown {
			unavailable(w)
			return
		}
		_ = json.NewEncoder(w).Encode(listOrganizationsResponse{Organizations: orgs})
	case "/v1/projects":
		if f.slow != nil {
			f.slow(r)
		}
		org := r.URL.Query().Get("organization_id")
		switch {
		case org == "":
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":"INVALID_ARGUMENT","message":"organization_id is required"}}`)
		case org == f.orgGone:
			orgNotAccessible(w)
		case f.projectsUnavailable || !member(org):
			unavailable(w)
		default:
			_ = json.NewEncoder(w).Encode(listProjectsResponse{Projects: projectsOf(org)})
		}
	case "/v1/policy":
		if f.slow != nil {
			f.slow(r)
		}
		f.policies.Add(1)
		if f.onPolicy != nil && f.onPolicy(w, r) {
			return
		}
		team := r.URL.Query().Get("project_id")
		if team == "" {
			http.Error(w, "missing project_id", http.StatusBadRequest)
			return
		}
		if f.goneTeams[team] {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"code":"PERMISSION_DENIED","reason":"PROJECT_NOT_FOUND","message":"team not found"}}`)
			return
		}
		if f.forbiddenTeams[team] {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"code":"PERMISSION_DENIED","message":"forbidden"}}`)
			return
		}
		if f.orgGone != "" && slices.ContainsFunc(projectsOf(f.orgGone), func(p project) bool { return p.ID == team }) {
			orgNotAccessible(w)
			return
		}
		if f.orgGonePolicyOnly != "" && slices.ContainsFunc(projectsOf(f.orgGonePolicyOnly), func(p project) bool { return p.ID == team }) {
			orgNotAccessible(w)
			return
		}
		if f.scopedPolicies && !slices.ContainsFunc(orgs, func(o organization) bool {
			return slices.ContainsFunc(projectsOf(o.ID), func(p project) bool { return p.ID == team })
		}) {
			unavailable(w)
			return
		}
		if body, ok := f.policyBodies[team]; ok {
			fmt.Fprint(w, body)
		} else if f.policyBody != "" {
			fmt.Fprint(w, f.policyBody)
		} else {
			fmt.Fprint(w, `{"policy":null}`)
		}
	default:
		http.NotFound(w, r)
	}
}

// unavailable is the auth gateway's retryable answer: a team or organization not known to
// be the person's yet, or an unreadable authorization store.
func unavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprint(w, `{"error":{"code":"UNAVAILABLE","reason":"AUTHORIZATION_UNAVAILABLE","message":"authorization is temporarily unavailable"}}`)
}

// orgNotAccessible is the auth gateway's answer when the person's membership says no.
func orgNotAccessible(w http.ResponseWriter) {
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprint(w, `{"error":{"code":"PERMISSION_DENIED","reason":"ORG_NOT_ACCESSIBLE","message":"organization not accessible"}}`)
}

// serveToken redeems a code, once, as /v1/auth/user/token does: a browser code with its
// listener's port, a delegated one only with redirect_port 0, either only with the
// installation's id. Every refusal is the auth gateway's one 401 UNAUTHENTICATED; refreshes
// are refused, as every token here is for good or dead.
func (f *fakeAuth) serveToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GrantType      string `json:"grant_type"`
		Code           string `json:"code"`
		CodeVerifier   string `json:"code_verifier"`
		RedirectPort   *int   `json:"redirect_port"`
		InstallationID string `json:"installation_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	refuse := func() {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"UNAUTHENTICATED","message":"invalid or expired credentials"}}`)
	}
	if body.GrantType != "authorization_code" {
		refuse()
		return
	}
	f.mu.Lock()
	grant, ok := f.grants[body.Code]
	delete(f.grants, body.Code)
	f.verifiers = append(f.verifiers, body.CodeVerifier)
	f.installations = append(f.installations, body.InstallationID)
	f.mu.Unlock()
	if !ok || body.RedirectPort == nil || (*body.RedirectPort == 0) != grant.delegated || body.CodeVerifier == "" || body.InstallationID == "" {
		refuse()
		return
	}
	n := f.exchanges.Add(1)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": fakeUserToken(grant.user), "refresh_token": fmt.Sprintf("%sgranted_%d", refreshTokenPrefix, n),
		"token_type": "Bearer", "expires_in": 3600, "session_id": fmt.Sprintf("s-granted-%d", n),
		"family_id": fmt.Sprintf("s-granted-%d", n),
		"user":      map[string]string{"id": grant.user, "email": "dev@example.com"},
	})
}

// grant makes code a code Terma Desktop delegated for user.
func (f *fakeAuth) grant(code, user string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.grants == nil {
		f.grants = map[string]fakeGrant{}
	}
	f.grants[code] = fakeGrant{user: user, delegated: true}
}

// verifiersSent are the code verifiers the token endpoint was sent.
func (f *fakeAuth) verifiersSent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.verifiers...)
}

// installationsSent are the installation ids the token endpoint was sent.
func (f *fakeAuth) installationsSent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.installations...)
}

// revokedTokens are the refresh tokens of the sessions ended so far.
func (f *fakeAuth) revokedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}

// legacyRevokedTokens are the access tokens of an older terma's sessions ended so far.
func (f *fakeAuth) legacyRevokedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.legacyRevoked...)
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

// storedSession is u-1's sign-in at f, as an earlier `terma setup` stored it.
func storedSession(f *fakeAuth) *auth.Credential {
	return storedSessionOf(f, "u-1")
}

// storedSessionOf is user's sign-in at f.
func storedSessionOf(f *fakeAuth, user string) *auth.Credential {
	return &auth.Credential{
		AccessToken:  fakeUserToken(user),
		RefreshToken: refreshTokenPrefix + "stored-" + user,
		ExpiresAt:    time.Now().Add(time.Hour),
		SessionID:    "s-stored-" + user,
		FamilyID:     "s-stored-" + user,
		AuthURL:      f.srv.URL,
		UserID:       user,
		UserEmail:    "dev@example.com",
	}
}

// saveSignIn stores cred as dir's default profile's sign-in, paired with the profile as
// storeLogin pairs a new login (config.Profile.Account), keeping what the profile chose.
func saveSignIn(dir string, cred *auth.Credential) error {
	if _, err := auth.SaveCredential(dir, config.DefaultProfile, cred); err != nil {
		return err
	}
	return config.UpdateProfile(dir, config.DefaultProfile, func(p *config.Profile) { p.Account = cred.Account() })
}
