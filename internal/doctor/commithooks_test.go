package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/repohooks"
)

// unboundRepo is a git repository with git's global config its own.
func unboundRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	root := t.TempDir()
	git(t, root, "init", "-q")
	git(t, root, "remote", "add", "origin", "git@github.com:acme/repo.git")
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

// stamping is a validated policy listing the repository the tests build.
func stamping() config.Policy {
	return config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/repo"},
		GitHooks: true, TeamID: "team_1", FetchedAt: time.Now()}
}

// Doctor says, for this repository alone, what terma's commit hooks are doing: installed,
// installed over a hook that keeps running, skipped for a repository with its own hooks
// path, waiting for the first agent session, not collected, or off by policy.
func TestDoctorJudgesThisRepositorysCommitHooks(t *testing.T) {
	for name, tc := range map[string]struct {
		policy config.Policy
		setup  func(t *testing.T, root, configDir string)
		want   CommitHooks
		status Status
		fix    string
	}{
		"waiting for the first session": {stamping(), nil, CommitHooksNotYet, Skip, ""},
		"installed": {stamping(), func(t *testing.T, root, configDir string) {
			install(t, configDir, root)
		}, CommitHooksInstalled, Pass, ""},
		"installed over a hook that keeps running": {stamping(), func(t *testing.T, root, configDir string) {
			hookDir(t, filepath.Join(root, ".git", "hooks"), "prepare-commit-msg")
			install(t, configDir, root)
		}, CommitHooksChained, Pass, ""},
		"its own hooks path": {stamping(), func(t *testing.T, root, _ string) {
			git(t, root, "config", "core.hooksPath", ".husky/_")
		}, CommitHooksOwnPath, Warn, "core.hooksPath"},
		"another tool's hook over terma's, with the one set aside still there": {stamping(), func(t *testing.T, root, configDir string) {
			hooks := filepath.Join(root, ".git", "hooks")
			hookDir(t, hooks, "prepare-commit-msg")
			install(t, configDir, root)
			if err := os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"), []byte("#!/bin/sh\necho another tool\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, CommitHooksTaken, Warn, ".pre-terma"},
		// A hooks path set after terma's hooks went in keeps git from running them (#123).
		"installed, then a hooks path set here": {stamping(), func(t *testing.T, root, configDir string) {
			install(t, configDir, root)
			git(t, root, "config", "core.hooksPath", ".husky/_")
		}, CommitHooksOwnPath, Warn, "git config --unset core.hooksPath"},
		"installed, then a hooks path set globally": {stamping(), func(t *testing.T, root, configDir string) {
			install(t, configDir, root)
			git(t, root, "config", "--global", "core.hooksPath", filepath.Join(t.TempDir(), "hooks"))
		}, CommitHooksGlobalPath, Warn, "git config --global --unset core.hooksPath"},
		"a hooks path naming .git/hooks itself": {stamping(), func(t *testing.T, root, configDir string) {
			install(t, configDir, root)
			git(t, root, "config", "core.hooksPath", ".git/hooks")
		}, CommitHooksInstalled, Pass, ""},
		// pre-commit, then terma, then pre-commit again: each runs the other (#124).
		"pre-commit's migration loop": {stamping(), func(t *testing.T, root, configDir string) {
			hooks := filepath.Join(root, ".git", "hooks")
			preCommit(t, hooks)
			install(t, configDir, root)
			preCommit(t, hooks)
		}, CommitHooksLooping, Warn, "pre-commit install -f"},
		// The developer's own hook, then terma, then pre-commit: pre-commit runs terma's from
		// .legacy, which runs the developer's (#125).
		"pre-commit chained over the developer's own hook": {stamping(), func(t *testing.T, root, configDir string) {
			hooks := filepath.Join(root, ".git", "hooks")
			hookDir(t, hooks, "prepare-commit-msg")
			install(t, configDir, root)
			preCommit(t, hooks)
		}, CommitHooksChained, Pass, ""},
		"not collected": {func() config.Policy {
			p := stamping()
			p.Repositories = []string{"github.com/acme/other"}
			return p
		}(), nil, CommitHooksUnadmitted, Skip, ""},
		"off by policy": {func() config.Policy {
			p := stamping()
			p.GitHooks = false
			return p
		}(), nil, CommitHooksOff, Skip, ""},
		"no policy at all": {config.NoPolicy("org", "https://auth.example"), nil, CommitHooksOff, Skip, ""},
	} {
		t.Run(name, func(t *testing.T) {
			root, configDir := unboundRepo(t), t.TempDir()
			if tc.setup != nil {
				tc.setup(t, root, configDir)
			}
			got := JudgeCommitHooks(filepath.Join(root, ".git"), tc.policy, time.Now())
			if got != tc.want {
				t.Fatalf("verdict = %d, want %d", got, tc.want)
			}
			c := CommitHooksCheck(got)
			if c.Status != tc.status || !strings.Contains(c.Fix, tc.fix) {
				t.Fatalf("check = %+v, want %s with a fix naming %q", c, tc.status, tc.fix)
			}
		})
	}
}

// preCommit is `pre-commit install -t prepare-commit-msg`: a hook it did not write moves to
// .legacy, which its own script then runs.
func preCommit(t *testing.T, hooks string) {
	t.Helper()
	path := filepath.Join(hooks, "prepare-commit-msg")
	const script = "#!/bin/sh\n# File generated by pre-commit: https://pre-commit.com\n[ -x \"$0.legacy\" ] && \"$0.legacy\" \"$@\"\nexit 0\n"
	if data, err := os.ReadFile(path); err == nil && string(data) != script {
		if err := os.Rename(path, path+".legacy"); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func install(t *testing.T, configDir, root string) {
	t.Helper()
	if _, err := repohooks.Install(configDir, filepath.Join(t.TempDir(), "terma"), filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
}

// A policy that has not been refreshed for over a week stamps nothing, as it records
// nothing, however many hooks were installed under it before.
func TestDoctorReportsCommitHooksOffUnderAnExpiredPolicy(t *testing.T) {
	root := unboundRepo(t)
	policy := stamping()
	policy.FetchedAt = time.Now().Add(-8 * 24 * time.Hour)
	if got := JudgeCommitHooks(filepath.Join(root, ".git"), policy, time.Now()); got != CommitHooksOff {
		t.Fatalf("verdict = %d, want off", got)
	}
}

// The checks doctor runs agree with what `terma status` prints, from the same judgement.
func TestDoctorsCommitHooksCheckUsesThePolicyInForce(t *testing.T) {
	root, configDir := unboundRepo(t), t.TempDir()
	install(t, configDir, root)
	d := &run{ctx: t.Context(), env: Env{ConfigDir: configDir, Root: root, GitDir: filepath.Join(root, ".git")}, pol: stamping()}
	if c := d.commitHooks(); c.Status != Pass || !strings.Contains(c.Detail, ".git/hooks") {
		t.Fatalf("installed: %+v", c)
	}
	d.nonGit = true
	if c := d.commitHooks(); c.Status != Skip {
		t.Fatalf("outside git: %+v", c)
	}
}
