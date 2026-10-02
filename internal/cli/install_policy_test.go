package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// Install knows nothing of the team's collection policy: under a server key with no
// developer login, and no policy fetched, it binds and writes, and reads no policy.
func TestInstallReadsNoPolicy(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	sandboxMachine(t)
	gitRepoHere(t)
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMA_POLICY_STUB", "")
	t.Setenv("TERMA_API_KEY", policyTestKey)
	id := projectsIn(orgA().ID)[0].ID
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/identity" || r.Header.Get("Authorization") != "Bearer "+policyTestKey {
			http.Error(w, "expected server-key identity request", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"project_id": id, "organization_id": orgA().ID, "auth_type": "server_key"})
	}))
	defer gateway.Close()
	t.Setenv("TERMA_API_URL", gateway.URL)
	out, err := within(5*time.Second).combined(t, "install", "--harness", "none", "--adapters", "cursor", "--team", id, "--yes", "--no-browser")
	if err != nil {
		t.Fatalf("install without a policy: %v\n%s", err, out)
	}
	bound, err := termaproject.Load(root)
	if err != nil || bound.Project.ID != id || bound.Project.OrganizationID != orgA().ID {
		t.Fatalf("server key lost its binding: %v %v", bound, err)
	}
	if f.policies.Load() != 0 || f.keysMint.Load() != 0 {
		t.Fatalf("policy reads=%d, key mints=%d", f.policies.Load(), f.keysMint.Load())
	}
}
