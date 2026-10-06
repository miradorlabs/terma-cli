package claude

import (
	"fmt"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// Connect merges terma's variables into the settings file, leaving every other setting alone.
// clearConflicts removes the clearable conflicts; without it the caller must refuse while any remain.
func (c exporter) Connect(e harness.Exporter, clearConflicts bool) error {
	env := c.render(e)
	settings := map[string]string{}
	if e.HelperPath != "" {
		settings[claudeOtelHeadersHelper] = e.HelperPath
	}

	path, err := c.ConfigPath()
	if err != nil {
		return err
	}
	s, err := loadSettings(path)
	if err != nil {
		return err
	}
	previousJournal, err := harness.LoadJournal(c.dir, c.Name(), path)
	if err != nil {
		return err
	}

	cleared := map[string]string{}
	clearedSettings := map[string]string{}
	if clearConflicts {
		for _, conflict := range claudeConflicts(c.dir, s.env, s.root, e, c.layer()) {
			// Only this file is terma's to touch; the caller refuses a connect over the rest.
			if !conflict.Clearable {
				continue
			}
			// The merge overwrites the generic endpoint anyway.
			if conflict.Key == harness.EnvOTLPEndpoint {
				continue
			}
			if conflict.Key == claudeOtelHeadersHelper {
				delete(s.root, claudeOtelHeadersHelper)
				clearedSettings[claudeOtelHeadersHelper] = conflict.Value
				continue
			}
			if value, ok := s.env[conflict.Key]; ok {
				cleared[conflict.Key] = value
			}
			delete(s.env, conflict.Key)
			// The switch alone does nothing, so it goes with its endpoint.
			if conflict.Key == claudeBetaTracingEndpoint {
				if value, ok := s.env[claudeBetaTracingDetailed]; ok {
					cleared[claudeBetaTracingDetailed] = value
				}
				delete(s.env, claudeBetaTracingDetailed)
			}
		}
	}

	// Clear managed keys this render omits: a leftover OTEL_EXPORTER_OTLP_HEADERS outranks the helper,
	// so a previous project's key keeps winning while everything reports connected. Only this scope's keys.
	for _, key := range c.managedKeys() {
		if _, rendered := env[key]; rendered {
			continue
		}
		value, present := s.env[key]
		if !present {
			continue
		}
		// Restore the pre-terma value an earlier connect recorded, never terma's own leftover.
		restore := value
		if previousJournal != nil {
			if installed, ok := previousJournal.Installed[key]; ok && installed == value {
				restore = ""
				if prior := previousJournal.Previous[key]; prior != nil {
					restore = *prior
				}
			}
		}
		if restore != "" {
			cleared[key] = restore
		}
		delete(s.env, key)
	}

	// Recorded before the merge, so disconnect can put the previous values back.
	j := harness.NewJournal(c.Name(), path, s.env, env, cleared, clearedSettings, previousJournal)
	j.ProjectID = e.ProjectID
	for key, value := range settings {
		j.InstalledSettings[key] = value
		current := stringSetting(s.root, key)
		// While the file still holds what the earlier connect installed, keep that connect's pre-terma
		// value, or disconnect would "restore" terma's own deleted helper path.
		if previousJournal != nil {
			if priorInstalled, owned := previousJournal.InstalledSettings[key]; owned && current == priorInstalled {
				j.PreviousSettings[key] = harness.CloneString(previousJournal.PreviousSettings[key])
				continue
			}
		}
		if current != "" {
			current := current
			j.PreviousSettings[key] = &current
		} else {
			j.PreviousSettings[key] = nil
		}
	}

	// The helper first: the settings file about to be written points at it.
	if e.HelperPath != "" {
		if err := harness.WriteHelper(e.HelperPath, e.APIKey); err != nil {
			return err
		}
	}

	// The journal before the settings, so a crash never leaves an unjournaled credential.
	if err := j.Save(c.dir); err != nil {
		return err
	}
	s.merge(env)
	for key, value := range settings {
		encoded, err := hookmgr.MarshalJSON(value, "", "")
		if err != nil {
			return err
		}
		s.root[key] = encoded
	}
	// Tighten the mode only for an inline key; otherwise the user's own mode (a committed 0644) survives.
	_, inlineKey := env[harness.EnvOTLPHeaders]
	defer harness.PruneJournals(c.dir)
	if err := s.save(inlineKey); err != nil {
		// Restore the previous journal so a failed reconnect keeps the current installation's record.
		var rollbackErr error
		if previousJournal != nil {
			rollbackErr = previousJournal.Save(c.dir)
		} else {
			rollbackErr = harness.DeleteJournal(c.dir, c.Name(), path)
		}
		if rollbackErr != nil {
			return fmt.Errorf("write settings: %w (also failed to restore telemetry journal: %w)", err, rollbackErr)
		}
		return err
	}
	return nil
}

// Disconnect undoes the recorded connect, leaving any key edited since alone. Without a journal,
// a repository policy loses only values terma writes (renderedByTerma); user settings are untouched.
func (c exporter) Disconnect() (harness.DisconnectResult, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return harness.DisconnectResult{}, err
	}
	j, err := harness.LoadJournal(c.dir, c.Name(), path)
	if err != nil {
		return harness.DisconnectResult{}, err
	}
	// Setup undoes every agent it was not asked for: settings terma never wrote are not parsed.
	if j == nil && c.root == "" {
		return harness.DisconnectResult{}, nil
	}
	s, err := loadSettings(path)
	if err != nil {
		return harness.DisconnectResult{}, err
	}

	var result harness.DisconnectResult
	var remaining *harness.Journal
	switch {
	case j != nil:
		result, remaining = j.Apply(s.env)
		// The helper file goes with its setting: it holds a live key.
		for key, installed := range j.InstalledSettings {
			current := stringSetting(s.root, key)
			if current != installed {
				if current != "" {
					result.Skipped = append(result.Skipped, key)
					remaining.InstalledSettings[key] = installed
					remaining.PreviousSettings[key] = harness.CloneString(j.PreviousSettings[key])
				}
				continue
			}
			if key == claudeOtelHeadersHelper && harness.IsOwnHelper(c.dir, installed) {
				if err := harness.DeleteHelper(installed); err != nil {
					return result, err
				}
			}
			if prior := j.PreviousSettings[key]; prior != nil {
				encoded, err := hookmgr.MarshalJSON(*prior, "", "")
				if err != nil {
					return result, err
				}
				s.root[key] = encoded
				result.Restored++
			} else {
				delete(s.root, key)
				result.Removed++
			}
		}
		for key, value := range j.ClearedSettings {
			if _, taken := s.root[key]; taken {
				result.Skipped = append(result.Skipped, key)
				remaining.ClearedSettings[key] = value
				continue
			}
			encoded, err := hookmgr.MarshalJSON(value, "", "")
			if err != nil {
				return result, err
			}
			s.root[key] = encoded
			result.Restored++
		}
	case c.root != "":
		var rendered []string
		for _, key := range c.managedKeys() {
			if value, ok := s.env[key]; ok && renderedByTerma(key, value) {
				rendered = append(rendered, key)
			}
		}
		result.Removed = s.remove(rendered)
		result.Unjournaled = result.Removed > 0
	}

	slices.Sort(result.Skipped)
	defer harness.PruneJournals(c.dir)
	if result.Removed > 0 || result.Restored > 0 {
		if err := s.save(false); err != nil {
			return result, err
		}
	}

	if j == nil {
		return result, nil
	}
	if remaining != nil && !remaining.Empty() {
		return result, remaining.Save(c.dir)
	}
	return result, harness.DeleteJournal(c.dir, c.Name(), path)
}
