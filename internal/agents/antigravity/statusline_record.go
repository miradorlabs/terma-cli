package antigravity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// The record of what setup replaced in agy's settings lives under terma's config
// directory, as every setup change does.

func (a Agent) statusLineRecordPath() string {
	return filepath.Join(a.ConfigDir, config.SetupDir, antigravityStatusLineRecord)
}

func (a Agent) statusLineRecords() (map[string]*antigravityStatusLineRecordEntry, error) {
	path := a.statusLineRecordPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]*antigravityStatusLineRecordEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	recs := map[string]*antigravityStatusLineRecordEntry{}
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return recs, nil
}

func (a Agent) statusLineRecord(settingsPath string) (*antigravityStatusLineRecordEntry, error) {
	if a.ConfigDir == "" {
		return nil, errors.New("antigravity: no config directory for the status line record")
	}
	recs, err := a.statusLineRecords()
	if err != nil {
		return nil, err
	}
	return recs[settingsPath], nil
}

// updateStatusLineRecords edits the record file under a lock; it holds the file's prior
// content, so it is as private as the settings it came from.
func (a Agent) updateStatusLineRecords(edit func(map[string]*antigravityStatusLineRecordEntry)) error {
	if a.ConfigDir == "" {
		return errors.New("antigravity: no config directory for the status line record")
	}
	path := a.statusLineRecordPath()
	return flock.Locked(path, harness.RecordLockWait, func() error {
		recs, err := a.statusLineRecords()
		if err != nil {
			return err
		}
		edit(recs)
		if len(recs) == 0 {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		}
		return config.WriteJSON(path, recs, harness.SettingsMode)
	})
}

func (a Agent) deleteStatusLineRecord(settingsPath string) error {
	return a.updateStatusLineRecords(func(recs map[string]*antigravityStatusLineRecordEntry) { delete(recs, settingsPath) })
}
