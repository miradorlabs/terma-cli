package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Account stands in for Terma's account service and API gateway where `terma setup`
// and the relay need them: a developer signed in with a user session (/v1/whoami, which
// names no organization), the organizations and teams it reads, and server keys minted
// per project (/v1/api-keys/server), each recorded so a scenario can tell which key
// the relay sent a project's records with.
type Account struct {
	srv       *httptest.Server
	token     string
	mints     atomic.Int32
	denyMints atomic.Bool
	// withholdContent is the team's collection policy: no prompts, no tool content.
	withholdContent atomic.Bool
	mu              sync.Mutex
	keys            map[string]string // project → minted key
	// desktopRefresh is Terma Desktop's refresh token, which /v1/auth/user/delegate rotates;
	// delegated are its unredeemed codes, each bound to the CLI's challenge, and delegations
	// counts its calls.
	desktopRefresh string
	delegated      map[string]delegatedCode
	delegations    atomic.Int32
	// issued are the refresh tokens of the CLI sessions delegated codes signed in, and
	// revoked those ended at /v1/auth/user/revoke.
	issued  map[string]bool
	revoked []string
	// refuseDelegation, when set, is the error.reason delegate refuses the desktop's
	// session with: NOT_DELEGABLE.
	refuseDelegation string
}

// delegatedCode is a code the delegate endpoint made for a CLI's challenge and label, the
// n-th delegation.
type delegatedCode struct {
	challenge, label string
	n                int32
}

// accountOrg is the organization the fake account signs the developer in to, and
// accountUser the developer it signs in.
const (
	accountOrg  = "11111111-1111-4111-8111-111111111111"
	accountUser = "22222222-2222-4222-8222-222222222222"
	// desktopSession is the Terma Desktop session that delegates, the family of every
	// session it delegates.
	desktopSession = "33333333-3333-4333-8333-333333333333"
)

// The shapes of the auth host's user-session secrets: an access token, a refresh token and
// a delegated code. Joined at run time: the org's secrets scanner reads test files.
var (
	userAccessPrefix  = "ter" + "_usr_"
	userRefreshPrefix = "mir" + "_urf_"
	userCodePrefix    = "mir" + "_ucd_"
)

// secretLike is a fake secret of a shape: prefix and n as 32 hex digits.
func secretLike(prefix string, n int32) string { return fmt.Sprintf("%s%032x", prefix, n) }

// StartAccount serves the fake account service until the test ends.
func (sb *Sandbox) StartAccount() *Account {
	if sb.account != nil {
		return sb.account
	}
	a := &Account{token: secretLike(userAccessPrefix, 1), keys: map[string]string{}, desktopRefresh: secretLike(userRefreshPrefix, 0xde5c0),
		delegated: map[string]delegatedCode{}, issued: map[string]bool{}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The sign-in endpoints take no bearer: the body is the credential.
		switch r.URL.Path {
		case "/v1/auth/user/delegate":
			a.serveDelegate(w, r)
			return
		case "/v1/auth/user/token":
			if a.serveDelegatedExchange(w, r) {
				return
			}
		case "/v1/auth/user/revoke":
			a.serveRevoke(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+a.token {
			writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "", "invalid token")
			return
		}
		switch r.URL.Path {
		case "/v1/whoami":
			// A user session belongs to no organization.
			_ = json.NewEncoder(w).Encode(map[string]any{"organization_id": "", "auth_type": "user_token", "user_id": accountUser, "email": "live@terma.test"})
		case "/v1/organizations":
			_ = json.NewEncoder(w).Encode(map[string]any{"organizations": []map[string]any{{"id": accountOrg, "name": "Live", "role": "admin", "type": "personal"}}})
		case "/v1/projects":
			// A user token reads every organization's teams, so it must name one.
			if r.URL.Query().Get("organization_id") != accountOrg {
				writeAuthError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "", "organization_id is required with a user token")
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"projects": []map[string]any{
				{"id": sb.ProjectID, "name": "Live", "organization_id": accountOrg},
				{"id": "proj_other", "name": "Other", "organization_id": accountOrg},
			}})
		case "/v1/policy":
			if r.URL.Query().Get("project_id") == "" {
				http.Error(w, "missing policy project_id", http.StatusBadRequest)
				return
			}
			withhold := a.withholdContent.Load()
			repos, _ := json.Marshal(append([]string{}, sb.Repositories...))
			// git_hooks on: the scenarios commit by hand and expect the trailer, which only
			// the hooks terma installs in the repository can add.
			fmt.Fprintf(w, `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":%s},"git_hooks":true,"capture":{"exclude_prompts":%t,"exclude_tool_content":%t}}},"revision":1,"updated_at":"2026-09-30T12:27:05Z"}`, repos, withhold, withhold)
		case "/v1/api-keys/server":
			if a.denyMints.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			var body struct {
				ProjectID string `json:"project_id"`
				Name      string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			n := a.mints.Add(1)
			key := fmt.Sprintf("ter_srv_minted%04d%s", n, strings.Repeat("0", 20))
			a.mu.Lock()
			a.keys[body.ProjectID] = key
			a.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"key": key, "server_key": map[string]any{"id": fmt.Sprint(n), "project_id": body.ProjectID, "name": body.Name, "key_prefix": "ter_srv_"}})
		default:
			http.NotFound(w, r)
		}
	}))
	sb.T.Cleanup(a.srv.Close)
	sb.ExtraEnv = append(sb.ExtraEnv, "TERMA_AUTH_URL="+a.srv.URL, "TERMA_API_URL="+a.srv.URL, "TERMA_APP_URL="+a.srv.URL)
	sb.account = a
	sb.SignIn("default")
	return a
}

// SignIn signs the developer in to the fake account under profile: the user session
// `terma setup` would have stored in user-credentials.json (tokens in the file, as
// --insecure-storage keeps them) and the organization setup chose, in config.json, with
// the account's hosts, so launchd/systemd relays (which do not inherit ExtraEnv) load the
// same scoped policy and login.
func (sb *Sandbox) SignIn(profile string) {
	a := sb.account
	credsPath := filepath.Join(sb.TermaConfig, "user-credentials.json")
	creds := map[string]any{}
	if data, err := os.ReadFile(credsPath); err == nil {
		_ = json.Unmarshal(data, &creds)
	}
	creds[profile] = map[string]any{
		"access_token": a.token, "refresh_token": secretLike(userRefreshPrefix, 1), "expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		"session_id": "seeded-" + profile, "family_id": "seeded-" + profile,
		"auth_url": a.srv.URL, "user_email": "live@terma.test", "user_id": accountUser,
	}
	data, _ := json.MarshalIndent(creds, "", "  ")
	sb.writeAbs(credsPath, string(data)+"\n")
	path := filepath.Join(sb.TermaConfig, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		sb.T.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(data, &file); err != nil {
		sb.T.Fatal(err)
	}
	profiles := file["profiles"].(map[string]any)
	p, _ := profiles[profile].(map[string]any)
	if p == nil {
		p = map[string]any{"otlp_url": profiles["default"].(map[string]any)["otlp_url"]}
		profiles[profile] = p
	}
	p["organization_id"] = accountOrg
	p["auth_url"], p["api_url"], p["app_url"] = a.srv.URL, a.srv.URL, a.srv.URL
	data, _ = json.Marshal(file)
	sb.writeAbs(path, string(data))
}

// The auth gateway's answers, as mirador-platform's gateways/auth/spec/openapi.yaml has
// them: the user a sign-in is for (a user session has no organization), and an error,
// {"error":{"code","reason","message"}}, its reason only where the spec gives one.
var accountUserInfo = map[string]string{"id": accountUser, "email": "live@terma.test"}

func writeAuthError(w http.ResponseWriter, status int, code, reason, message string) {
	body := map[string]string{"code": code, "message": message}
	if reason != "" {
		body["reason"] = reason
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": body})
}

// RefuseDelegation makes delegate refuse the desktop's session with reason (403
// PERMISSION_DENIED), "" to delegate again.
func (a *Account) RefuseDelegation(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refuseDelegation = reason
}

// serveDelegate is the platform's POST /v1/auth/user/delegate, as Terma Desktop calls it:
// its user session's refresh token rotates and a 60-second single-use code is bound to the
// CLI's challenge. A challenge that is not 43 characters is 400, a refresh token that is
// not the desktop's live one 401, and a session that may not delegate 403 NOT_DELEGABLE.
func (a *Account) serveDelegate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken  string `json:"refresh_token"`
		CodeChallenge string `json:"code_challenge"`
		Label         string `json:"label"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if len(body.CodeChallenge) != 43 {
		writeAuthError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "", "code_challenge must be 43 characters")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if body.RefreshToken != a.desktopRefresh {
		writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "", "invalid or expired refresh token")
		return
	}
	if a.refuseDelegation != "" {
		writeAuthError(w, http.StatusForbidden, "PERMISSION_DENIED", a.refuseDelegation, "this session cannot delegate")
		return
	}
	n := a.delegations.Add(1)
	a.desktopRefresh = secretLike(userRefreshPrefix, 0xde5c0+n)
	code := secretLike(userCodePrefix, n)
	a.delegated[code] = delegatedCode{challenge: body.CodeChallenge, label: body.Label, n: n}
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": secretLike(userAccessPrefix, 0xde5c0+n), "refresh_token": a.desktopRefresh,
		"token_type": "Bearer", "expires_in": 3600, "session_id": desktopSession, "family_id": desktopSession, "user": accountUserInfo,
		"code": code, "code_expires_in": 60})
}

// serveDelegatedExchange is POST /v1/auth/user/token for a delegated code: redeemed once,
// with redirect_port 0, the installation's id and the verifier of the challenge it was made
// for, it signs in a session of its own in the desktop session's family. It answers only a
// code's exchange: for a refresh it answers nothing and returns false, so a refresh is
// refused as before.
func (a *Account) serveDelegatedExchange(w http.ResponseWriter, r *http.Request) bool {
	var body struct {
		GrantType      string `json:"grant_type"`
		Code           string `json:"code"`
		CodeVerifier   string `json:"code_verifier"`
		RedirectPort   *int   `json:"redirect_port"`
		InstallationID string `json:"installation_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.GrantType != "authorization_code" {
		return false
	}
	a.mu.Lock()
	grant, ok := a.delegated[body.Code]
	delete(a.delegated, body.Code)
	a.mu.Unlock()
	sum := sha256.Sum256([]byte(body.CodeVerifier))
	// Every failed redemption is the auth gateway's one 401, whatever the account service
	// refused it for.
	if body.InstallationID == "" {
		writeAuthError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "", "installation_id is required")
		return true
	}
	if !ok || body.RedirectPort == nil || *body.RedirectPort != 0 ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != grant.challenge {
		writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "", "invalid or expired credentials")
		return true
	}
	n := grant.n
	refresh := secretLike(userRefreshPrefix, 0xc11000+n)
	a.mu.Lock()
	a.issued[refresh] = true
	a.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": a.token, "refresh_token": refresh, "token_type": "Bearer",
		"expires_in": 3600, "session_id": fmt.Sprintf("delegated-%d", n), "family_id": desktopSession, "user": accountUserInfo})
	return true
}

// serveRevoke is POST /v1/auth/user/revoke: the refresh token in the body names the
// session it ends, and every answer is 200.
func (a *Account) serveRevoke(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	a.mu.Lock()
	a.revoked = append(a.revoked, body.RefreshToken)
	a.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]string{"enforcement": "ENFORCED"})
}

// RevokedDelegated is how many sessions delegated codes signed in were revoked, and how
// many revokes named anything else.
func (a *Account) RevokedDelegated() (delegated, other int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, token := range a.revoked {
		if a.issued[token] {
			delegated++
		} else {
			other++
		}
	}
	return delegated, other
}

// Delegate is what Terma Desktop does with the CLI's challenge: it trades its own refresh
// token for a rotated pair and a code, and returns the code.
func (a *Account) Delegate(challenge, label string) (string, error) {
	a.mu.Lock()
	refresh := a.desktopRefresh
	a.mu.Unlock()
	req, _ := json.Marshal(map[string]string{"refresh_token": refresh, "code_challenge": challenge, "label": label})
	resp, err := http.Post(a.srv.URL+"/v1/auth/user/delegate", "application/json", bytes.NewReader(req))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Code  string `json:"code"`
		Error struct {
			Code   string `json:"code"`
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || out.Code == "" {
		return "", fmt.Errorf("delegate: HTTP %d %s %s, %v", resp.StatusCode, out.Error.Code, out.Error.Reason, err)
	}
	return out.Code, nil
}

// KeyFor is the key the account minted for a project, "" if none.
func (a *Account) KeyFor(project string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.keys[project]
}
