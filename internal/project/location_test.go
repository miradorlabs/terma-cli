package project

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// A writer may resolve its store before git init and write after a newer hook.
// Both handles must address the same lock and files, even with no prior events.
func TestStateDirKeepsInFlightWritersTogether(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	private, err := StateDir(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(private, 0o700); err != nil {
		t.Fatal(err)
	}
	before := session.Open(private)
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	selected, err := StateDir(root, gitDir)
	if err != nil {
		t.Fatal(err)
	}
	after := session.Open(selected)
	sess := session.Session{ID: "same-session", Tool: "claude-code"}
	now := time.Now().UTC()
	if err := after.Touch(sess, []string{"new-hook.txt"}, now); err != nil {
		t.Fatal(err)
	}
	if err := before.Touch(sess, []string{"in-flight-hook.txt"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	manifests, err := after.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 || len(manifests[0].Files) != 2 {
		t.Fatalf("writers split state: %+v", manifests)
	}
	if selected != private {
		t.Fatalf("store changed from %s to %s", private, selected)
	}
}

func TestStateDirDoesNotShareOtherWorkspaces(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	other := t.TempDir()
	private, err := StateDir(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(private, 0o700); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(other, ".git")
	selected, err := StateDir(other, gitDir)
	if err != nil || selected != gitDir {
		t.Fatalf("new Git worktree reused another workspace's store: %s %v", selected, err)
	}
	if _, err := os.Stat(gitDir); !os.IsNotExist(err) {
		t.Fatalf("read-only resolution created state: %v", err)
	}
}

func TestStateDirCanonicalizesWorkspaceAlias(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	private, err := StateDir(alias, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(private, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, alias} {
		selected, err := StateDir(path, filepath.Join(root, ".git"))
		if err != nil || selected != private {
			t.Fatalf("alias selected a different store: %s %v", selected, err)
		}
	}
}
