package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// userSandbox also leaves the repository the test binary was built in, so status reads
// nothing of it.
func userSandbox(t *testing.T) string {
	t.Helper()
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	useConfigDir(t, t.TempDir())
	t.Chdir(t.TempDir())
	fakeClaudeOnPath(t)
	return filepath.Join(claudeDir, "settings.json")
}

// useConfigDir points testApp, and the agents it registers, at the config directory dir and
// a state directory of its own until the test ends, as Execute does with the ones it
// resolves. The two are kept apart, so state written to the config directory fails a test.
func useConfigDir(t *testing.T, dir string) {
	t.Helper()
	useDirs(t, dir, t.TempDir())
}

// useDirs points testApp at the config directory configDir and the state directory stateDir
// until the test ends.
func useDirs(t *testing.T, configDir, stateDir string) {
	t.Helper()
	prevConfig, prevState := testApp.dir, testApp.stateDir
	testApp.configure(configDir, stateDir)
	t.Cleanup(func() { testApp.configure(prevConfig, prevState) })
}

const testServerKey = "ter_srv_0123456789abcdef"

// fakeClaudeOnPath makes these tests independent of which agents the machine has
// installed: status and doctor report only a harness they can find.
func fakeClaudeOnPath(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs an executable shim on PATH")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho '2.1.0 (Claude Code)'\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(git, filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

const testProjectID = "770e8400-e29b-41d4-a716-446655440000"

// gitRepo is a fresh git repository, the working directory, with Terma's directory and
// Claude's sandboxed.
func gitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", repo}, {"-C", repo, "config", "user.email", "dev@example.com"}, {"-C", repo, "config", "user.name", "Dev"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	useConfigDir(t, t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Chdir(repo)
	realDir, _ := filepath.EvalSymlinks(repo)
	return realDir
}
