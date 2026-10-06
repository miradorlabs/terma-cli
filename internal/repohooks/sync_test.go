package repohooks

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

var now = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// collecting is a validated policy of the given mode that stamps commits.
func collecting(mode string, repositories ...string) config.Policy {
	return config.Policy{Mode: mode, Repositories: repositories, GitHooks: true,
		TeamID: "team_1", FetchedAt: now.Add(-time.Minute)}
}

// A claimed session installs terma's hooks only where the policy asks for them.
func TestSyncInstallsOnlyWhereThePolicyAsks(t *testing.T) {
	listed := collecting(config.ModeRepo, "github.com/acme/repo")
	for name, tc := range map[string]struct {
		policy config.Policy
		// prepare returns the directory the session ran in.
		prepare func(t *testing.T, root, gitDir string) string
		want    bool
	}{
		"a listed repository": {listed, nil, true},
		"every repository, in global mode": {collecting(config.ModeGlobal), func(t *testing.T, root, gitDir string) string {
			git(t, root, "remote", "remove", "origin")
			return root
		}, true},
		"a subdirectory of one": {listed, func(t *testing.T, root, _ string) string {
			dir := filepath.Join(root, "src", "deep")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return dir
		}, true},
		"git_hooks off":       {func() config.Policy { p := listed; p.GitHooks = false; return p }(), nil, false},
		"no validated policy": {config.NoPolicy("org", "https://auth.example"), nil, false},
		"a policy not refreshed for over a week": {func() config.Policy {
			p := listed
			p.FetchedAt = now.Add(-8 * 24 * time.Hour)
			return p
		}(), nil, false},
		"a repository the list does not name": {collecting(config.ModeRepo, "github.com/acme/other"), nil, false},
		"a plain folder in global mode": {collecting(config.ModeGlobal), func(t *testing.T, _, _ string) string {
			return t.TempDir()
		}, false},
		"a repository with its own hooks path": {listed, func(t *testing.T, root, _ string) string {
			git(t, root, "config", "core.hooksPath", ".husky/_")
			return root
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			root, gitDir := scratch(t)
			git(t, root, "remote", "add", "origin", "git@github.com:acme/repo.git")
			stateDir := t.TempDir()
			dir := root
			if tc.prepare != nil {
				dir = tc.prepare(t, root, gitDir)
			}
			if _, err := Sync(stateDir, terma, tc.policy, now, dir); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			installed, _ := Installed(gitDir)
			if installed != tc.want {
				t.Fatalf("installed = %v, want %v", installed, tc.want)
			}
		})
	}
}

// A policy switched off adds no hooks anywhere new and leaves the ones it added: hooks
// never come and go with the policy.
func TestAPolicySwitchedOffLeavesTheHooksItAdded(t *testing.T) {
	root, gitDir := scratch(t)
	git(t, root, "remote", "add", "origin", "git@github.com:acme/repo.git")
	stateDir := t.TempDir()
	on := collecting(config.ModeRepo, "github.com/acme/repo")
	if changed, err := Sync(stateDir, terma, on, now, root); err != nil || !changed {
		t.Fatalf("Sync(on) = %v, %v", changed, err)
	}
	off := on
	off.GitHooks = false
	unlisted := on
	unlisted.Repositories = []string{"github.com/acme/other"}
	for name, p := range map[string]config.Policy{"git_hooks off": off, "repository unlisted": unlisted} {
		if changed, err := Sync(stateDir, terma, p, now, root); err != nil || changed {
			t.Fatalf("Sync(%s) = %v, %v", name, changed, err)
		}
		if installed, _ := Installed(gitDir); !installed {
			t.Fatalf("Sync(%s) took the hooks out", name)
		}
	}
}

// A repository terma never installed into is never touched, whatever the policy says: a
// repository with its own hooks path keeps its own hooks and gets no .pre-terma.
func TestSyncLeavesARepositoryItNeverInstalledIntoAlone(t *testing.T) {
	root, gitDir := scratch(t)
	git(t, root, "remote", "add", "origin", "git@github.com:acme/repo.git")
	git(t, root, "config", "core.hooksPath", ".husky/_")
	stateDir := t.TempDir()
	theirs := "#!/bin/sh\necho theirs\n"
	writeExec(t, hookPath(gitDir, "prepare-commit-msg"), theirs)
	if changed, err := Sync(stateDir, terma, collecting(config.ModeRepo, "github.com/acme/repo"), now, root); err != nil || changed {
		t.Fatalf("Sync = %v, %v", changed, err)
	}
	if got := readFile(t, hookPath(gitDir, "prepare-commit-msg")); got != theirs {
		t.Errorf("prepare-commit-msg = %q, want untouched", got)
	}
	if !OwnHooksPath(gitDir) {
		t.Error("the repository's own hooks path was not seen")
	}
}

// A core.hooksPath in git's global config means git never reads .git/hooks, so nothing
// is installed there.
func TestSyncSkipsARepositoryUnderAGlobalHooksPath(t *testing.T) {
	root, gitDir := scratch(t)
	git(t, root, "config", "--global", "core.hooksPath", filepath.Join(t.TempDir(), "hooks"))
	if !OwnHooksPath(gitDir) {
		t.Fatal("OwnHooksPath = false under a global core.hooksPath")
	}
}
