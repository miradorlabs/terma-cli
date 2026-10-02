package e2e

import (
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
	mu        sync.Mutex
	keys      map[string]string // project → minted key
}

// accountOrg is the organization the fake account signs the developer in to.
const accountOrg = "11111111-1111-4111-8111-111111111111"

// StartAccount serves the fake account service until the test ends.
func (sb *Sandbox) StartAccount() *Account {
	if sb.account != nil {
		return sb.account
	}
	a := &Account{token: "ter_cli_" + accountOrg, keys: map[string]string{}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+a.token {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"code":"UNAUTHENTICATED","message":"invalid token"}`)
			return
		}
		switch r.URL.Path {
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(map[string]any{"organization_id": accountOrg, "auth_type": "cli_token", "user_id": "u-live", "email": "live@terma.test"})
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
			fmt.Fprint(w, `{"policy":{"version":"1.0","terma":{"per_repository":{},"capture":{"exclude_paths":[],"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-09-30T12:27:05Z"}`)
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
	// The developer is signed in: the credential `terma setup` would have stored.
	creds := map[string]any{"default": map[string]any{"active": accountOrg, "organizations": map[string]any{accountOrg: map[string]any{
		"access_token": a.token, "refresh_token": "ter_clr_live", "expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		"auth_url": a.srv.URL, "organization_id": accountOrg, "user_email": "live@terma.test",
	}}}}
	data, _ := json.MarshalIndent(creds, "", "  ")
	sb.writeAbs(filepath.Join(sb.TermaConfig, "credentials.json"), string(data)+"\n")
	sb.ExtraEnv = append(sb.ExtraEnv, "TERMA_AUTH_URL="+a.srv.URL, "TERMA_API_URL="+a.srv.URL, "TERMA_APP_URL="+a.srv.URL)
	// Mirror the organization selection that login persists, keeping the existing
	// private sandbox's endpoint settings.
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
	profile := profiles["default"].(map[string]any)
	profile["organization_id"] = accountOrg
	// Services do not inherit ExtraEnv. Persist the selected deployment as setup
	// does for a custom host, so launchd/systemd use the same scoped policy/login.
	profile["auth_url"], profile["api_url"], profile["app_url"] = a.srv.URL, a.srv.URL, a.srv.URL
	data, _ = json.Marshal(file)
	sb.writeAbs(path, string(data))
	sb.account = a
	return a
}

// KeyFor is the key the account minted for a project, "" if none.
func (a *Account) KeyFor(project string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.keys[project]
}
