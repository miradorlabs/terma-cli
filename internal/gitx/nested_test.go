package gitx

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// nestedOf is every checkout NestedCheckoutsFS yields below dir, by root, and its error.
func nestedOf(dir string) (map[string]string, error) {
	got := map[string]string{}
	err := NestedCheckoutsFS(dir, func(root, gitDir string) bool {
		got[root] = gitDir
		return true
	})
	return got, err
}

// The checkouts below a folder: a nested clone, a submodule and a clone nested in that clone;
// not the folder's own checkout, not a .git folder's insides, and not a checkout a link leads
// to.
func TestNestedCheckoutsFS(t *testing.T) {
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE"} {
		t.Setenv(k, "") // registers the restore
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	resolved := func(dir string) string {
		d, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	outer, other := resolved(initRepo(t)), resolved(initRepo(t))
	nested := filepath.Join(outer, "vendor", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, nested, "init", "-q")
	deeper := filepath.Join(nested, "lib", "deeper")
	if err := os.MkdirAll(deeper, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, deeper, "init", "-q")
	// A submodule: its .git a file naming its git directory in the superproject's modules.
	sub := filepath.Join(outer, "sub")
	modules := filepath.Join(outer, ".git", "modules", "sub")
	writePlaceFile(t, filepath.Join(modules, "HEAD"), "ref: refs/heads/main\n")
	writePlaceFile(t, filepath.Join(modules, "config"), "[core]\n")
	writePlaceFile(t, filepath.Join(sub, ".git"), "gitdir: ../.git/modules/sub\n")
	// A .git file that names nothing: yielded, with no git directory.
	broken := filepath.Join(outer, "broken")
	writePlaceFile(t, filepath.Join(broken, ".git"), "gitdir: nowhere\n")
	plain := filepath.Join(outer, "plain", "deep")
	writePlaceFile(t, filepath.Join(plain, "file.txt"), "made-up file\n")

	got, err := nestedOf(outer)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		nested: filepath.Join(nested, ".git"),
		deeper: filepath.Join(deeper, ".git"),
		sub:    modules,
		broken: "",
	}
	if len(got) != len(want) {
		t.Errorf("checkouts below the outer one: %v, want %v", got, want)
	}
	for root, gitDir := range want {
		if g, ok := got[root]; !ok || filepath.Clean(g) != filepath.Clean(gitDir) {
			t.Errorf("%s: git directory %q (yielded %v), want %q", root, g, ok, gitDir)
		}
	}
	// A folder that holds no checkout, and a nested clone's own folder, which holds only the
	// clone below it.
	if got, err := nestedOf(filepath.Join(outer, "plain")); err != nil || len(got) != 0 {
		t.Errorf("a plain folder: %v %v", got, err)
	}
	if got, err := nestedOf(nested); err != nil || len(got) != 1 || got[deeper] == "" {
		t.Errorf("the nested clone: %v %v", got, err)
	}
	// Stopping at the first checkout.
	n := 0
	if err := NestedCheckoutsFS(outer, func(string, string) bool { n++; return false }); err != nil || n != 1 {
		t.Errorf("stopped walk: %d yielded, %v", n, err)
	}

	// A link to another checkout is not followed.
	links := filepath.Join(outer, "links")
	if err := os.MkdirAll(links, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(links, "into-other")); err != nil {
		t.Skip("no symlinks here")
	}
	if got, err := nestedOf(links); err != nil || len(got) != 0 {
		t.Errorf("a folder holding a link to a checkout: %v %v", got, err)
	}
}

// A walk past NestedWalkLimit folders, or one that cannot read a folder, says so; a relative
// folder, or GIT_DIR set, walks nothing.
func TestNestedCheckoutsFSFailsClosed(t *testing.T) {
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	for _, d := range []string{"a", "b", filepath.Join("a", "c")} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	defer func(was int) { NestedWalkLimit = was }(NestedWalkLimit)
	// root, a, b, a/c: four folders.
	NestedWalkLimit = 4
	if _, err := nestedOf(root); err != nil {
		t.Errorf("four folders under a bound of four: %v", err)
	}
	NestedWalkLimit = 3
	if _, err := nestedOf(root); !errors.Is(err, ErrNestedWalkBound) {
		t.Errorf("four folders under a bound of three: %v", err)
	}
	NestedWalkLimit = 20000

	for _, dir := range []string{filepath.Join("some", "folder"), "."} {
		if err := NestedCheckoutsFS(dir, func(string, string) bool { return true }); err == nil {
			t.Errorf("a relative folder %q walked", dir)
		}
	}
	if _, err := nestedOf(filepath.Join(root, "missing")); err == nil {
		t.Error("a missing folder walked")
	}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		locked := filepath.Join(root, "a", "c")
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(locked, 0o755) }()
		// Some users read a folder its mode forbids (root, and a sandbox's user with that
		// capability): there is nothing unreadable to walk past then.
		if _, readErr := os.ReadDir(locked); readErr != nil {
			if _, err := nestedOf(root); err == nil {
				t.Error("a folder that cannot be read was walked past")
			}
		}
	}

	t.Setenv("GIT_DIR", filepath.Join(root, ".git"))
	if _, err := nestedOf(root); err == nil {
		t.Error("with GIT_DIR set the walk went on")
	}
}

// BenchmarkNestedCheckoutsFS walks this repository's own checkout.
func BenchmarkNestedCheckoutsFS(b *testing.B) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if err := NestedCheckoutsFS(root, func(string, string) bool { return true }); err != nil {
			b.Fatal(err)
		}
	}
}
