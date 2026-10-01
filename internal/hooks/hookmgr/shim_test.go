package hookmgr

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestShimNeverChainsItself runs the committed shim through git with
// core.hooksPath pointing at the shim directory (the installed layout) and a
// terma-less PATH. The shim must exit 0 promptly, chain the repository's own
// hook of the same name, and never exec itself.
func TestShimNeverChainsItself(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	git("config", "core.hooksPath", ShimDir)

	shim := filepath.Join(root, filepath.FromSlash(ShimDir), "prepare-commit-msg")
	if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte(ShimScript("prepare-commit-msg")), 0o755); err != nil {
		t.Fatal(err)
	}
	// The repository's own hook, which the shim must chain exactly once.
	marker := filepath.Join(root, "chained.txt")
	own := filepath.Join(git("rev-parse", "--git-common-dir"), "hooks", "prepare-commit-msg")
	if !filepath.IsAbs(own) {
		own = filepath.Join(root, own)
	}
	if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, []byte("#!/bin/sh\necho \"$1\" >> \""+marker+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", shim, ".git/COMMIT_EDITMSG", "message")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH=/usr/bin:/bin", "TERMA_CHAIN_HOOKS_DIR=")
	out, err := cmd.CombinedOutput()
	// The context deadline is the recursion guard: a shim that execs itself never
	// returns, and the deadline is what stops it. There is deliberately no tighter
	// wall-clock bound here — this test measures correctness, not speed (that is
	// `make bench-hook`), and a 2s bound flaked under -race on a loaded runner.
	if ctx.Err() != nil {
		t.Fatalf("shim did not finish (recursion?):\n%s", out)
	}
	if err != nil {
		t.Fatalf("shim failed: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(marker)
	if strings.Count(string(got), ".git/COMMIT_EDITMSG") != 1 {
		t.Fatalf("chained hook ran %d times, want 1:\n%s", strings.Count(string(got), "COMMIT_EDITMSG"), got)
	}

	// Without a hook of its own, the shim exits 0 with nothing to chain.
	if err := os.Remove(own); err != nil {
		t.Fatal(err)
	}
	cmd = exec.CommandContext(ctx, "/bin/sh", shim, ".git/COMMIT_EDITMSG", "message")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH=/usr/bin:/bin", "TERMA_CHAIN_HOOKS_DIR=")
	if out, err := cmd.CombinedOutput(); err != nil || ctx.Err() != nil {
		t.Fatalf("bare shim: %v\n%s", err, out)
	}
}
