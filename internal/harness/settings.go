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
	// SettingsMode is the mode of a settings file that holds a server key.
	SettingsMode fs.FileMode = 0o600
)

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

// BackupFile copies writePath to writePath.terma.bak, replacing an existing backup only when replace is set.
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
	// 0600 regardless of the source: the backup may hold a server key.
	if err := config.WriteFileAtomic(path, data, SettingsMode); err != nil {
		return "", err
	}
	return path, nil
}

// RecordLockWait bounds the wait on a displaced-settings record; only commands, never hooks, write one.
const RecordLockWait = 5 * time.Second
