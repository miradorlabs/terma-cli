package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run this offline too: provider credentials must never be needed to prepare the real
// CLI's sandbox, whose setup writes the agents' machine-wide hooks into the sandbox and
// nowhere else.
func TestSandboxSetupWithoutProviderCredentials(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "terma")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/terma")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	t.Setenv("TERMA_E2E", "1")
	t.Setenv("TERMA_E2E_BINARY", binary)
	sb := New(t, Isolated)
	shim := filepath.Join(sb.Dir, "path", "terma")
	for _, f := range []string{filepath.Join(sb.ClaudeConfig, "settings.json"), filepath.Join(sb.CodexHome, "hooks.json")} {
		if data, err := os.ReadFile(f); err != nil || !strings.Contains(string(data), shim+"' hook --user") {
			t.Errorf("%s has no machine-wide hooks calling the recording shim: %v\n%s", f, err, data)
		}
	}
	// Setup leaves git's own configuration alone: the commit hooks go into each repository
	// an agent session is claimed in, never into a hooks path. git exits 1 on an unset key,
	// so the config is read as a file.
	if cfg, err := os.ReadFile(sb.gitConfigGlobal()); err == nil && strings.Contains(strings.ToLower(string(cfg)), "hookspath") {
		t.Errorf("setup wrote a hooks path into git's global config:\n%s", cfg)
	}
	// The scratch repository has had no agent session yet, so it has none of terma's hooks.
	for _, h := range []string{"prepare-commit-msg", "post-commit"} {
		if _, err := os.Stat(filepath.Join(sb.Repo, ".git", "hooks", h)); err == nil {
			t.Errorf("setup installed %s before any session was claimed", h)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(sb.ClaudeConfig, "settings.json")); !strings.Contains(string(data), "statusline") {
		t.Errorf("setup did not wrap the status line:\n%s", data)
	}
	sb.directClaude()
	sb.directCodex()
}
