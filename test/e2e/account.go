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
// and the relay need them: a signed-in developer (/v1/whoami) and server keys minted
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
	// desktopRefresh is Terma Desktop's refresh token, which /v1/auth/cli/delegate rotates;
	// delegated are its unredeemed codes, each bound to the CLI's challenge, and delegations
	// counts its calls.
	desktopRefresh string
	delegated      map[string]delegatedCode
	delegations    atomic.Int32
}

// delegatedCode is a code the delegate endpoint made for a CLI's challenge and label.
type delegatedCode struct{ challenge, label string }

// accountOrg is the organization the fake account signs the developer in to, and
// accountUser the developer it signs in.
const (
	accountOrg  = "11111111-1111-4111-8111-111111111111"
	accountUser = "22222222-2222-4222-8222-222222222222"
)

// StartAccount serves the fake account service until the test ends.
func (sb *Sandbox) StartAccount() *Account {
	if sb.account != nil {
		return sb.account
	}
	a := &Account{token: "ter_cli_" + accountOrg, keys: map[string]string{}, desktopRefresh: "ter_clr_desktop_0", delegated: map[string]delegatedCode{}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The two sign-in endpoints take no bearer: the body is the credential.
		switch r.URL.Path {
		case "/v1/auth/cli/delegate":
			a.serveDelegate(w, r)
			return
		case "/v1/auth/cli/token":
			a.serveDelegatedExchange(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+a.token {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"code":"UNAUTHENTICATED","message":"invalid token"}`)
			return
		}
		switch r.URL.Path {
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(map[string]any{"organization_id": accountOrg, "auth_type": "cli_token", "user_id": accountUser, "email": "live@terma.test"})
		case "/v1/organizations":
			_ = json.NewEncoder(w).Encode(map[string]any{"organizations": []map[string]any{{"id": accountOrg, "name": "Live", "role": "admin"}}})
		case "/v1/projects":
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

// SignIn signs the developer in to the fake account under profile: the credential
// `terma setup` would have stored and the organization selection login persists, with
// the account's hosts, so launchd/systemd relays (which do not inherit ExtraEnv) load
// the same scoped policy and login.
func (sb *Sandbox) SignIn(profile string) {
	a := sb.account
	credsPath := filepath.Join(sb.TermaConfig, "credentials.json")
	creds := map[string]any{}
	if data, err := os.ReadFile(credsPath); err == nil {
		_ = json.Unmarshal(data, &creds)
	}
	creds[profile] = map[string]any{"active": accountOrg, "organizations": map[string]any{accountOrg: map[string]any{
		"access_token": a.token, "refresh_token": "ter_clr_live", "expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		"auth_url": a.srv.URL, "organization_id": accountOrg, "user_email": "live@terma.test", "user_id": accountUser,
	}}}
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

// serveDelegate is the platform's POST /v1/auth/cli/delegate, as Terma Desktop calls it:
// its refresh token rotates and a 60-second single-use code is bound to the CLI's challenge.
func (a *Account) serveDelegate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken  string `json:"refresh_token"`
		CodeChallenge string `json:"code_challenge"`
		Label         string `json:"label"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	a.mu.Lock()
	defer a.mu.Unlock()
	if body.RefreshToken != a.desktopRefresh || body.CodeChallenge == "" {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"code":"UNAUTHENTICATED","message":"invalid refresh token"}`)
		return
	}
	n := a.delegations.Add(1)
	a.desktopRefresh = fmt.Sprintf("ter_clr_desktop_%d", n)
	code := fmt.Sprintf("ter_cod_delegated_%d", n)
	a.delegated[code] = delegatedCode{challenge: body.CodeChallenge, label: body.Label}
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("ter_cli_desktop_%d", n), "refresh_token": a.desktopRefresh,
		"token_type": "Bearer", "expires_in": 3600, "session_id": "desktop", "code": code, "code_expires_in": 60})
}

// serveDelegatedExchange is POST /v1/auth/cli/token for a delegated code: redeemed once,
// with redirect_port 0 and the verifier of the challenge it was made for.
func (a *Account) serveDelegatedExchange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GrantType    string `json:"grant_type"`
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
		RedirectPort *int   `json:"redirect_port"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	a.mu.Lock()
	grant, ok := a.delegated[body.Code]
	delete(a.delegated, body.Code)
	a.mu.Unlock()
	sum := sha256.Sum256([]byte(body.CodeVerifier))
	if !ok || body.GrantType != "authorization_code" || body.RedirectPort == nil || *body.RedirectPort != 0 ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != grant.challenge {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":"INVALID_ARGUMENT","message":"invalid grant"}`)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": a.token, "refresh_token": "ter_clr_live", "token_type": "Bearer",
		"expires_in": 3600, "session_id": "delegated", "organization": map[string]string{"id": accountOrg, "name": "Live"},
		"user": map[string]string{"id": accountUser, "email": "live@terma.test"}})
}

// Delegate is what Terma Desktop does with the CLI's challenge: it trades its own refresh
// token for a rotated pair and a code, and returns the code.
func (a *Account) Delegate(challenge, label string) (string, error) {
	a.mu.Lock()
	refresh := a.desktopRefresh
	a.mu.Unlock()
	req, _ := json.Marshal(map[string]string{"refresh_token": refresh, "code_challenge": challenge, "label": label})
	resp, err := http.Post(a.srv.URL+"/v1/auth/cli/delegate", "application/json", bytes.NewReader(req))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || out.Code == "" {
		return "", fmt.Errorf("delegate: HTTP %d, %v", resp.StatusCode, err)
	}
	return out.Code, nil
}

// KeyFor is the key the account minted for a project, "" if none.
func (a *Account) KeyFor(project string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.keys[project]
}
