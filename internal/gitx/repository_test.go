package gitx

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A checkout is named by its folder and origin's repository name, in any of git's URL
// forms, with origin's owner/name as its path; another remote names nothing.
func TestRepositoryFS(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		names        []string
		path         string
	}{
		{"ssh scp form", "[remote \"origin\"]\n\turl = git@github.com:Mirador/Sales.git\n", []string{"checkout", "Sales"}, "Mirador/Sales"},
		{"https", "[remote \"origin\"]\n\turl = https://tok@github.com/o/app\n", []string{"checkout", "app"}, "o/app"},
		{"ssh scheme", "[remote \"origin\"]\n\turl = ssh://git@gitlab.example.com:2222/group/sub/app.git\n", []string{"checkout", "app"}, "group/sub/app"},
		{"only origin counts", "[remote \"fork\"]\n\turl = git@github.com:me/fork.git\n", []string{"checkout"}, ""},
		{"no remote", "[core]\n\tbare = false\n", []string{"checkout"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "checkout")
			gitDir := filepath.Join(root, ".git")
			if err := os.MkdirAll(gitDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(tc.config), 0o644); err != nil {
				t.Fatal(err)
			}
			if names, path := RepositoryFS(root, gitDir); !slices.Equal(names, tc.names) || path != tc.path {
				t.Fatalf("RepositoryFS = %q, %q; want %q, %q", names, path, tc.names, tc.path)
			}
		})
	}
}

// Outside Git a working folder is named by itself and every parent below the home
// directory.
func TestRepositoryFSOutsideGit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, "work", "notes", "today")
	if names, path := RepositoryFS(dir, ""); !slices.Equal(names, []string{"today", "notes", "work"}) || path != "" {
		t.Fatalf("RepositoryFS = %q, %q", names, path)
	}
}

// Windows remotes and CRLF configs: a local origin names the repository but has no
// owner/name, and a drive letter is never an scp host.
func TestRepositoryFSWindowsShapes(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		names     []string
		path      string
	}{
		{"file URL with a drive", "file:///C:/repos/app.git", []string{"checkout", "app"}, ""},
		{"drive path", `C:\repos\billing`, []string{"checkout", "billing"}, ""},
		{"UNC path", `\\server\share\tools.git`, []string{"checkout", "tools"}, ""},
		{"plain local path", "/srv/git/web.git/", []string{"checkout", "web"}, ""},
		{"scp form", "git@github.com:acme/app.git", []string{"checkout", "app"}, "acme/app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "checkout")
			gitDir := filepath.Join(root, ".git")
			if err := os.MkdirAll(gitDir, 0o755); err != nil {
				t.Fatal(err)
			}
			config := "[core]\r\n\tbare = false\r\n[remote \"origin\"]\r\n\turl = " + tc.url + "\r\n"
			if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			if names, path := RepositoryFS(root, gitDir); !slices.Equal(names, tc.names) || path != tc.path {
				t.Fatalf("RepositoryFS = %q, %q; want %q, %q", names, path, tc.names, tc.path)
			}
		})
	}
	if got := NormalizeRemote(`C:\repos\billing`); got != "" {
		t.Fatalf("a drive path normalized to %q", got)
	}
}

// The home folder itself never names a folder outside Git, nor does anything above it.
func TestRepositoryFSStopsBeforeHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if names, _ := RepositoryFS(home, ""); len(names) != 0 {
		t.Fatalf("the home folder named %q", names)
	}
	if names, _ := RepositoryFS(filepath.Join(home, "notes"), ""); !slices.Equal(names, []string{"notes"}) {
		t.Fatalf("RepositoryFS = %q", names)
	}
}
