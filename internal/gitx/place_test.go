package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// placeFS is both steps: the checkout that holds path.
func placeFS(path string) (root string, place Place) {
	dir, ok := ResolveFolderFS(path)
	if !ok {
		return "", Unplaced
	}
	root, _, place = PlaceFolderFS(dir)
	return root, place
}

func writePlaceFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A path's checkout is the nearest one that holds its resolved folder: links are followed, a
// nested clone, a submodule and a linked worktree are each their own, and a file not there yet
// is its nearest existing folder's.
func TestPlaceFS(t *testing.T) {
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
	none := resolved(t.TempDir())
	for _, dir := range []string{outer, other, none} {
		writePlaceFile(t, filepath.Join(dir, "file.txt"), "made-up file\n")
	}
	nested := filepath.Join(outer, "vendor", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, nested, "init", "-q")
	writePlaceFile(t, filepath.Join(nested, "file.txt"), "made-up file\n")
	link := filepath.Join(outer, "into-other")
	if err := os.Symlink(other, link); err != nil {
		t.Skip("no symlinks here")
	}
	dangling := filepath.Join(outer, "dangling")
	if err := os.Symlink(filepath.Join(outer, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	// A linked worktree: its .git a file naming its git directory in the main checkout's.
	mustGit(t, outer, "commit", "-q", "--allow-empty", "-m", "made-up")
	linked := filepath.Join(resolved(t.TempDir()), "wt")
	mustGit(t, outer, "worktree", "add", "-q", linked)
	// A submodule: its .git a file naming its git directory in the superproject's modules.
	sub := filepath.Join(outer, "sub")
	modules := filepath.Join(outer, ".git", "modules", "sub")
	writePlaceFile(t, filepath.Join(modules, "HEAD"), "ref: refs/heads/main\n")
	writePlaceFile(t, filepath.Join(modules, "config"), "[core]\n")
	writePlaceFile(t, filepath.Join(sub, ".git"), "gitdir: ../.git/modules/sub\n")
	writePlaceFile(t, filepath.Join(sub, "file.txt"), "made-up file\n")
	sep := string(filepath.Separator)
	for _, tc := range []struct {
		name, path string
		root       string
		want       Place
	}{
		{"a file in a checkout", filepath.Join(outer, "file.txt"), outer, InCheckout},
		{"the checkout itself", outer, outer, InCheckout},
		{"a file in no checkout", filepath.Join(none, "file.txt"), "", NoCheckout},
		{"through a link into another checkout", filepath.Join(link, "file.txt"), other, InCheckout},
		{"a link to another checkout", link, other, InCheckout},
		{"a clone nested in a checkout", filepath.Join(nested, "file.txt"), nested, InCheckout},
		{"a submodule", filepath.Join(sub, "file.txt"), sub, InCheckout},
		{"a linked worktree", filepath.Join(linked, "new.txt"), linked, InCheckout},
		{"a file not there yet", filepath.Join(other, "new", "deeper", "file.txt"), other, InCheckout},
		{"a dangling link", dangling, "", Unplaced},
		{"a file through a dangling link", filepath.Join(dangling, "file.txt"), "", Unplaced},
		{"a missing path that climbs", outer + sep + filepath.Join("missing", "x") + sep + ".." + sep + "y", "", Unplaced},
		{"a relative path", "file.txt", "", Unplaced},
	} {
		if root, got := placeFS(tc.path); got != tc.want || root != tc.root {
			t.Errorf("%s: %q %d, want %q %d", tc.name, root, got, tc.root, tc.want)
		}
	}

	// With GIT_DIR set any folder reads as that checkout, so none can be placed.
	t.Setenv("GIT_DIR", filepath.Join(outer, ".git"))
	if _, got := placeFS(filepath.Join(other, "file.txt")); got != Unplaced {
		t.Errorf("with GIT_DIR set: %d", got)
	}
}

// A folder is placed only when ResolveFolderFS's answer is: a relative one is not.
func TestPlaceFolderFSWantsAnAbsoluteFolder(t *testing.T) {
	if _, _, got := PlaceFolderFS(strings.TrimPrefix(t.TempDir(), string(filepath.Separator))); got != Unplaced {
		t.Errorf("a relative folder: %d", got)
	}
}
