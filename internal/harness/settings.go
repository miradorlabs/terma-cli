package harness

import (
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

const (
	// SettingsMode is the mode of a settings file that holds a server key.
	SettingsMode fs.FileMode = 0o600
)

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
