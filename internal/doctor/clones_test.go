package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Doctor lists every clone terma routed through its hooks, a linked worktree by its own
// checkout, and skips one gone since.
func TestDoctorListsRoutedClones(t *testing.T) {
	root, gitDir := listed(t)
	for _, args := range [][]string{
		{"-C", root, "-c", "user.email=d@example.com", "-c", "user.name=d", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", root, "worktree", "add", "-q", filepath.Join(filepath.Dir(root), "wt")},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	dir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	for name, routed := range map[string]string{
		"a": gitDir,
		"b": filepath.Join(gitDir, "worktrees", "wt"),
		"c": filepath.Join(t.TempDir(), "gone", ".git"),
	} {
		d := filepath.Join(dir, "clone-hooks", name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, ".git-dir"), []byte(routed+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	resolved := func(p string) string {
		p = strings.TrimSpace(p)
		// Doctor shows a path under the home folder as ~ (Windows keeps temp dirs there).
		if rest, ok := strings.CutPrefix(p, "~"); ok {
			if home, err := os.UserHomeDir(); err == nil {
				p = home + rest
			}
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	var listedRoots []string
	for _, r := range machineRows(&config.Config{Environment: config.EnvProd, AuthURL: config.DefaultAuthURL}) {
		if r.Label == "" {
			listedRoots = append(listedRoots, resolved(r.Value))
		}
	}
	want := []string{resolved(root), resolved(filepath.Join(filepath.Dir(root), "wt"))}
	if !slices.Equal(listedRoots, want) {
		t.Fatalf("routed clones = %q, want %q", listedRoots, want)
	}
}
