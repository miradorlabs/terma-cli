package gitx

import (
	"os"
	"path/filepath"
	"testing"
)

// A checkout is named by origin alone, read from a CRLF config too; another remote or a
// local origin names nothing.
func TestRepositoryFS(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, config, want string }{
		{"scp form", "[remote \"origin\"]\n\turl = git@github.com:Mirador/Sales.git\n", "github.com/Mirador/Sales"},
		{"https with a token", "[remote \"origin\"]\n\turl = https://tok@github.com/o/app\n", "github.com/o/app"},
		{"ssh scheme with a port", "[remote \"origin\"]\n\turl = ssh://git@gitlab.example.com:2222/group/sub/app.git\n", "gitlab.example.com/group/sub/app"},
		{"CRLF", "[core]\r\n\tbare = false\r\n[remote \"origin\"]\r\n\turl = git@github.com:acme/app.git\r\n", "github.com/acme/app"},
		{"only origin counts", "[remote \"fork\"]\n\turl = git@github.com:me/fork.git\n", ""},
		{"no remote", "[core]\n\tbare = false\n", ""},
		{"a local path", "[remote \"origin\"]\n\turl = /srv/git/web.git\n", ""},
		{"a file URL with a drive", "[remote \"origin\"]\r\n\turl = file:///C:/repos/app.git\r\n", ""},
		{"a drive path", "[remote \"origin\"]\r\n\turl = C:\\\\repos\\\\billing\r\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitDir := filepath.Join(t.TempDir(), "checkout", ".git")
			if err := os.MkdirAll(gitDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(tc.config), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := RepositoryFS(gitDir); got != tc.want {
				t.Fatalf("RepositoryFS = %q, want %q", got, tc.want)
			}
		})
	}
	if got := RepositoryFS(""); got != "" {
		t.Fatalf("outside git: %q", got)
	}
}

// A linked worktree reads origin from the repository its commondir names, in either
// separator, a bare repository's included.
func TestRepositoryFSLinkedWorktree(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, commondir string }{
		{"relative, as git writes it", "../.."},
		{"absolute", "MAIN"},
		{"absolute, forward slashes", "MAIN/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			common := filepath.Join(t.TempDir(), "billing", ".git")
			gitDir := filepath.Join(common, "worktrees", "fix-1")
			if err := os.MkdirAll(gitDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(common, "config"), []byte("[remote \"origin\"]\r\n\turl = git@github.com:acme/billing.git\r\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			dir := tc.commondir
			switch dir {
			case "MAIN":
				dir = common
			case "MAIN/":
				dir = filepath.ToSlash(common) + "/"
			}
			if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte(dir+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := RepositoryFS(gitDir); got != "github.com/acme/billing" {
				t.Fatalf("RepositoryFS = %q", got)
			}
		})
	}
}

// An origin splits into the vcs attributes: a GitLab group path is the owner, and only a
// host that names its provider gives one.
func TestParseOrigin(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]Origin{
		"github.com/acme/app":            {Provider: "github", Owner: "acme", Name: "app"},
		"gitlab.com/group/sub/app":       {Provider: "gitlab", Owner: "group/sub", Name: "app"},
		"bitbucket.org/team/app":         {Provider: "bitbucket", Owner: "team", Name: "app"},
		"git.example.com/platform/infra": {Owner: "platform", Name: "infra"},
	} {
		if got, ok := ParseOrigin(id); !ok || got != want {
			t.Errorf("ParseOrigin(%q) = %+v, %v; want %+v", id, got, ok, want)
		}
	}
	for _, id := range []string{"", "github.com", "github.com/app", "github.com/acme/"} {
		if got, ok := ParseOrigin(id); ok {
			t.Errorf("ParseOrigin(%q) = %+v, want no owner", id, got)
		}
	}
}
