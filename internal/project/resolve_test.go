package project

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// mainWithWorktree makes a main checkout bound to id and a sibling linked worktree
// without a binding of its own, as `git worktree add` leaves one when the binding is
// gitignored.
func mainWithWorktree(t *testing.T, id string) (main, wt string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main, wt = filepath.Join(base, "main"), filepath.Join(base, "feature")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, main, "init", "-q", "-b", "main")
	git(t, main, "-c", "user.email=dev@example.com", "-c", "user.name=Dev", "commit", "-q", "--allow-empty", "-m", "init")
	git(t, main, "worktree", "add", "-q", wt)
	writeID(t, main, id)
	return main, wt
}

func TestResolveFollowsALinkedWorktreeToItsMainCheckout(t *testing.T) {
	main, wt := mainWithWorktree(t, "proj-main")
	if _, err := Load(wt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("precondition: the worktree has no binding of its own (%v)", err)
	}
	f, from, err := Resolve(wt, "")
	if err != nil || f.Project.ID != "proj-main" || from != main {
		t.Fatalf("Resolve = %+v from %q, %v", f, from, err)
	}
	sub := filepath.Join(wt, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f, root, err := ResolveDir(sub)
	if err != nil || f.Project.ID != "proj-main" || root != wt {
		t.Fatalf("ResolveDir = %+v at %q, %v", f, root, err)
	}

	// The worktree's own binding wins.
	writeID(t, wt, "proj-own")
	if f, from, err := Resolve(wt, ""); err != nil || f.Project.ID != "proj-own" || from != wt {
		t.Fatalf("own binding: %+v from %q, %v", f, from, err)
	}
}

// Only git's link counts: a separate repository inside a bound one is not its worktree.
func TestResolveDoesNotInheritThroughNesting(t *testing.T) {
	main, _ := mainWithWorktree(t, "proj-main")
	nested := filepath.Join(main, "vendor", "other")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, nested, "init", "-q")
	if _, _, err := Resolve(nested, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a nested repository inherited its parent's binding: %v", err)
	}
	// And a main checkout with no binding has nowhere to fall back to.
	if err := os.Remove(Path(main)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resolve(main, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unbound main: %v", err)
	}
}

// A broken main binding is reported, not mistaken for no binding at all.
func TestResolveReportsABrokenMainBinding(t *testing.T) {
	main, wt := mainWithWorktree(t, "proj-main")
	writeRaw(t, main, []byte("{not json"))
	if _, _, err := Resolve(wt, ""); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("broken main binding: %v", err)
	}
}
