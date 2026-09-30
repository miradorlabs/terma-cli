package harness

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

const (
	// SettingsMode is what a settings file is written as once it holds a server key.
	// The default 0644 would leave a live credential readable by every account on the
	// machine, and the file is only ever read by the harness running as this user.
	SettingsMode fs.FileMode = 0o600
)

// ResolveWritePath is where a config file should actually be written, given the path
// the harness reads it from.
//
// A symlink is resolved so the write lands on the real file and the link survives. A
// *dangling* link is refused rather than followed: writing to the link path would
// replace it with a regular file, silently detaching a dotfiles setup from its repo,
// and there is no safe way to guess where the missing target was meant to live.
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
		// Not a link itself, but a parent directory may be one; resolve so the atomic
		// rename happens in the directory the file actually lives in.
		return resolved, false, nil
	}
	return path, false, nil
}

// BackupFile copies writePath to writePath.terma.bak — alongside the real file, not
// the link that points at it. See settingsFile.backup for when replace is set.
func BackupFile(writePath string, existed, replace bool) (string, error) {
	if !existed {
		return "", nil
	}
	path := writePath + ".terma.bak"
	if _, err := os.Stat(path); err == nil {
		if !replace {
			return path, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	data, err := os.ReadFile(writePath)
	if err != nil {
		return "", err
	}
	// Written 0600 regardless of the source's mode: a backup of a connected config holds
	// a server key, and inheriting a permissive mode would copy it somewhere readable.
	if err := config.WriteFileAtomic(path, data, SettingsMode); err != nil {
		return "", err
	}
	return path, nil
}

// RecordLockWait bounds the wait for another terma's update of a displaced-settings
// record (a notifier, a status line terma wrapped). Only connect, install and
// disconnect write one, never a hook, so it may wait.
const RecordLockWait = 5 * time.Second
