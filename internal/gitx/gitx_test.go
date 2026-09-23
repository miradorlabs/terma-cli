package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		if _, err := run(ctx, dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return dir
}

func TestStagedFilesAndHead(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	if HeadSHA(ctx, dir) != "" {
		t.Fatal("unborn repo should have no HEAD")
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "src", "a.go"), []byte("package a\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644)
	if _, err := run(ctx, dir, "add", "src/a.go"); err != nil {
		t.Fatal(err)
	}
	staged, err := StagedFiles(ctx, dir)
	if err != nil || len(staged) != 1 || staged[0] != "src/a.go" {
		t.Fatalf("staged %v (%v)", staged, err)
	}
	if _, err := run(ctx, dir, "commit", "-q", "-m", "first\n\nAgent-Session-Id: s1"); err != nil {
		t.Fatal(err)
	}
	if HeadSHA(ctx, dir) == "" {
		t.Fatal("expected a HEAD after commit")
	}
	msg, err := CommitMessage(ctx, dir, "HEAD")
	if err != nil || msg != "first\n\nAgent-Session-Id: s1" {
		t.Fatalf("message %q (%v)", msg, err)
	}
	if staged, _ := StagedFiles(ctx, dir); len(staged) != 0 {
		t.Fatalf("nothing should be staged after commit: %v", staged)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	if CommentChar(ctx, dir) != "#" {
		t.Fatal("default comment char")
	}
	if err := ConfigSet(ctx, dir, "core.hooksPath", ".terma/hooks"); err != nil {
		t.Fatal(err)
	}
	if got := ConfigGet(ctx, dir, "core.hooksPath"); got != ".terma/hooks" {
		t.Fatalf("got %q", got)
	}
	if err := ConfigUnset(ctx, dir, "core.hooksPath"); err != nil {
		t.Fatal(err)
	}
	if err := ConfigUnset(ctx, dir, "core.hooksPath"); err != nil {
		t.Fatalf("unsetting a missing key must be a no-op: %v", err)
	}
}

func TestRelativize(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "repo")
	if got := Relativize(root, filepath.Join(root, "src", "a.go")); got != "src/a.go" {
		t.Fatalf("got %q", got)
	}
	if got := Relativize(root, filepath.Join(string(filepath.Separator), "elsewhere", "x")); got != "" {
		t.Fatalf("outside the root should be empty, got %q", got)
	}
	if got := Relativize(root, "already/relative.go"); got != "already/relative.go" {
		t.Fatalf("got %q", got)
	}
}
