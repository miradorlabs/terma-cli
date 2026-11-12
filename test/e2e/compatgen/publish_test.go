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

	generated := []string{"docs/COMPATIBILITY.md", "docs/FIELDS.md", "docs/compat/compat.json", "docs/compat/history.json", "docs/compat/fields.json",
		"docs/harnesses/index.json", "docs/harnesses/claude/versions/1.0.0.json"}
	for night := 1; night <= 2; night++ {
		wt := filepath.Join(tmp, "night"+string(rune('0'+night)))
		run(tmp, "git", "clone", "-q", "-b", "main", origin, wt)
		run(wt, "bash", script, "restore")
		if night == 2 {
			// What the first night published, back in place and ignored, as the job renders over it.
			for _, f := range []string{"docs/FIELDS.md", "docs/compat/history.json", "docs/compat/fields.json", "docs/harnesses/index.json", "docs/harnesses/claude/diffs/0.9.0..1.0.0.json"} {
				if b, err := os.ReadFile(filepath.Join(wt, f)); err != nil || string(b) != "night 1\n" {
					t.Fatalf("night 2 restored %s as %q (%v)", f, b, err)
				}
			}
		}
		for _, f := range generated {
			write(wt, f, "night "+string(rune('0'+night))+"\n")
		}
		if night == 1 {
			// A diff only the first night writes: the second, which no longer does, takes it out.
			write(wt, "docs/harnesses/claude/diffs/0.9.0..1.0.0.json", "night 1\n")
		} else {
			if err := os.Remove(filepath.Join(wt, "docs/harnesses/claude/diffs/0.9.0..1.0.0.json")); err != nil {
				t.Fatal(err)
			}
		}
		if s := run(wt, "git", "status", "--porcelain"); s != "" {
			t.Fatalf("night %d's generated files are not ignored on main:\n%s", night, s)
		}
		run(wt, "bash", script, "publish")
	}

	// Between nights, the frequent check publishes a new build's reference alone.
	between := filepath.Join(tmp, "between")
	run(tmp, "git", "clone", "-q", "-b", "main", origin, between)
	run(between, "bash", script, "restore")
	write(between, "docs/harnesses/claude/versions/1.0.1.json", "between\n")
	write(between, "docs/compat/history.json", "not to be published\n")
	run(between, "bash", script, "publish-reference")

	// The Model Reference publishes its own tree, twice: the second run starts from the first's,
	// with the Harness Reference beside it to read, and changes nothing else.
	for i, content := range []string{"models 1\n", "models 2\n"} {
		models := filepath.Join(tmp, "models"+string(rune('1'+i)))
		run(tmp, "git", "clone", "-q", "-b", "main", origin, models)
		run(models, "bash", script, "restore-models")
		if _, err := os.Stat(filepath.Join(models, "docs/harnesses")); err == nil {
			t.Fatal("restore-models put back the Harness Reference, which it reads from where it is published")
		}
		if i == 1 {
			if b, err := os.ReadFile(filepath.Join(models, "docs/models/index.json")); err != nil || string(b) != "models 1\n" {
				t.Fatalf("restore-models put back %q (%v)", b, err)
			}
		}
		write(models, "docs/models/index.json", content)
		write(models, "docs/harnesses/index.json", "not to be published\n")
		if s := run(models, "git", "status", "--porcelain"); s != "" {
			t.Fatalf("the Model Reference is not ignored on main:\n%s", s)
		}
		if i == 1 {
			// Another job publishes while this one renders: its publish starts from the branch as
			// it is then, and keeps what the other published.
			other := filepath.Join(tmp, "other")
			run(tmp, "git", "clone", "-q", "-b", "main", origin, other)
			run(other, "bash", script, "restore")
			write(other, "docs/harnesses/claude/versions/1.0.3.json", "meanwhile\n")
			run(other, "bash", script, "publish-reference")
		}
		run(models, "bash", script, "publish-models")
	}
	// A run that changed nothing commits nothing.
	same := filepath.Join(tmp, "models-same")
	run(tmp, "git", "clone", "-q", "-b", "main", origin, same)
	run(same, "bash", script, "restore-models")
	run(same, "bash", script, "publish-models")
	// And the Harness Reference's next publish leaves the Model Reference where it is.
	after := filepath.Join(tmp, "after")
	run(tmp, "git", "clone", "-q", "-b", "main", origin, after)
	run(after, "bash", script, "restore")
	write(after, "docs/harnesses/claude/versions/1.0.1.json", "between\n")
	write(after, "docs/harnesses/claude/versions/1.0.2.json", "after\n")
	run(after, "bash", script, "publish-reference")

	// An origin that cannot be read fails the restore: starting from nothing would publish every
	// model as new.
	lost := filepath.Join(tmp, "lost")
	run(tmp, "git", "clone", "-q", "-b", "main", origin, lost)
	run(lost, "git", "remote", "set-url", "origin", filepath.Join(tmp, "gone.git"))
	cmd := exec.Command("bash", script, "restore-models")
	cmd.Dir = lost
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "cannot read origin's compat-matrix") {
		t.Errorf("restore-models went on without its origin (%v):\n%s", err, out)
	}

	check := filepath.Join(tmp, "check")
	run(tmp, "git", "clone", "-q", "-b", "compat-matrix", origin, check)
	if n := run(check, "git", "rev-list", "--count", "HEAD"); n != "7" {
		t.Errorf("compat-matrix has %s commits, want one a night, the checks' and two of the Model Reference's", n)
	}
	if b, err := os.ReadFile(filepath.Join(check, "docs/harnesses/claude/versions/1.0.3.json")); err != nil || string(b) != "meanwhile\n" {
		t.Errorf("the Model Reference's publish lost what another job published meanwhile: %q (%v)", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(check, "docs/models/index.json")); err != nil || string(b) != "models 2\n" {
		t.Errorf("compat-matrix has the Model Reference as %q (%v)", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(check, "docs/harnesses/claude/versions/1.0.1.json")); err != nil || string(b) != "between\n" {
		t.Errorf("the check's reference was not published: %q (%v)", b, err)
	}
	for _, f := range generated {
		if b, err := os.ReadFile(filepath.Join(check, f)); err != nil || string(b) != "night 2\n" {
			t.Errorf("compat-matrix has %s as %q (%v), want night 2's", f, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(check, "docs/harnesses/claude/diffs/0.9.0..1.0.0.json")); err == nil {
		t.Error("a reference file the night no longer wrote stayed on compat-matrix")
	}
	if _, err := os.Stat(filepath.Join(check, "README.md")); err == nil {
		t.Error("compat-matrix carries main's files")
	}

	// A check that ran the new build's contracts publishes the matrix they extended with its
	// reference; the field catalog stays the night's.
	contracts := filepath.Join(tmp, "contracts")
	run(tmp, "git", "clone", "-q", "-b", "main", origin, contracts)
	run(contracts, "bash", script, "restore")
	for _, f := range generated {
		write(contracts, f, "contracts\n")
	}
	t.Setenv("TERMA_REFERENCE_MATRIX", "1")
	run(contracts, "bash", script, "publish-reference")
	run(check, "git", "pull", "-q")
	for _, f := range generated {
		want := "contracts\n"
		if f == "docs/FIELDS.md" || f == "docs/compat/fields.json" {
			want = "night 2\n"
		}
		if b, err := os.ReadFile(filepath.Join(check, f)); err != nil || string(b) != want {
			t.Errorf("after the contracts' check, compat-matrix has %s as %q (%v), want %q", f, b, err, want)
		}
	}
}
