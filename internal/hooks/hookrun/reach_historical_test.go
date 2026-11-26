package hookrun

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// historicalMissingTree builds base/orgA (the session's checkout, with no deleted-sub) and
// base/orgB/secret.txt, and returns base and orgA.
func historicalMissingTree(t *testing.T) (base, orgA string) {
	t.Helper()
	// Resolved, as the scan reports paths: a temp folder may sit behind a symlink or a short name.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	orgA = filepath.Join(base, "orgA")
	if err := os.MkdirAll(orgA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "orgB"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "orgB", "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return base, orgA
}

// A ".." after a component that is gone now may have climbed out of a symlink when the
// command ran, so a past command's reach through it is not known. A live command's stays as
// it was: the shell would have failed on the missing component.
func TestHistoricalDotDotAfterMissingComponentIsUnknown(t *testing.T) {
	t.Parallel()
	_, orgA := historicalMissingTree(t)
	for _, tc := range []struct{ name, cwd, command string }{
		{"deleted subfolder", orgA, "cat " + filepath.ToSlash(filepath.Join(orgA, "deleted-sub")) + "/../../orgB/secret.txt"},
		{"deleted cwd", filepath.Join(orgA, "gone"), "cat ../../orgB/secret.txt"},
	} {
		if got := ShellPathsHistorical(tc.command, tc.cwd); !slices.ContainsFunc(got, isUnknownReach) {
			t.Errorf("%s: ShellPathsHistorical(%q) = %q, want UnknownReach", tc.name, tc.command, got)
		}
		if got := ShellPaths(tc.command, tc.cwd); slices.ContainsFunc(got, isUnknownReach) {
			t.Errorf("%s: ShellPaths(%q) = %q, want no UnknownReach (live behavior unchanged)", tc.name, tc.command, got)
		}
	}
}

// "." cannot change where a path leads, so "./sub/notes.txt" in a deleted cwd is placed as
// the missing path, the same as "sub/notes.txt", and is not an unknown reach. (A bare
// "notes.txt" is not path-like, so a missing one is placed nowhere, with or without a cwd.)
func TestHistoricalDotInMissingCwdIsPlaced(t *testing.T) {
	t.Parallel()
	_, orgA := historicalMissingTree(t)
	cwd := filepath.Join(orgA, "gone")
	dotted := ShellPathsHistorical("cat ./sub/notes.txt", cwd)
	plain := ShellPathsHistorical("cat sub/notes.txt", cwd)
	if slices.ContainsFunc(dotted, isUnknownReach) || !slices.Equal(dotted, plain) {
		t.Errorf("ShellPathsHistorical: ./sub/notes.txt = %q, sub/notes.txt = %q; want the same, no UnknownReach", dotted, plain)
	}
	if want := filepath.Join(cwd, "sub", "notes.txt"); !slices.Contains(dotted, want) {
		t.Errorf("ShellPathsHistorical(cat ./sub/notes.txt) = %q, want it to name %q", dotted, want)
	}
	if got := ShellPathsHistorical("cat ./notes.txt", cwd); !slices.Equal(got, []string{filepath.Join(cwd, "notes.txt")}) {
		t.Errorf("ShellPathsHistorical(cat ./notes.txt) = %q, want only %q", got, filepath.Join(cwd, "notes.txt"))
	}
	if got := ShellPaths("cat ./notes.txt", cwd); len(got) != 0 {
		t.Errorf("ShellPaths(cat ./notes.txt) = %q, want nothing (live behavior unchanged)", got)
	}
}

// VAR+=x onto a variable set outside a past command does not know what VAR held before.
func TestHistoricalAppendToOutsideVariableIsUnknown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got := ShellPathsHistorical(`REPO+=secret.txt; cat "$REPO"`, dir); !slices.ContainsFunc(got, isUnknownReach) {
		t.Errorf("append to outside variable: %q, want UnknownReach", got)
	}
	if got := ShellPathsHistorical(`REPO=a; REPO+=b; cat "$REPO"`, dir); slices.ContainsFunc(got, isUnknownReach) {
		t.Errorf("append to variable set in the command: %q, want no UnknownReach", got)
	}
}
