// Package hookruntest is what a hook handler's tests share: a sandboxed repository and
// the events a handler spooled.
package hookruntest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// InitRepo creates a git repository with a private terma config directory, and returns
// its resolved path.
func InitRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "dev@example.com"},
		{"config", "user.name", "Dev"},
		{"config", "commit.gpgsign", "false"},
	} {
		if _, err := gitx.Git(context.Background(), dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	resolved, _ := filepath.EvalSymlinks(dir)
	return resolved
}

// WriteFile writes content to rel under root, creating its directories.
func WriteFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Spooled flushes the spool and returns every event in it, in order.
func Spooled(t *testing.T, sp *spool.Spool) []spool.Event {
	t.Helper()
	var out []spool.Event
	res := sp.Flush(context.Background(), spool.SenderFunc(func(_ context.Context, events []spool.Event) ([]spool.Event, error) {
		out = append(out, events...)
		return nil, nil
	}), spool.FlushOptions{})
	if res.Err != nil {
		t.Fatalf("flush: %v", res.Err)
	}
	return out
}
