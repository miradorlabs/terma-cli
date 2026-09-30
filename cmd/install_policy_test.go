package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/keystore"
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
