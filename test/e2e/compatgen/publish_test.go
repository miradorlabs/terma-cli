package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The nightly job's git steps, run as it runs them on two nights in a row, each a fresh
// checkout of main: the first creates compat-matrix, the second starts from what the first
// published and publishes over it. The second is the one that broke: the restored catalog sat
// untracked in the way of the checkout.
func TestPublishTwoNights(t *testing.T) {
	for _, tool := range []string{"git", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	script, err := filepath.Abs("publish.sh")
	if err != nil {
		t.Fatal(err)
	}
	ignore, err := os.ReadFile("../../../.gitignore")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	tmp := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(dir, name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	origin := filepath.Join(tmp, "origin.git")
	run(tmp, "git", "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(tmp, "seed")
	run(tmp, "git", "clone", "-q", origin, seed)
	write(seed, ".gitignore", string(ignore))
	write(seed, "README.md", "main\n")
	run(seed, "git", "add", ".")
	run(seed, "git", "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "main")
	run(seed, "git", "push", "-q", "origin", "main")

	generated := []string{"docs/COMPATIBILITY.md", "docs/FIELDS.md", "docs/compat/compat.json", "docs/compat/history.json", "docs/compat/fields.json"}
	for night := 1; night <= 2; night++ {
		wt := filepath.Join(tmp, "night"+string(rune('0'+night)))
		run(tmp, "git", "clone", "-q", "-b", "main", origin, wt)
		run(wt, "bash", script, "restore")
		if night == 2 {
			// What the first night published, back in place and ignored, as the job renders over it.
			for _, f := range []string{"docs/FIELDS.md", "docs/compat/history.json", "docs/compat/fields.json"} {
				if b, err := os.ReadFile(filepath.Join(wt, f)); err != nil || string(b) != "night 1\n" {
					t.Fatalf("night 2 restored %s as %q (%v)", f, b, err)
				}
			}
		}
		for _, f := range generated {
			write(wt, f, "night "+string(rune('0'+night))+"\n")
		}
		if s := run(wt, "git", "status", "--porcelain"); s != "" {
			t.Fatalf("night %d's generated files are not ignored on main:\n%s", night, s)
		}
		run(wt, "bash", script, "publish")
	}

	check := filepath.Join(tmp, "check")
	run(tmp, "git", "clone", "-q", "-b", "compat-matrix", origin, check)
	if n := run(check, "git", "rev-list", "--count", "HEAD"); n != "2" {
		t.Errorf("compat-matrix has %s commits, want one a night", n)
	}
	for _, f := range generated {
		if b, err := os.ReadFile(filepath.Join(check, f)); err != nil || string(b) != "night 2\n" {
			t.Errorf("compat-matrix has %s as %q (%v), want night 2's", f, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(check, "README.md")); err == nil {
		t.Error("compat-matrix carries main's files")
	}
}
