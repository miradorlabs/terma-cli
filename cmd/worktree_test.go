package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// doctor says when a linked worktree is bound through its main checkout, and names both
// places when neither has a binding.
func TestDoctorNamesTheMainCheckoutForAWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main, wt := filepath.Join(base, "main"), filepath.Join(base, "feature")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=dev@example.com", "-c", "user.name=Dev", "commit", "-q", "--allow-empty", "-m", "init"},
		{"worktree", "add", "-q", wt},
	} {
		c := exec.Command("git", args...)
		c.Dir, c.Env = main, append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Chdir(wt)
	root, gitDir, err := repoHere(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	d := &doctorRun{root: root, gitDir: gitDir}
	if c := d.repositoryBound(); c.Status != doctor.Fail || !strings.Contains(c.Detail, "or its main checkout "+main) {
		t.Fatalf("unbound worktree: %+v", c)
	}
	if err := termaproject.Save(main, &termaproject.File{Project: termaproject.Project{ID: testProjectID, Name: "Main"}}); err != nil {
		t.Fatal(err)
	}
	if c := d.repositoryBound(); c.Status != doctor.Pass || c.Detail != "Main (through the main checkout "+main+")" {
		t.Fatalf("bound through main: %+v", c)
	}
}
