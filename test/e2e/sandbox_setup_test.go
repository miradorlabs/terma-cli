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
// CLI's sandbox, whose setup writes the agents' machine-wide hooks and git's global
// hooks path into the sandbox and nowhere else.
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
	if hooks := strings.TrimSpace(sb.git("config", "--global", "core.hooksPath")); hooks != filepath.Join(sb.TermaConfig, "git-hooks") {
		t.Errorf("git's global hooks path = %q", hooks)
	}
	if data, _ := os.ReadFile(filepath.Join(sb.ClaudeConfig, "settings.json")); !strings.Contains(string(data), "statusline") {
		t.Errorf("setup did not wrap the status line:\n%s", data)
	}
	sb.directClaude()
	sb.directCodex()
}
