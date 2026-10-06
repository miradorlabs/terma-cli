//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// holdOpen opens path the way os.Open does, without FILE_SHARE_DELETE, and closes it after d.
func holdOpen(t *testing.T, path string, d time.Duration) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(d)
		_ = f.Close()
	}()
}

// A file someone else is reading is moved and deleted once they let go, not refused.
func TestRenameAndRemoveWaitOutAnOpenHandle(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "hook"), filepath.Join(dir, "hook.pre-terma")
	if err := os.WriteFile(from, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	holdOpen(t, from, 50*time.Millisecond)
	if err := Rename(from, to); err != nil {
		t.Fatalf("rename while open: %v", err)
	}
	holdOpen(t, to, 50*time.Millisecond)
	if err := Remove(to); err != nil {
		t.Fatalf("remove while open: %v", err)
	}
	if _, err := os.Stat(to); !os.IsNotExist(err) {
		t.Fatalf("still there: %v", err)
	}
}
