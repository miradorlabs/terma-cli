package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Journal records what a connect changed, so a disconnect restores exactly that: a key is
// Terma's only while it holds what Terma wrote. It can hold a previous credential (0600).
type Journal struct {
	Harness    string `json:"harness"`
	ConfigPath string `json:"config_path"`

	Installed map[string]string `json:"installed"`
	// Previous is what each key held before; nil means absent, which restores differently from empty.
	Previous map[string]*string `json:"previous"`
	// Cleared holds conflicting settings --force removed, so disconnect can put them back.
	Cleared map[string]string `json:"cleared,omitempty"`
	// ClearedSettings is Cleared for top-level settings, which restored inside env would be invalid.
	ClearedSettings map[string]string `json:"cleared_settings,omitempty"`
	// InstalledSettings and PreviousSettings mirror Installed and Previous for top-level settings.
	InstalledSettings map[string]string  `json:"installed_settings,omitempty"`
	PreviousSettings  map[string]*string `json:"previous_settings,omitempty"`
	ProjectID         string             `json:"project_id,omitempty"`
}

// JournalPath keys the record by a hash of the config path, so a sandboxed config and the
// real one beside it never overwrite each other's record.
func JournalPath(harness, configPath string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(configPath))
	name := fmt.Sprintf("%s-%s.json", harness, hex.EncodeToString(sum[:])[:12])
	return filepath.Join(dir, "telemetry", name), nil
}

// LoadJournal returns nil, not an error, when there is no record.
func LoadJournal(harness, configPath string) (*Journal, error) {
	path, err := JournalPath(harness, configPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		// Corruption is not absence: treating it as absent would let disconnect delete a user's edits.
		return nil, fmt.Errorf("parse %s: %w (repair or remove it explicitly, then retry)", path, err)
	}
	if j.ConfigPath != configPath {
		// A hash collision or a file moved by hand: not this config's record.
		return nil, nil
	}
	if j.Harness != harness || j.ConfigPath == "" || j.Installed == nil || j.Previous == nil {
		return nil, fmt.Errorf("parse %s: incomplete telemetry journal (repair or remove it explicitly, then retry)", path)
	}
	for key := range j.Installed {
		if _, ok := j.Previous[key]; !ok {
			return nil, fmt.Errorf("parse %s: missing previous value for %s (repair or remove it explicitly, then retry)", path, key)
		}
	}
	if j.InstalledSettings == nil {
		j.InstalledSettings = map[string]string{}
	}
	if j.PreviousSettings == nil {
		j.PreviousSettings = map[string]*string{}
	}
	for key := range j.InstalledSettings {
		if _, ok := j.PreviousSettings[key]; !ok {
			return nil, fmt.Errorf("parse %s: missing previous value for setting %s (repair or remove it explicitly, then retry)", path, key)
		}
	}
	return &j, nil
}

// Save writes the journal, private to this user: it can hold a previous credential.
func (j *Journal) Save() error {
	path, err := JournalPath(j.Harness, j.ConfigPath)
	if err != nil {
		return err
	}
	return config.WriteJSON(path, j, SettingsMode)
}

// DeleteJournal removes the journal of the config at configPath.
func DeleteJournal(harness, configPath string) error {
	path, err := JournalPath(harness, configPath)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// NewJournal captures the state before a connect; on a reconnect, unchanged keys keep
// their pre-Terma value and keys edited since become the value to restore.
func NewJournal(
	harnessName, configPath string,
	existing, installing map[string]string,
	cleared, clearedSettings map[string]string,
	previous *Journal,
) *Journal {
	j := &Journal{
		Harness:           harnessName,
		ConfigPath:        configPath,
		Installed:         make(map[string]string, len(installing)),
		Previous:          make(map[string]*string, len(installing)),
		Cleared:           map[string]string{},
		ClearedSettings:   map[string]string{},
		InstalledSettings: map[string]string{},
		PreviousSettings:  map[string]*string{},
	}

	// A conditional key this connect omits is still owned while it stays unchanged.
	if previous != nil {
		for key, installed := range previous.Installed {
			if _, overwritten := installing[key]; overwritten {
				continue
			}
			j.Installed[key] = installed
			j.Previous[key] = CloneString(previous.Previous[key])
		}
		maps.Copy(j.Cleared, previous.Cleared)
		maps.Copy(j.ClearedSettings, previous.ClearedSettings)
		for key, installed := range previous.InstalledSettings {
			j.InstalledSettings[key] = installed
			j.PreviousSettings[key] = CloneString(previous.PreviousSettings[key])
		}
	}

	for key, value := range installing {
		j.Installed[key] = value
		if previous != nil {
			if priorInstalled, owned := previous.Installed[key]; owned {
				current, present := existing[key]
				if present && current == priorInstalled {
					j.Previous[key] = CloneString(previous.Previous[key])
					continue
				}
				// Edited since the earlier connect: restore the edit, not stale pre-history.
				if present {
					current := current
					j.Previous[key] = &current
				} else {
					j.Previous[key] = nil
				}
				continue
			}
		}
		if prior, ok := existing[key]; ok {
			prior := prior
			j.Previous[key] = &prior
		} else {
			j.Previous[key] = nil
		}
	}
	maps.Copy(j.Cleared, cleared)
	maps.Copy(j.ClearedSettings, clearedSettings)
	return j
}

// CloneString copies what value points at; nil stays nil.
func CloneString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// Apply undoes the connect against env, touching only keys that still hold what Terma installed.
func (j *Journal) Apply(env map[string]string) (DisconnectResult, *Journal) {
	var result DisconnectResult
	remaining := &Journal{
		Harness:           j.Harness,
		ConfigPath:        j.ConfigPath,
		Installed:         map[string]string{},
		Previous:          map[string]*string{},
		Cleared:           map[string]string{},
		ClearedSettings:   map[string]string{},
		InstalledSettings: map[string]string{},
		PreviousSettings:  map[string]*string{},
	}

	for key, installed := range j.Installed {
		current, present := env[key]
		if !present || current != installed {
			result.Skipped = append(result.Skipped, key)
			remaining.Installed[key] = installed
			remaining.Previous[key] = CloneString(j.Previous[key])
			continue
		}

		if prior := j.Previous[key]; prior != nil {
			env[key] = *prior
			result.Restored++
		} else {
			delete(env, key)
			result.Removed++
		}
	}

	// Conflicts --force took away go back only if nothing has claimed the key since.
	for key, value := range j.Cleared {
		if _, taken := env[key]; !taken {
			env[key] = value
			result.Restored++
		} else {
			result.Skipped = append(result.Skipped, key)
			remaining.Cleared[key] = value
		}
	}
	return result, remaining
}

// Empty reports whether the journal records nothing terma owns.
func (j *Journal) Empty() bool {
	return len(j.Installed) == 0 && len(j.Cleared) == 0 && len(j.ClearedSettings) == 0 &&
		len(j.InstalledSettings) == 0
}

// PruneJournals removes, best-effort, records whose config file no longer exists.
func PruneJournals() {
	dir, err := config.Dir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(filepath.Join(dir, "telemetry"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, "telemetry", entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var record struct {
			ConfigPath string `json:"config_path"`
		}
		if json.Unmarshal(data, &record) != nil || record.ConfigPath == "" {
			// Deleting what cannot be read could lose a live config's ownership record.
			continue
		}
		if _, err := os.Stat(record.ConfigPath); errors.Is(err, fs.ErrNotExist) {
			_ = os.Remove(path)
		}
	}
}
