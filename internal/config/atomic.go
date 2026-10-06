package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// WriteFileAtomic writes a temp file and renames it, syncing file and directory so a
// rotated refresh token survives a power loss. A file already holding data at perm is
// left alone, so whatever watches it sees no change that isn't one.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if mode, ok := holds(path, data); ok && sameMode(mode, perm) {
		return nil
	}
	return writeFileAtomic(path, data, perm, true)
}

// sameMode compares the bits the platform keeps: Windows keeps only the owner-write bit.
func sameMode(have, want os.FileMode) bool {
	if runtime.GOOS == "windows" {
		return have&0o200 == want&0o200
	}
	return have == want
}

// holds reports whether path is a regular file whose contents are exactly data, and its
// mode, special bits included so a stray setuid is rewritten away.
func holds(path string, data []byte) (os.FileMode, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(data)) {
		return 0, false
	}
	current, err := os.ReadFile(path)
	return info.Mode(), err == nil && bytes.Equal(current, data)
}

// WriteFileAtomicNoSync is WriteFileAtomic without the syncs or the skip, for state hooks
// rewrite on every tool call (a claim's age is its mtime): on macOS a sync is
// F_FULLFSYNC, ~10 ms against 0.2 ms.
func WriteFileAtomicNoSync(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(path, data, perm, false)
}

// ResolveWritePath resolves a symlinked config so the write keeps the link, and refuses a
// dangling one rather than replacing it with a regular file.
func ResolveWritePath(path string) (writePath string, symlinked bool, err error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			target, readErr := os.Readlink(path)
			if readErr != nil {
				target = "its target"
			}
			return "", true, fmt.Errorf(
				"%s is a symlink to %s, which does not exist — restore it or replace the link, then retry",
				path, target)
		}
		return resolved, true, nil
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		// A parent may be a link; the atomic rename must happen where the file lives.
		return resolved, false, nil
	}
	return path, false, nil
}

// TempPrefix starts the name of an atomic write's temporary file, which a crash can leave behind.
const TempPrefix = ".tmp-"

func writeFileAtomic(path string, data []byte, perm os.FileMode, durable bool) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, TempPrefix+"*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	// Chmod first, so a secret is never briefly readable at the default mode.
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if durable {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return fmt.Errorf("sync temp file: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	if !durable {
		return nil
	}
	// Sync the directory so the rename survives too; best effort, as not every platform can.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// WriteJSON writes a file of terma's own at path: private directory, indented
// with a trailing newline, atomic and durable.
func WriteJSON(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'), perm)
}
