package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

func TestInstallChecksRepositoryPermissionWithoutNativeExporters(t *testing.T) {
	for _, agent := range []string{"cursor", "antigravity", "none"} {
		t.Run(agent, func(t *testing.T) {
			f := newFakeAuth(t)
			authSandbox(t, f)
			sandboxMachine(t)
			gitRepoHere(t)
			t.Setenv("TERMA_POLICY_STUB", "")
			f.policyBody = `{"policy":{"version":"1.0","terma":{"per_repository":{"members_can_add_repositories":false},"capture":{"exclude_paths":[],"exclude_prompts":true,"exclude_tool_content":false,"signals":["logs"]}}},"revision":1,"updated_at":"2026-09-30T12:27:05Z"}`
			if _, err := auth.SaveCredential("default", storedSession(f, orgA())); err != nil {
				t.Fatal(err)
			}
			id := projectsIn(orgA().ID)[0].ID
			out, err := within(5*time.Second).combined(t, "install", "--harness", "none", "--adapters", agent, "--project", id, "--yes", "--no-doctor", "--no-browser")
			if err == nil || !strings.Contains(err.Error(), "does not allow members to add repositories") {
				t.Fatalf("install bypassed repository permission: %v\n%s", err, out)
			}
			if f.policies.Load() != 1 || f.keysMint.Load() != 0 || keystore.Get(id) != "" {
				t.Fatalf("policy reads=%d, key mints=%d", f.policies.Load(), f.keysMint.Load())
			}
			for _, path := range []string{termaproject.FileName, ".cursor/hooks.json", ".agents/hooks.json", ".terma/hooks/prepare-commit-msg"} {
				if _, err := os.Stat(filepath.FromSlash(path)); !os.IsNotExist(err) {
					t.Fatalf("denied install wrote %s: %v", path, err)
				}
			}
		})
	}
}

func TestInstallDeniedRepositoryPermissionStillAllowsExistingBinding(t *testing.T) {
	f := boundRepo(t, termaproject.Project{ID: projectsIn(orgA().ID)[0].ID, OrganizationID: orgA().ID}, true)
	t.Setenv("TERMA_POLICY_STUB", "")
	f.policyBody = `{"policy":{"version":"1.0","terma":{"per_repository":{"members_can_add_repositories":false},"capture":{"exclude_paths":[],"exclude_prompts":true,"exclude_tool_content":false,"signals":["logs"]}}},"revision":1,"updated_at":"2026-09-30T12:27:05Z"}`
	if out, err := within(5*time.Second).combined(t, "install", "--harness", "none", "--adapters", "cursor", "--yes", "--no-doctor", "--no-browser"); err != nil {
		t.Fatalf("existing connected repository was refused: %v\n%s", err, out)
	}
	if f.policies.Load() != 1 || f.keysMint.Load() != 1 {
		t.Fatalf("policy reads=%d, key mints=%d", f.policies.Load(), f.keysMint.Load())
	}
}

func TestInstallWithServerKeyUsesDeveloperLoginForPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		loginOrg   string
		canAdd     bool
		wantErr    string
		wantPolicy int32
	}{
		{name: "allowed", loginOrg: orgA().ID, canAdd: true, wantPolicy: 1},
		{name: "denied", loginOrg: orgA().ID, wantErr: "does not allow members to add repositories", wantPolicy: 1},
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
			f.policyBody = fmt.Sprintf(`{"policy":{"version":"1.0","terma":{"per_repository":{"members_can_add_repositories":%t},"capture":{"exclude_paths":[],"exclude_prompts":true,"exclude_tool_content":false,"signals":["logs"]}}},"revision":1,"updated_at":"2026-09-30T12:27:05Z"}`, tc.canAdd)
			if tc.loginOrg != "" {
				if _, err := auth.SaveCredential("default", storedSession(f, organization{ID: tc.loginOrg})); err != nil {
					t.Fatal(err)
				}
			}
			out, err := within(5*time.Second).combined(t, "install", "--harness", "none", "--adapters", "cursor", "--project", id, "--yes", "--no-doctor", "--no-browser")
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
