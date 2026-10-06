package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

const envKey = "env"

// settingsFile is a Claude Code settings document held as json.RawMessage, so settings terma
// does not know survive byte-for-byte; only top-level key order is lost.
type settingsFile struct {
	path string
	// writePath is path with symlinks resolved, so the atomic rename does not replace a dotfiles link.
	writePath string
	root      map[string]json.RawMessage
	env       map[string]string
	existed   bool
	// symlinked files are emptied to `{}`, never deleted: deleting the target leaves the link dangling.
	symlinked bool
	// mode is kept, so a file tighter than 0600 is not loosened.
	mode fs.FileMode
}

func loadSettings(path string) (*settingsFile, error) {
	s := &settingsFile{
		path:      path,
		writePath: path,
		root:      map[string]json.RawMessage{},
		env:       map[string]string{},
		mode:      harness.SettingsMode,
	}

	writePath, symlinked, err := config.ResolveWritePath(path)
	if err != nil {
		return nil, err
	}
	s.writePath, s.symlinked = writePath, symlinked

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	s.existed = true

	if info, statErr := os.Stat(path); statErr == nil {
		s.mode = info.Mode().Perm()
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return s, nil
	}

	if err := json.Unmarshal(data, &s.root); err != nil {
		// A merge into a file terma cannot parse would overwrite settings it cannot see.
		return nil, fmt.Errorf("parse %s: %w (fix or move the file, then retry)", path, err)
	}

	if raw, ok := s.root[envKey]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &s.env); err != nil {
			return nil, fmt.Errorf("parse %q in %s: %w (values must be strings)", envKey, path, err)
		}
	}
	if s.env == nil {
		s.env = map[string]string{}
	}
	return s, nil
}

func (s *settingsFile) merge(env map[string]string) {
	maps.Copy(s.env, env)
}

// remove deletes the named keys and reports how many were present.
func (s *settingsFile) remove(keys []string) int {
	removed := 0
	for _, k := range keys {
		if _, ok := s.env[k]; ok {
			delete(s.env, k)
			removed++
		}
	}
	return removed
}

// save writes the document back atomically; tighten clamps a permissive mode to 0600 when the
// file now carries a credential.
func (s *settingsFile) save(tighten bool) error {
	if len(s.env) == 0 {
		delete(s.root, envKey)
	} else {
		encoded, err := hookmgr.MarshalJSON(s.env, "", "")
		if err != nil {
			return fmt.Errorf("encode %q: %w", envKey, err)
		}
		s.root[envKey] = encoded
	}

	// An empty document is removed, except through a symlink, where it is written as `{}`.
	if len(s.root) == 0 && !s.symlinked {
		if !s.existed {
			return nil
		}
		if err := os.Remove(s.writePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", s.writePath, err)
		}
		return nil
	}

	data, err := hookmgr.MarshalJSON(s.root, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", s.writePath, err)
	}
	data = append(data, '\n')

	mode := s.mode
	if tighten && mode&0o077 != 0 {
		mode = harness.SettingsMode
	}

	if err := os.MkdirAll(filepath.Dir(s.writePath), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(s.writePath), err)
	}
	return config.WriteFileAtomic(s.writePath, data, mode)
}

// backup copies the current file alongside itself before the first modification, best effort.
// replace, set when the current file is not terma's own work, lets it overwrite a stale backup.
func (s *settingsFile) backup(replace bool) (string, error) {
	return harness.BackupFile(s.writePath, s.existed, replace)
}
