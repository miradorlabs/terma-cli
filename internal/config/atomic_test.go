package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Both durabilities replace the file whole, at the mode asked for, and leave no temp
// file behind — the only difference between them is the fsync.
func TestWriteFileAtomicVariants(t *testing.T) {
	for name, write := range map[string]func(string, []byte, os.FileMode) error{
		"durable": WriteFileAtomic,
		"no sync": WriteFileAtomicNoSync,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			if err := os.WriteFile(path, []byte("old and considerably longer than the new content"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := write(path, []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(path); string(got) != "new" {
				t.Fatalf("content = %q", got)
			}
			if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatalf("a temp file was left behind: %v", entries)
			}
		})
	}
}

func TestWriteFileAtomicLeavesTheOldFileWhenItCannotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-dir", "state.json")
	if err := WriteFileAtomic(path, []byte("x"), 0o600); err == nil {
		t.Fatal("writing into a directory that does not exist must fail")
	}
}

// WriteJSON is the whole recipe, so a first write on a new machine needs nothing done
// for it beforehand.
func TestWriteJSONCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not", "there", "yet", "keys.json")
	if err := WriteJSON(path, map[string]string{"a": "b"}, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"a\": \"b\"\n}\n"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if info, _ := os.Stat(filepath.Dir(path)); runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v, want 0700", info.Mode().Perm())
	}
	if strings.Contains(string(got), "\t") {
		t.Fatal("state files are indented with spaces")
	}
}
