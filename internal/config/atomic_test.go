package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Both durabilities replace the file whole, at the mode asked for, leaving no temp file.
func TestWriteFileAtomicVariants(t *testing.T) {
	t.Parallel()
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

// The durable write leaves a file already holding its bytes at its mode alone, and
// rewrites one whose bytes or mode differ; the no-sync write still rewrites it, as claims
// refresh their age that way.
func TestWriteFileAtomicSkipsUnchanged(t *testing.T) {
	t.Parallel()
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	seed := func(t *testing.T) string {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte("same"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil { // past the umask
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		return path
	}
	mtime := func(path string) time.Time {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.ModTime()
	}

	path := seed(t)
	if err := WriteFileAtomic(path, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mtime(path).Equal(old) {
		t.Fatal("an unchanged file was rewritten")
	}

	// Windows keeps only the owner-write bit, which 0644 and 0600 share.
	if runtime.GOOS != "windows" {
		if err := WriteFileAtomic(path, []byte("same"), 0o600); err != nil {
			t.Fatal(err)
		}
		if info, _ := os.Stat(path); info.Mode() != 0o600 || mtime(path).Equal(old) {
			t.Fatalf("a file at another mode was not rewritten at 0600: %v", info.Mode())
		}
		if err := os.Chmod(path, 0o600|os.ModeSetuid); err != nil {
			t.Fatal(err)
		}
		if err := WriteFileAtomic(path, []byte("same"), 0o600); err != nil {
			t.Fatal(err)
		}
		if info, _ := os.Stat(path); info.Mode() != 0o600 {
			t.Fatalf("a setuid file was left at %v, want 0600", info.Mode())
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if err := WriteFileAtomic(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "changed" || mtime(path).Equal(old) {
		t.Fatalf("a changed file was not rewritten: %q", got)
	}

	path = seed(t)
	if err := WriteFileAtomicNoSync(path, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	if mtime(path).Equal(old) {
		t.Fatal("the no-sync write did not refresh an unchanged file")
	}
}

func TestWriteFileAtomicLeavesTheOldFileWhenItCannotWrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing-dir", "state.json")
	if err := WriteFileAtomic(path, []byte("x"), 0o600); err == nil {
		t.Fatal("writing into a directory that does not exist must fail")
	}
}

// A write through a symlink lands on the file it points at; a dangling link is refused
// rather than replaced by a regular file.
func TestResolveWritePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	resolvedReal, _ := filepath.EvalSymlinks(target)
	if got, linked, err := ResolveWritePath(link); err != nil || !linked || got != resolvedReal {
		t.Fatalf("link: %q %v %v", got, linked, err)
	}
	dangling := filepath.Join(dir, "dangling.json")
	if err := os.Symlink(filepath.Join(dir, "gone.json"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveWritePath(dangling); err == nil {
		t.Fatal("a dangling link was accepted")
	}
	missing := filepath.Join(dir, "missing.json")
	if got, linked, err := ResolveWritePath(missing); err != nil || linked || got != missing {
		t.Fatalf("missing: %q %v %v", got, linked, err)
	}
}

// WriteJSON creates its directory, so a first write needs nothing beforehand.
func TestWriteJSONCreatesItsDirectory(t *testing.T) {
	t.Parallel()
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
