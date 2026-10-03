package globalmode

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The hook scripts run under any POSIX sh: each one calls terma, then the hook it chains to,
// whether that is the clone's own .git/hooks or a hooks path terma replaced. CI sets
// TERMA_SHIM_SHELLS to dash and busybox; locally the test uses sh.
func TestGitHookScriptsRunUnderPOSIXShells(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake terma and chained hooks are sh scripts")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	shells := []string{"sh"}
	if s := os.Getenv("TERMA_SHIM_SHELLS"); s != "" {
		shells = strings.Split(s, ",")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	marks := t.TempDir()
	mark := func(who string) string {
		return "#!/bin/sh\necho \"$@\" > '" + filepath.Join(marks, who) + "'\n"
	}
	terma := filepath.Join(t.TempDir(), "terma")
	writeExec(t, terma, mark("terma"))

	for _, sh := range shells {
		for _, previous := range []bool{false, true} {
			t.Run(sh+map[bool]string{false: "/own-hooks", true: "/previous-path"}[previous], func(t *testing.T) {
				root := t.TempDir()
				git(t, "init", "-q", root)
				chained := filepath.Join(root, ".git", "hooks")
				prev := ""
				if previous {
					prev = t.TempDir()
					chained = prev
				}
				hooks := t.TempDir()
				for hook := range termaGitHooks {
					writeExec(t, filepath.Join(hooks, hook), globalGitHookScript(hook, terma, prev))
					writeExec(t, filepath.Join(chained, hook), mark("chained-"+hook))
				}
				for hook := range termaGitHooks {
					for _, f := range []string{"terma", "chained-" + hook} {
						_ = os.Remove(filepath.Join(marks, f))
					}
					args := append(strings.Fields(sh), filepath.Join(hooks, hook), "msg.txt", "message")
					cmd := exec.Command(args[0], args[1:]...)
					cmd.Dir = root
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("%s %s: %v\n%s", sh, hook, err, out)
					}
					if got := readMark(t, filepath.Join(marks, "terma")); got != "hook "+hook+" msg.txt message" {
						t.Errorf("%s: terma got %q", hook, got)
					}
					if got := readMark(t, filepath.Join(marks, "chained-"+hook)); got != "msg.txt message" {
						t.Errorf("%s: the chained hook got %q", hook, got)
					}
				}
			})
		}
	}
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

func readMark(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("not run: %v", err)
	}
	return strings.TrimSpace(string(b))
}
