package live

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	srv   *httptest.Server
	token string
	mints atomic.Int32
	mu    sync.Mutex
	keys  map[string]string // project → minted key
}

// accountOrg is the organization the fake account signs the developer in to.
const accountOrg = "11111111-1111-4111-8111-111111111111"

// StartAccount serves the fake account service until the test ends.
func (sb *Sandbox) StartAccount() *Account {
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
		case "/v1/api-keys/server":
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
	return a
}

// KeyFor is the key the account minted for a project, "" if none.
func (a *Account) KeyFor(project string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.keys[project]
}
