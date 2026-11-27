package hookrun

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A past session's search inside its checkout reaches the checkouts nested in it, at any
// depth and only below the folder searched; node_modules and symbolic links are not walked.
// A hook's search is unchanged: it stays in the checkout, which it has no time to walk.
func TestHistoricalSearchReachesCheckoutsNestedInItsOwn(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	deep := filepath.Join(repo, "a", "b", "c", "d", "e", "vendor", "other")
	side := filepath.Join(repo, "libs", "side")
	inner := filepath.Join(side, "third_party", "inner")
	hidden := filepath.Join(repo, "web", "node_modules", "pkg")
	outside := filepath.Join(root, "elsewhere")
	for _, d := range []string{repo, deep, side, inner, hidden, outside} {
		makeValidGitDir(t, filepath.Join(d, ".git"))
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(inner), ".git"), []byte("gitdir: /nowhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}

	got, unknown := ExpandReachHistorical([]string{repo}, repo)
	if unknown {
		t.Fatalf("ExpandReachHistorical(repo) = %q, unknown", got)
	}
	for _, want := range []string{deep, side, inner, filepath.Dir(inner)} {
		if !slices.Contains(got, want) {
			t.Errorf("ExpandReachHistorical(repo) = %q, missing %q", got, want)
		}
	}
	for _, not := range []string{hidden, outside} {
		if slices.Contains(got, not) {
			t.Errorf("ExpandReachHistorical(repo) = %q, want no %q", got, not)
		}
	}
	if got, _ := ExpandReachHistorical([]string{filepath.Join(repo, "libs")}, repo); !slices.Contains(got, side) || slices.Contains(got, deep) {
		t.Errorf("ExpandReachHistorical(repo/libs) = %q, want %q and not %q", got, side, deep)
	}

	live := searchedBy([]string{repo}, repo, os.LookupEnv, time.Now().Add(time.Minute))
	if slices.ContainsFunc(live, isUnknownReach) || slices.Contains(live, side) {
		t.Errorf("a hook's search of repo = %q, want only repo", live)
	}
}

// A checkout with more folders than the limit, or one that cannot be read, fails closed.
func TestListNestedFailsClosed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, d := range []string{"a/x", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	none := func(string) bool { return false }
	if _, ok := listNested(root, none, 3); ok {
		t.Errorf("listNested over 4 folders with a limit of 3 = ok, want false")
	}
	if _, ok := listNested(root, none, 4); !ok {
		t.Errorf("listNested over 4 folders with a limit of 4 = false, want ok")
	}
	if _, ok := listNested(filepath.Join(root, "missing"), none, 4); ok {
		t.Errorf("listNested of a missing checkout = ok, want false")
	}
}
