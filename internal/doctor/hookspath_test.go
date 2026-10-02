package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// unboundRepo is a git repository bound to nothing, with git's global config its own.
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

// Doctor reads where git looks for an unbound repository's hooks: a setting left pointing at
// no hooks disables them all, and in global mode a local one outranks terma's.
func TestDoctorJudgesAnUnboundRepositorysHooksPath(t *testing.T) {
	for name, tc := range map[string]struct {
		setup  func(t *testing.T, root string)
		global bool
		status Status
		want   string
	}{
		"stale terma shims": {func(t *testing.T, root string) {
			hookDir(t, filepath.Join(root, hookmgr.ShimDir))
			git(t, root, "config", "extensions.worktreeConfig", "true")
			git(t, root, "config", "--worktree", "core.hooksPath", hookmgr.ShimDir)
		}, true, Warn, "terma uninstall"},
		"missing directory": {func(t *testing.T, root string) {
			git(t, root, "config", "core.hooksPath", "gone")
		}, false, Warn, "git config --local --unset core.hooksPath"},
		"per-repository mode, nothing set": {func(*testing.T, string) {}, false, Skip, ""},
		"per-repository mode, working local hooks": {func(t *testing.T, root string) {
			git(t, root, "config", "core.hooksPath", hookDir(t, filepath.Join(root, "hooks"), "pre-commit"))
		}, false, Skip, ""},
		"global mode, terma's global hooks": {func(t *testing.T, root string) {
			dir := hookDir(t, filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "git-hooks"), "prepare-commit-msg")
			git(t, root, "config", "--global", "core.hooksPath", dir)
		}, true, Pass, ""},
		"global mode, local hooks outrank terma's": {func(t *testing.T, root string) {
			dir := hookDir(t, filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), "git-hooks"), "prepare-commit-msg")
			git(t, root, "config", "--global", "core.hooksPath", dir)
			git(t, root, "config", "core.hooksPath", hookDir(t, filepath.Join(root, "hooks"), "pre-commit"))
		}, true, Warn, "terma install"},
		"global mode, terma's global hooks not set": {func(*testing.T, string) {}, true, Warn, "terma setup"},
	} {
		t.Run(name, func(t *testing.T) {
			root := unboundRepo(t)
			tc.setup(t, root)
			c := UnboundHooksCheck(JudgeHooksPath(t.Context(), root), tc.global)
			if c.Status != tc.status || !strings.Contains(c.Fix, tc.want) {
				t.Fatalf("hooks = %+v, want %s with fix %q", c, tc.status, tc.want)
			}
		})
	}
}

// The check runs in a repository doctor used to skip for having no binding.
func TestDoctorReportsStaleShimsInAnUnboundRepository(t *testing.T) {
	e := env(t, Probes{Credential: signedIn, Spool: func() SpoolState { return SpoolState{Open: true} }})
	if err := termaproject.Remove(e.Root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	hookDir(t, filepath.Join(e.Root, hookmgr.ShimDir))
	git(t, e.Root, "config", "core.hooksPath", hookmgr.ShimDir)
	e.Config.Policy = config.Policy{Mode: config.ModeGlobal}
	if c := check(Run(t.Context(), e, Progress{}), KeyHooks); c.Status != Warn || c.Fix != "terma uninstall" {
		t.Fatalf("commit hooks = %+v", c)
	}
}
