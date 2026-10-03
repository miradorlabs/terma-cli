package gitx

import (
	"os"
	"path/filepath"
	"testing"
)

// A working copy is named by its remote, origin first, in any of git's URL forms, else by
// its folder; a linked worktree by its main checkout's.
func TestRepositoryFS(t *testing.T) {
	for _, tc := range []struct{ name, config, name2, path string }{
		{"ssh scp form", "[remote \"origin\"]\n\turl = git@github.com:Mirador/Sales.git\n", "Sales", "Mirador/Sales"},
		{"https", "[remote \"origin\"]\n\turl = https://tok@github.com/o/app\n", "app", "o/app"},
		{"ssh scheme", "[remote \"origin\"]\n\turl = ssh://git@gitlab.example.com:2222/group/sub/app.git\n", "app", "group/sub/app"},
		{"origin wins over an earlier remote", "[remote \"fork\"]\n\turl = git@github.com:me/fork.git\n[remote \"origin\"]\n\turl = git@github.com:o/app.git\n", "app", "o/app"},
		{"else the first remote", "[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = https://github.com/o/up.git\n[remote \"fork\"]\n\turl = https://github.com/me/fork.git\n", "up", "o/up"},
		{"no remote: the folder", "[core]\n\tbare = false\n", "checkout", ""},
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
			if name, path := RepositoryFS(root, gitDir); name != tc.name2 || path != tc.path {
				t.Fatalf("RepositoryFS = %q, %q; want %q, %q", name, path, tc.name2, tc.path)
			}
		})
	}
	if name, path := RepositoryFS(filepath.Join(t.TempDir(), "scratch"), ""); name != "scratch" || path != "" {
		t.Fatalf("outside Git = %q, %q", name, path)
	}
}
