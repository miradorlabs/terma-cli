package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

func TestInstallWithServerKeyUsesDeveloperLoginForPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		loginOrg   string
		wantErr    string
		wantPolicy int32
	}{
		{name: "allowed", loginOrg: orgA().ID, wantPolicy: 1},
		{name: "missing-login"},
		{name: "wrong-organization", loginOrg: orgB().ID, wantErr: "another organization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			f.policyBody = `{"policy":{"version":"1.0","terma":{"per_repository":{},"capture":{"exclude_paths":[],"exclude_prompts":true,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-09-30T12:27:05Z"}`
			if tc.loginOrg != "" {
				if _, err := auth.SaveCredential("default", storedSession(f, organization{ID: tc.loginOrg})); err != nil {
					t.Fatal(err)
				}
			}
			out, err := within(5*time.Second).combined(t, "install", "--harness", "none", "--adapters", "cursor", "--team", id, "--yes", "--no-browser")
			switch {
			case tc.loginOrg == "":
				if !errors.Is(err, auth.ErrNotLoggedIn) {
					t.Fatalf("server key bypassed developer login: %v\n%s", err, out)
				}
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("install error = %v, want %s\n%s", err, tc.wantErr, out)
				}
			default:
				if err != nil {
					t.Fatalf("install with server key and developer login: %v\n%s", err, out)
				}
				bound, err := termaproject.Load(root)
				if err != nil || bound.Project.ID != id || bound.Project.OrganizationID != orgA().ID {
					t.Fatalf("server key lost its binding: %v %v", bound, err)
				}
			}
			if f.policies.Load() != tc.wantPolicy || f.keysMint.Load() != 0 {
				t.Fatalf("policy reads=%d, key mints=%d", f.policies.Load(), f.keysMint.Load())
			}
			if err != nil {
				if _, err := os.Stat(termaproject.FileName); !os.IsNotExist(err) {
					t.Fatalf("refused install wrote a binding: %v", err)
				}
			}
		})
	}
}
