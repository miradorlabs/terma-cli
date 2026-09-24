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

// journal records exactly what a connect changed, so a disconnect can put it back.
//
// Without it, ownership has to be inferred — "the endpoint points at Terma, so
// Terma must have written this" — and inference gets it wrong in both directions. A
// config someone assembled by hand against the Terma endpoint looks like Terma's
// and gets deleted wholesale on disconnect; a Terma config whose endpoint was edited
// out looks like the user's and gets treated as the state worth preserving. Recording
// the values removes the guess: a key is Terma's if Terma wrote it and it still
// holds what Terma wrote.
//
// It lives under ~/.config/terma rather than in the harness's own config, because it is
// Terma's bookkeeping and has no business in a file the user reads and edits. It can
// hold a previous Authorization header, so it is written 0600 like the credential file.
type journal struct {
	Harness    string `json:"harness"`
	ConfigPath string `json:"config_path"`

	// Installed is what Terma wrote, keyed by variable. A key whose current value no
	// longer matches has been edited since, and disconnect leaves it alone.
	Installed map[string]string `json:"installed"`
	// Previous is what each of those keys held beforehand. A nil value means the key
	// was absent, which is different from being present and empty — restoring the two
	// differently is the whole point of the pointer.
	Previous map[string]*string `json:"previous"`
	// Cleared holds conflicting settings --force removed, so disconnect can put those
	// back too. They were never Terma's, and taking them was the price of connecting.
	Cleared map[string]string `json:"cleared,omitempty"`
	// ClearedSettings is the same record for top-level settings. Keeping these separate
	// from Cleared matters: restoring a setting such as otelHeadersHelper inside `env`
	// produces a different, invalid configuration.
	ClearedSettings map[string]string `json:"cleared_settings,omitempty"`
	// InstalledSettings / PreviousSettings mirror Installed / Previous for the top-level
	// settings Terma writes — today the otelHeadersHelper path. Same ownership rule:
	// touched on disconnect only while still holding what Terma wrote.
	InstalledSettings map[string]string  `json:"installed_settings,omitempty"`
	PreviousSettings  map[string]*string `json:"previous_settings,omitempty"`
	// ProjectID is the Terma project the connect reported to. Status and key reuse
	// read it back.
	ProjectID string `json:"project_id,omitempty"`
}

// journalPath keys the record by the config file it describes, not by the harness alone.
//
// One harness can be connected in more than one place — a sandbox under CLAUDE_CONFIG_DIR
// alongside the real ~/.claude, which is exactly how anyone tests this. A single
// per-harness record makes those two collide: connecting the sandbox overwrites the
// record for the real config, and every later read finds a journal describing a file it
// was not asked about. Each config gets its own record instead, so the two are simply
// independent.
//
// The path is hashed rather than embedded because it is an absolute filesystem path:
// too long, and full of separators. The basename keeps the harness name so the directory
// stays readable, and ConfigPath inside the file remains the authoritative answer to
// "which config is this?".
func journalPath(harness, configPath string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(configPath))
	name := fmt.Sprintf("%s-%s.json", harness, hex.EncodeToString(sum[:])[:12])
	return filepath.Join(dir, "telemetry", name), nil
}

// loadJournal returns nil when there is no record, which is not an error: a config
// configured by hand or in another clone simply has none.
func loadJournal(harness, configPath string) (*journal, error) {
	path, err := journalPath(harness, configPath)
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

	var j journal
	if err := json.Unmarshal(data, &j); err != nil {
		// Absence means there is no local ownership record. Corruption
		// is different: silently treating a damaged ownership record as absent would let
		// disconnect delete values that a user changed after connecting.
		return nil, fmt.Errorf("parse %s: %w (repair or remove it explicitly, then retry)", path, err)
	}
	if j.ConfigPath != configPath {
		// A hash collision, or a file moved by hand. Not this config's record, so it is
		// as good as absent rather than an error about someone else's file.
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
	// Older journals predate the settings maps; absent means "none installed", which
	// initialized-empty expresses without failing the whole record.
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

func (j *journal) save() error {
	path, err := journalPath(j.Harness, j.ConfigPath)
	if err != nil {
		return err
	}
	// 0600: Previous may hold the Authorization header that was there before.
	return config.WriteJSON(path, j, settingsMode)
}

func deleteJournal(harness, configPath string) error {
	path, err := journalPath(harness, configPath)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// newJournal captures the state before a connect overwrites it. When this is a
// reconnect, previous carries the original ownership chain forward: values that still
// match the prior install retain their pre-Terma value, while values edited since the
// prior connect become the new value to restore after this explicit reconnect.
func newJournal(
	harnessName, configPath string,
	existing, installing map[string]string,
	cleared, clearedSettings map[string]string,
	previous *journal,
) *journal {
	j := &journal{
		Harness:           harnessName,
		ConfigPath:        configPath,
		Installed:         make(map[string]string, len(installing)),
		Previous:          make(map[string]*string, len(installing)),
		Cleared:           map[string]string{},
		ClearedSettings:   map[string]string{},
		InstalledSettings: map[string]string{},
		PreviousSettings:  map[string]*string{},
	}

	// Carry keys from an earlier connect that this connect does not write. Render can
	// omit a conditional key (for example the traces beta switch), but disconnect still
	// owns it if it remains unchanged in the file.
	if previous != nil {
		for key, installed := range previous.Installed {
			if _, overwritten := installing[key]; overwritten {
				continue
			}
			j.Installed[key] = installed
			j.Previous[key] = cloneString(previous.Previous[key])
		}
		maps.Copy(j.Cleared, previous.Cleared)
		maps.Copy(j.ClearedSettings, previous.ClearedSettings)
		// Settings ownership carries the same way env ownership does: the pre-Terma
		// value survives reconnects so the eventual disconnect restores it, not an
		// intermediate Terma state.
		for key, installed := range previous.InstalledSettings {
			j.InstalledSettings[key] = installed
			j.PreviousSettings[key] = cloneString(previous.PreviousSettings[key])
		}
	}

	for key, value := range installing {
		j.Installed[key] = value
		if previous != nil {
			if priorInstalled, owned := previous.Installed[key]; owned {
				current, present := existing[key]
				if present && current == priorInstalled {
					j.Previous[key] = cloneString(previous.Previous[key])
					continue
				}
				// The value was edited or removed after the earlier connect. This
				// reconnect deliberately overwrites that edit, so restore the edit—not
				// stale pre-history—when it is later disconnected.
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
			// Copied, not aliased: the loop variable would otherwise be shared.
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

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// apply undoes the connect against env, and reports what it did.
//
// A key is only touched when it still holds the value Terma installed. Anything else
// is somebody's later edit — possibly the whole reason they are disconnecting — and
// silently discarding it would be the same class of bug as never having journaled.
func (j *journal) apply(env map[string]string) (DisconnectResult, *journal) {
	var result DisconnectResult
	remaining := &journal{
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
			remaining.Previous[key] = cloneString(j.Previous[key])
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

func (j *journal) empty() bool {
	return len(j.Installed) == 0 && len(j.Cleared) == 0 && len(j.ClearedSettings) == 0 &&
		len(j.InstalledSettings) == 0
}

// pruneJournals removes records whose config file no longer exists — a temp-dir
// sandbox that was deleted, a CLAUDE_CONFIG_DIR that came and went. Best-effort by
// design and called after successful connects and disconnects: a record that cannot be
// pruned today is retried on the next lifecycle operation, and a prune failure must
// never fail the operation that triggered it.
func pruneJournals() {
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
			// Unparseable records are left in place: deleting what cannot be read is how
			// an ownership record for a live config gets lost.
			continue
		}
		if _, err := os.Stat(record.ConfigPath); errors.Is(err, fs.ErrNotExist) {
			_ = os.Remove(path)
		}
	}
}
