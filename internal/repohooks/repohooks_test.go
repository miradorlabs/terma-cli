package repohooks

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/gitx"
)

const terma = "/opt/terma/bin/terma"

// scratch is an empty git repository with git's global config its own.
func scratch(t *testing.T) (root, gitDir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	return initRepo(t, t.TempDir())
}

// initRepo makes dir a repository with symlinks resolved, as a linked worktree's gitfile
// names it (macOS puts temporary directories under one).
func initRepo(t *testing.T, dir string) (root, gitDir string) {
	t.Helper()
	root = resolve(t, dir)
	git(t, root, "init", "-q")
	git(t, root, "config", "user.email", "dev@example.com")
	git(t, root, "config", "user.name", "Dev")
	git(t, root, "config", "commit.gpgsign", "false")
	return root, filepath.Join(root, ".git")
}

func resolve(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func hookPath(gitDir, name string) string { return filepath.Join(gitDir, hooksDir, name) }

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The install state machine: nothing, terma's own at this version, terma's own at an
// older one, someone else's, and someone else's written over terma's again.
func TestInstallWritesTermasHooksAndChainsToAnyOthers(t *testing.T) {
	_, gitDir := scratch(t)
	stateDir := t.TempDir()

	// Nothing there: both hooks are written, and the repository is recorded.
	changed, err := Install(stateDir, terma, gitDir)
	if err != nil || !changed {
		t.Fatalf("Install = %v, %v", changed, err)
	}
	for _, name := range names {
		info, err := os.Stat(hookPath(gitDir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable (%v)", name, info.Mode())
		}
		body := readFile(t, hookPath(gitDir, name))
		if !isOurs([]byte(body)) || !strings.Contains(body, "hook "+name) || !strings.Contains(body, terma) {
			t.Errorf("%s does not call terma under a marker:\n%s", name, body)
		}
		if _, err := os.Lstat(hookPath(gitDir, name+preTermaSuffix)); err == nil {
			t.Errorf("%s: a chained hook was invented out of nothing", name)
		}
	}
	// terma's own, at this version: nothing is written.
	if changed, err := Install(stateDir, terma, gitDir); err != nil || changed {
		t.Fatalf("a second install = %v, %v", changed, err)
	}
	if installed, chained := Installed(gitDir); !installed || chained {
		t.Fatalf("Installed = %v, %v, want installed and unchained", installed, chained)
	}

	// terma's own, from an earlier template or for a terma that has moved: rewritten in
	// place, never chained to itself.
	old := "#!/bin/sh\n" + marker + "0: an earlier template\nexit 0\n"
	writeExec(t, hookPath(gitDir, "post-commit"), old)
	if changed, err := Install(stateDir, terma, gitDir); err != nil || !changed {
		t.Fatalf("install over an older script = %v, %v", changed, err)
	}
	if body := readFile(t, hookPath(gitDir, "post-commit")); body == old || !strings.Contains(body, terma) {
		t.Errorf("an older script was not rewritten:\n%s", body)
	}
	if _, err := os.Lstat(hookPath(gitDir, "post-commit"+preTermaSuffix)); err == nil {
		t.Error("terma's own older script was chained to itself")
	}

	// Someone else's: it moves to the name terma's script runs.
	theirs := "#!/bin/sh\necho theirs\n"
	writeExec(t, hookPath(gitDir, "prepare-commit-msg"), theirs)
	if changed, err := Install(stateDir, terma, gitDir); err != nil || !changed {
		t.Fatalf("install beside another hook = %v, %v", changed, err)
	}
	if got := readFile(t, hookPath(gitDir, "prepare-commit-msg"+preTermaSuffix)); got != theirs {
		t.Errorf("the displaced hook = %q, want %q", got, theirs)
	}
	if installed, chained := Installed(gitDir); !installed || !chained {
		t.Fatalf("Installed = %v, %v, want installed and chained", installed, chained)
	}

	// That tool reinstalls itself over terma's script, an identical copy: it replaces the
	// one set aside, and terma chains to it as before.
	writeExec(t, hookPath(gitDir, "prepare-commit-msg"), theirs)
	if changed, err := Install(stateDir, terma, gitDir); err != nil || !changed {
		t.Fatalf("install over a reinstalled hook = %v, %v", changed, err)
	}
	if got := readFile(t, hookPath(gitDir, "prepare-commit-msg"+preTermaSuffix)); got != theirs {
		t.Errorf("the displaced hook = %q, want %q", got, theirs)
	}
	if Taken(gitDir) {
		t.Error("a hook manager's identical reinstall was taken for another tool's")
	}

	// A different hook over terma's, with the one set aside still there: terma deletes
	// neither and writes nothing, not even the other hook name.
	newer := "#!/bin/sh\necho another tool\n"
	writeExec(t, hookPath(gitDir, "prepare-commit-msg"), newer)
	writeExec(t, hookPath(gitDir, "post-commit"), old)
	if _, err := Install(stateDir, terma, gitDir); !errors.Is(err, ErrTaken) {
		t.Fatalf("install over a different hook = %v, want ErrTaken", err)
	}
	if got := readFile(t, hookPath(gitDir, "prepare-commit-msg"+preTermaSuffix)); got != theirs {
		t.Errorf("the hook terma set aside = %q, want it kept: %q", got, theirs)
	}
	if got := readFile(t, hookPath(gitDir, "prepare-commit-msg")); got != newer {
		t.Errorf("prepare-commit-msg = %q, want the other tool's %q left alone", got, newer)
	}
	if got := readFile(t, hookPath(gitDir, "post-commit")); got != old {
		t.Error("a taken repository had its other hook rewritten")
	}
	if !Taken(gitDir) {
		t.Error("Taken = false for a repository Install refused")
	}
}

// A hook that is not terma's is never taken for one, whatever it holds.
func TestOnlyTermasOwnScriptCarriesTheMarker(t *testing.T) {
	t.Parallel()
	ours := script("post-commit", terma)
	if !isOurs([]byte(ours)) {
		t.Fatalf("terma's own script is not recognized:\n%s", ours)
	}
	if lines := strings.SplitN(ours, "\n", 3); !strings.HasPrefix(lines[1], marker) {
		t.Errorf("the marker is not on line 2:\n%s", ours)
	}
	for name, body := range map[string]string{
		"empty":                   "",
		"shebang only":            "#!/bin/sh\n",
		"the marker further down": "#!/bin/sh\necho hi\n" + marker + "1: not where terma writes it\n",
		"a mention in prose":      "#!/bin/sh\n# this one is not " + marker + "1\n",
		"a hook manager's":        "#!/usr/bin/env sh\n. \"$(dirname -- \"$0\")/_/h\"\n",
		// A compiled hook is read no further than the bound, so nothing large is rewritten
		// however it begins.
		"something large that opens like ours": ours + strings.Repeat("x", maxOurScript),
	} {
		if isOurs([]byte(body)) {
			t.Errorf("%s was taken for terma's own:\n%s", name, body)
		}
	}
}

// A hook too large to be terma's is moved aside whole, never read past the bound and
// never rewritten.
func TestALargeHookIsChainedToWhole(t *testing.T) {
	_, gitDir := scratch(t)
	stateDir := t.TempDir()
	theirs := "#!/bin/sh\n# " + strings.Repeat("compiled-ish ", 2000) + "\n"
	writeExec(t, hookPath(gitDir, "post-commit"), theirs)
	if _, err := Install(stateDir, terma, gitDir); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, hookPath(gitDir, "post-commit"+preTermaSuffix)); got != theirs {
		t.Errorf("the displaced hook is %d bytes, want the whole %d", len(got), len(theirs))
	}
	// And a second install leaves it be, though it is too large to read whole.
	if changed, err := Install(stateDir, terma, gitDir); err != nil || changed {
		t.Fatalf("a second install = %v, %v", changed, err)
	}
	if got := readFile(t, hookPath(gitDir, "post-commit"+preTermaSuffix)); got != theirs {
		t.Error("a second install touched the large hook")
	}
}

// One install in the common git directory serves every linked worktree, which is where
// git looks for a worktree's hooks.
func TestALinkedWorktreeSharesOneInstall(t *testing.T) {
	root, gitDir := scratch(t)
	stateDir := t.TempDir()
	git(t, root, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, root, "worktree", "add", "-q", wt)
	_, wtGitDir, ok := gitx.LocateFS(wt)
	if !ok {
		t.Fatalf("no git directory for the worktree at %s", wt)
	}
	if changed, err := Install(stateDir, terma, wtGitDir); err != nil || !changed {
		t.Fatalf("Install from the worktree = %v, %v", changed, err)
	}
	for _, name := range names {
		if _, err := os.Stat(hookPath(gitDir, name)); err != nil {
			t.Errorf("%s is not in the common git directory: %v", name, err)
		}
		if _, err := os.Stat(hookPath(wtGitDir, name)); err == nil {
			t.Errorf("%s was written into the worktree's own git directory too", name)
		}
	}
	if installed, _ := Installed(wtGitDir); !installed {
		t.Error("the worktree does not see the install")
	}
}

// Sessions starting at once in a repository with a hook of its own: exactly one sets it
// aside, and it is never replaced by terma's script.
func TestConcurrentInstallsSetTheHookAsideOnce(t *testing.T) {
	for range 20 {
		_, gitDir := scratch(t)
		stateDir := t.TempDir()
		theirs := "#!/bin/sh\necho theirs\n"
		writeExec(t, hookPath(gitDir, "prepare-commit-msg"), theirs)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				if _, err := Install(stateDir, terma, gitDir); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if got := readFile(t, hookPath(gitDir, "prepare-commit-msg"+preTermaSuffix)); got != theirs {
			t.Fatalf("the hook set aside = %q, want %q", got, theirs)
		}
	}
}

// A repository with the two commit hooks an earlier terma wrote still reads as installed,
// so doctor keeps reporting hooks that stamp even where the policy no longer asks for them;
// the next install adds pre-push and rewrites neither of the others.
func TestAnInstallFromBeforePrePushIsStillInstalled(t *testing.T) {
	_, gitDir := scratch(t)
	for _, name := range commitHooks {
		writeExec(t, hookPath(gitDir, name), script(name, terma))
	}
	if installed, chained := Installed(gitDir); !installed || chained {
		t.Fatalf("Installed = %v, %v; want the commit hooks installed, unchained", installed, chained)
	}
	if current(gitDir, terma) {
		t.Fatal("an install without pre-push reads as current, so no session would add it")
	}
	before := map[string]os.FileInfo{}
	for _, name := range commitHooks {
		before[name], _ = os.Stat(hookPath(gitDir, name))
	}
	if changed, err := Install(t.TempDir(), terma, gitDir); err != nil || !changed {
		t.Fatalf("Install = %v, %v", changed, err)
	}
	if body := readFile(t, hookPath(gitDir, "pre-push")); !isOurs([]byte(body)) {
		t.Fatalf("pre-push not added:\n%s", body)
	}
	for _, name := range commitHooks {
		if after, _ := os.Stat(hookPath(gitDir, name)); !os.SameFile(before[name], after) || !after.ModTime().Equal(before[name].ModTime()) {
			t.Errorf("%s was rewritten", name)
		}
	}
}
