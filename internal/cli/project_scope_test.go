package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

func TestProjectShowFollowsRepositoryBinding(t *testing.T) {
	t.Setenv("TERMA_PROJECT_ID", "")
	for _, id := range []string{"repo-a", "repo-b"} {
		t.Run(id, func(t *testing.T) {
			repo := installRepo(t)
			if err := termaproject.Save(repo, &termaproject.File{Project: termaproject.Project{ID: id, Name: "Project " + id, OrganizationID: "repo-org"}}); err != nil {
				t.Fatal(err)
			}
			subdir := filepath.Join(repo, "src", "nested")
			if err := os.MkdirAll(subdir, 0755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(subdir)
			t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
			if err := config.UpdateProfile(config.DefaultProfile, func(p *config.Profile) {
				p.OrganizationID, p.OrganizationName = "another-org", "Another organization"
			}); err != nil {
				t.Fatal(err)
			}
			out, err := runTerma(t, "project", "show", "-o", "json")
			var got project
			if err != nil || json.Unmarshal([]byte(out), &got) != nil || got.ID != id || got.Name != "Project "+id || got.OrganizationID != "repo-org" {
				t.Fatalf("project show disagrees with the binding: %v\n%s", err, out)
			}
			file, _ := config.LoadFile()
			if file.Profiles[config.DefaultProfile].OrganizationID != "another-org" {
				t.Fatal("a repository read rewrote machine-wide state")
			}
		})
	}
}

func TestUnboundRepositoryRequiresProject(t *testing.T) {
	repo := installRepo(t)
	t.Setenv("TERMA_PROJECT_ID", "")
	t.Setenv("TERMA_API_KEY", "")
	// An unbound checkout and a directory outside git both need an explicit choice.
	for _, dir := range []string{repo, t.TempDir()} {
		t.Chdir(dir)
		cfg, err := testApp.loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if err := requireProject(cfg); err == nil || !strings.Contains(err.Error(), "terma install") {
			t.Fatalf("unbound directory should require a project: %+v, %v", cfg, err)
		}
	}
	// A nested, unbound checkout must not accidentally use its parent's project.
	if err := termaproject.Save(repo, &termaproject.File{Project: termaproject.Project{ID: "parent-project"}}); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repo, "nested")
	if out, err := exec.Command("git", "init", "-q", nested).CombinedOutput(); err != nil {
		t.Fatalf("nested checkout: %v %s", err, out)
	}
	t.Chdir(nested)
	cfg, err := testApp.loadProjectConfig()
	if err != nil || cfg.ProjectID != "" {
		t.Fatalf("nested checkout inherited a project: %+v, %v", cfg, err)
	}
}

func TestProjectScopeExplicitOverrideAndBinding(t *testing.T) {
	repo := installRepo(t)
	t.Setenv("TERMA_PROJECT_ID", "")
	if err := termaproject.Save(repo, &termaproject.File{Project: termaproject.Project{ID: "bound-repo", Name: "Repository"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testApp.flags = globalFlags{} })
	for _, tc := range []struct{ flag, env, want string }{
		{"", "", "bound-repo"},
		{"", "env-project", "env-project"},
		{"flag-project", "env-project", "flag-project"},
	} {
		testApp.flags = globalFlags{projectID: tc.flag}
		t.Setenv("TERMA_PROJECT_ID", tc.env)
		cfg, err := testApp.loadProjectConfig()
		if err != nil || cfg.ProjectID != tc.want {
			t.Fatalf("scope: %+v, %v; want %s", cfg, err, tc.want)
		}
		if tc.want != "bound-repo" && cfg.ProjectName != "" {
			t.Fatal("explicit override retained the repository's display name")
		}
	}
	testApp.flags = globalFlags{}
	t.Setenv("TERMA_PROJECT_ID", "")
	if err := os.MkdirAll(filepath.Dir(termaproject.Path(repo)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(termaproject.Path(repo), []byte("{invalid"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := testApp.loadProjectConfig(); err == nil {
		t.Fatal("invalid JSON binding was accepted")
	}
}

func TestInstallKeepsProjectChoiceInEachRepo(t *testing.T) {
	repos := []string{installRepo(t), installRepo(t)}
	ids := []string{testProjectID, "770e8400-e29b-41d4-a716-446655440001"}
	t.Setenv("TERMA_PROJECT_ID", "")
	t.Setenv("TERMA_API_KEY", "")
	for i, repo := range repos {
		t.Chdir(repo)
		if out, err := runTerma(t, "install", "--harness", "none", "--project", ids[i], "--yes", "--no-doctor"); err != nil {
			t.Fatalf("install repository %d: %v\n%s", i, err, out)
		}
	}
	for i, repo := range repos {
		t.Chdir(repo)
		if out, err := runTerma(t, "install", "--harness", "none", "--yes", "--no-doctor"); err != nil {
			t.Fatalf("reinstall repository %d: %v\n%s", i, err, out)
		}
		bound, err := termaproject.Load(repo)
		if err != nil || bound.Project.ID != ids[i] {
			t.Fatalf("repository %d changed projects: %+v, %v", i, bound, err)
		}
	}
}
