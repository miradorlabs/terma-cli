package e2e

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Run this offline too: provider credentials must never be needed to prepare
// the real CLI's sandbox or install an additional hooks-only adapter.
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
	sb.terma(sb.Repo, "install", "--harness", "none", "--no-browser", "--no-doctor", "--project", sb.ProjectID, "--adapters", "cursor", "--yes")
	sb.connectClaude()
	sb.connectCodex()
}
