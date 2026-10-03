package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// unboundRepo is a git repository with git's global config its own.
func unboundRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	root := t.TempDir()
	git(t, root, "init", "-q")
	return root
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func hookDir(t *testing.T, dir string, hooks ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range hooks {
		if err := os.WriteFile(filepath.Join(dir, h), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Doctor reads where git looks for a repository's hooks: terma's global ones, a local
// setting that outranks them, or one left pointing at no hooks, which disables them all.
func TestDoctorJudgesARepositorysHooksPath(t *testing.T) {
	for name, tc := range map[string]struct {
		setup  func(t *testing.T, root string)
		status Status
		want   string
	}{
		"missing directory": {func(t *testing.T, root string) {
			git(t, root, "config", "core.hooksPath", "gone")
		}, Warn, "git config --local --unset core.hooksPath"},
		"only git's samples": {func(t *testing.T, root string) {
			git(t, root, "config", "core.hooksPath", hookDir(t, filepath.Join(root, "hooks"), "pre-commit.sample"))
		}, Warn, "git config --local --unset core.hooksPath"},
		"terma's global hooks": {func(t *testing.T, root string) {
			dir := hookDir(t, filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "git-hooks"), "prepare-commit-msg")
			git(t, root, "config", "--global", "core.hooksPath", dir)
		}, Pass, ""},
		"local hooks outrank terma's": {func(t *testing.T, root string) {
			dir := hookDir(t, filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "git-hooks"), "prepare-commit-msg")
			git(t, root, "config", "--global", "core.hooksPath", dir)
			git(t, root, "config", "core.hooksPath", hookDir(t, filepath.Join(root, "hooks"), "pre-commit"))
		}, Warn, "git config --local --unset core.hooksPath"},
		"terma's global hooks not set": {func(*testing.T, string) {}, Warn, "terma setup"},
	} {
		t.Run(name, func(t *testing.T) {
			root := unboundRepo(t)
			tc.setup(t, root)
			c := HooksPathCheck(JudgeHooksPath(t.Context(), root))
			if c.Status != tc.status || !strings.Contains(c.Fix, tc.want) {
				t.Fatalf("hooks = %+v, want %s with fix %q", c, tc.status, tc.want)
			}
		})
	}
}

// pre-commit refuses to install under a global hooks path: a repository that configures it
// with no pre-commit hook of its own is told how to install one terma's hooks will run.
func TestDoctorHintsAtInstallingPreCommit(t *testing.T) {
	root := unboundRepo(t)
	git(t, root, "config", "--global", "core.hooksPath", hookDir(t, filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "git-hooks"), "prepare-commit-msg"))
	d := &run{ctx: t.Context(), env: Env{Root: root, GitDir: filepath.Join(root, ".git")}}
	if c := d.commitHooks(); c.Status != Pass {
		t.Fatalf("without pre-commit: %+v", c)
	}
	if err := os.WriteFile(filepath.Join(root, ".pre-commit-config.yaml"), []byte("repos: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := d.commitHooks(); c.Status != Warn || !strings.HasPrefix(c.Fix, "GIT_CONFIG_GLOBAL=/dev/null pre-commit install") {
		t.Fatalf("pre-commit not installed: %+v", c)
	}
	hookDir(t, filepath.Join(root, ".git", "hooks"), "pre-commit")
	if c := d.commitHooks(); c.Status != Pass {
		t.Fatalf("pre-commit installed: %+v", c)
	}
}
