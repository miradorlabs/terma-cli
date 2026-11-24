package repohooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/gitx"
)

func TestSyncIfInstallsOnlyWhenWanted(t *testing.T) {
	stateDir := t.TempDir()
	repo := filepath.Join(t.TempDir(), "work")
	if _, err := gitx.Git(context.Background(), filepath.Dir(repo), "init", "-q", "-b", "main", repo); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(repo, ".git", "hooks", "prepare-commit-msg")
	changed, err := SyncIf(stateDir, "/usr/local/bin/terma", func(string) bool { return false }, repo)
	if err != nil || changed {
		t.Fatalf("SyncIf(false) = (%v, %v); want (false, nil)", changed, err)
	}
	if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
		t.Fatalf("prepare-commit-msg installed when wanted=false: err=%v", err)
	}
	changed, err = SyncIf(stateDir, "/usr/local/bin/terma", func(string) bool { return true }, repo)
	if err != nil || !changed {
		t.Fatalf("SyncIf(true) = (%v, %v); want (true, nil)", changed, err)
	}
	if _, err := os.Stat(hookPath); err != nil {
		t.Fatalf("prepare-commit-msg not installed when wanted=true: %v", err)
	}
}
