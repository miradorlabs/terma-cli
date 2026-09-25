package cmd

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// A server key (TERMA_API_KEY) is scoped to one project, and the account service that
// lists projects accepts only a signed-in user. Install binds the key's own project,
// read from the API gateway's /v1/identity, and never asks the account service.
func TestInstallWithAServerKeyBindsTheKeysProject(t *testing.T) {
	const keyProject, keyOrg = "11111111-2222-4333-8444-555555555555", "99999999-8888-4777-8666-555555555555"
	var paths []string
	gw := fakeGateway(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/v1/identity" {
			http.Error(w, `{"error":{"code":"PERMISSION_DENIED","message":"invalid credential prefix"}}`, http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"project_id": keyProject, "organization_id": keyOrg, "auth_type": "server_key"})
	})
	run := termaRun{within: 30 * time.Second, env: gw}

	repo := installRepo(t) // its config dir is replaced by the gateway's when run sets env
	out, err := run.combined(t, "install", "--harness", "none", "--project", keyProject, "--adapters", "claude", "--yes", "--no-doctor", "--no-browser")
	if err != nil {
		t.Fatalf("install with a server key and its own project: %v\n%s", err, out)
	}
	bound, err := termaproject.Load(repo)
	if err != nil || bound.Project.ID != keyProject || bound.Project.OrganizationID != keyOrg {
		t.Fatalf("binding %+v, err %v", bound, err)
	}
	// /v1/identity names no project, and the committed file says nothing rather than "".
	if raw, err := os.ReadFile(filepath.Join(repo, ".terma", "settings.json")); err != nil || strings.Contains(string(raw), `"name"`) {
		t.Fatalf("the binding should carry no project name: %v\n%s", err, raw)
	}
	for _, p := range paths {
		if p == "/v1/projects" {
			t.Fatalf("install asked the account service, which rejects server keys: %v", paths)
		}
	}

	// Re-running without --project keeps the key's project.
	if out, err := run.combined(t, "install", "--harness", "none", "--adapters", "claude", "--yes", "--no-doctor", "--no-browser"); err != nil {
		t.Fatalf("reinstall: %v\n%s", err, out)
	}

	// Another project is refused, by name, and the binding is left alone.
	out, err = run.combined(t, "install", "--harness", "none", "--project", "22222222-2222-4333-8444-555555555555", "--adapters", "claude", "--yes", "--no-doctor", "--no-browser")
	if err == nil || !strings.Contains(err.Error(), keyProject) {
		t.Fatalf("a --project the key does not belong to was accepted: %v\n%s", err, out)
	}
	if bound, _ := termaproject.Load(repo); bound.Project.ID != keyProject {
		t.Fatalf("the refused install rebound the repository: %+v", bound)
	}
}
