package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// The status-line payload carries the plan's rate-limit windows, and the only way to see it is to
// be the statusLine command, so terma puts `terma hook statusline` in front of the configured one
// (statusline_hook.go), copies every other option, falls back to the previous command when terma
// is not on the PATH, and restores the entry only while the file still holds what terma wrote.

const (
	claudeStatusLineKey = "statusLine"
	statusLineMarker    = "terma hook statusline"
	// statusLineRecordFile keeps, per config path, the entry terma installed and the one it replaced.
	statusLineRecordFile = "statusline.json"
)

type statusLineRecord struct {
	Installed json.RawMessage `json:"installed"`
	// Previous is null when there was none.
	Previous json.RawMessage `json:"previous"`
}

// statusLineCommand is the installed command; previous is its fallback when terma is not installed.
func statusLineCommand(previous string) string {
	// /bin/sh by absolute path: the fallback runs precisely when the PATH is not what it was.
	fallback := "exit 0"
	if strings.TrimSpace(previous) != "" {
		fallback = "exec /bin/sh -c " + shellSingleQuote(previous)
	}
	return "command -v terma >/dev/null 2>&1 && exec " + statusLineMarker + " || " + fallback
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isStatusLineCommand recognizes the installed guard and direct invocations, never a substring:
// a renderer may print terma's command name.
func isStatusLineCommand(command string) bool {
	command = strings.TrimSpace(command)
	if strings.HasPrefix(command, "command -v terma >/dev/null 2>&1 && exec "+statusLineMarker+" || ") {
		return true
	}
	fields := strings.Fields(command)
	if len(fields) > 0 && fields[0] == "exec" {
		fields = fields[1:]
	}
	return len(fields) >= 3 && fields[0] == "terma" && fields[1] == "hook" && fields[2] == "statusline"
}

func isStatusLineOurs(command string) bool { return isStatusLineCommand(command) }

type statusLineEntry struct {
	Type    string
	Command string
	Options map[string]json.RawMessage
}

func parseStatusLine(raw json.RawMessage) (*statusLineEntry, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("statusLine is not an object: %w", err)
	}
	e := &statusLineEntry{Options: map[string]json.RawMessage{}}
	for k, v := range obj {
		switch k {
		case "type":
			_ = json.Unmarshal(v, &e.Type)
		case "command":
			if err := json.Unmarshal(v, &e.Command); err != nil {
				return nil, fmt.Errorf("statusLine.command is not a string")
			}
		default:
			e.Options[k] = v
		}
	}
	return e, nil
}

// InstallStatusLine puts terma's command in front of the configured one in the user settings,
// idempotently, and reports whether the file changed.
func (c exporter) InstallStatusLine() (bool, error) {
	if c.root != "" {
		return false, errors.New("the status line is a user setting; Terma does not write it into a repository")
	}
	path, err := c.ConfigPath()
	if err != nil {
		return false, err
	}
	s, err := loadSettings(path)
	if err != nil {
		return false, err
	}
	raw := s.root[claudeStatusLineKey]
	prev, err := parseStatusLine(raw)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if prev != nil && prev.Type != "" && prev.Type != "command" {
		return false, fmt.Errorf("%s: statusLine.type %q is not one Terma can wrap", path, prev.Type)
	}
	if prev != nil && isStatusLineOurs(prev.Command) {
		return false, nil
	}

	entry := map[string]json.RawMessage{}
	previousCommand := ""
	if prev != nil {
		maps.Copy(entry, prev.Options)
		previousCommand = prev.Command
	}
	entry["type"] = json.RawMessage(`"command"`)
	cmdJSON, _ := hookmgr.MarshalJSON(statusLineCommand(previousCommand), "", "")
	entry["command"] = cmdJSON
	installed, err := hookmgr.MarshalJSON(entry, "", "")
	if err != nil {
		return false, err
	}

	// Record first: Claude Code runs the new command the moment the settings change.
	rec := statusLineRecord{Installed: installed, Previous: raw}
	if len(raw) == 0 {
		rec.Previous = json.RawMessage("null")
	}
	if err := saveStatusLineRecord(path, &rec); err != nil {
		return false, err
	}
	s.root[claudeStatusLineKey] = installed
	if err := s.save(false); err != nil {
		return false, err
	}
	return true, nil
}

// RefreshStatusLine rewrites an older form of terma's command and never installs one; it returns
// the settings path and whether the file changed.
func (c exporter) RefreshStatusLine() (string, bool, error) {
	if c.root != "" {
		return "", false, nil
	}
	path, err := c.ConfigPath()
	if err != nil {
		return "", false, err
	}
	s, err := loadSettings(path)
	if err != nil {
		return path, false, err
	}
	cur, err := parseStatusLine(s.root[claudeStatusLineKey])
	if err != nil || cur == nil || !isStatusLineOurs(cur.Command) {
		return path, false, nil
	}
	rec, err := loadStatusLineRecord(path)
	if err != nil || rec == nil {
		return path, false, err
	}
	prev, err := parseStatusLine(rec.Previous)
	if err != nil {
		return path, false, fmt.Errorf("%s: %w", statusLineRecordFile, err)
	}
	previousCommand := ""
	if prev != nil {
		previousCommand = prev.Command
	}
	want := statusLineCommand(previousCommand)
	if cur.Command == want {
		return path, false, nil
	}
	entry := maps.Clone(cur.Options)
	entry["type"] = json.RawMessage(`"command"`)
	entry["command"], _ = hookmgr.MarshalJSON(want, "", "")
	installed, err := hookmgr.MarshalJSON(entry, "", "")
	if err != nil {
		return path, false, err
	}
	// Record first, as InstallStatusLine does.
	rec.Installed = installed
	if err := saveStatusLineRecord(path, rec); err != nil {
		return path, false, err
	}
	s.root[claudeStatusLineKey] = installed
	if err := s.save(false); err != nil {
		return path, false, err
	}
	return path, true, nil
}

// RemoveStatusLine restores the entry terma replaced, if the file still holds terma's command.
func (c exporter) RemoveStatusLine() (bool, error) {
	if c.root != "" {
		return false, nil
	}
	path, err := c.ConfigPath()
	if err != nil {
		return false, err
	}
	s, err := loadSettings(path)
	if err != nil {
		return false, err
	}
	rec, err := loadStatusLineRecord(path)
	if err != nil {
		return false, err
	}
	cur, err := parseStatusLine(s.root[claudeStatusLineKey])
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if cur == nil || !isStatusLineOurs(cur.Command) {
		// A leftover record describes a wrap the user has since undone.
		return false, deleteStatusLineRecord(path)
	}
	switch {
	case rec == nil:
		delete(s.root, claudeStatusLineKey)
	case string(rec.Previous) == "null" || len(rec.Previous) == 0:
		delete(s.root, claudeStatusLineKey)
	default:
		s.root[claudeStatusLineKey] = rec.Previous
	}
	if err := s.save(false); err != nil {
		return false, err
	}
	return true, deleteStatusLineRecord(path)
}

// StatusLineState reports the status line as terma sees it; cwd names the repository checked for overrides.
func (c exporter) StatusLineState(cwd string) (agents.StatusLineState, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return agents.StatusLineState{}, err
	}
	st := agents.StatusLineState{ConfigPath: path}
	s, err := loadSettings(path)
	if err != nil {
		return st, err
	}
	rec, err := loadStatusLineRecord(path)
	if err != nil {
		return st, err
	}
	cur, err := parseStatusLine(s.root[claudeStatusLineKey])
	if err != nil {
		return st, err
	}
	if cur != nil && isStatusLineOurs(cur.Command) {
		st.Installed = true
		if rec != nil {
			if prev, err := parseStatusLine(rec.Previous); err == nil && prev != nil {
				st.Renderer = prev.Command
			}
		}
	} else if rec != nil {
		st.Replaced = true
	}
	st.Overrides = statusLineOverrides(cwd)
	return st, nil
}

// statusLineRenderer is the command terma replaced in the config in effect for this process.
func statusLineRenderer() (string, error) {
	path, err := exporter{}.ConfigPath()
	if err != nil {
		return "", err
	}
	rec, err := loadStatusLineRecord(path)
	if err != nil || rec == nil {
		return "", err
	}
	prev, err := parseStatusLine(rec.Previous)
	if err != nil || prev == nil {
		return "", err
	}
	return prev.Command, nil
}

// statusLineOverrides lists the project, local and managed settings files that define their own statusLine.
func statusLineOverrides(cwd string) []string {
	var candidates []string
	if cwd != "" {
		candidates = append(candidates,
			filepath.Join(cwd, ".claude", "settings.local.json"),
			filepath.Join(cwd, ".claude", "settings.json"))
	}
	switch runtime.GOOS {
	case "darwin":
		candidates = append(candidates, "/Library/Application Support/ClaudeCode/managed-settings.json")
	case "linux":
		candidates = append(candidates, "/etc/claude-code/managed-settings.json")
	}
	var out []string
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal(data, &doc) != nil {
			continue
		}
		if raw, ok := doc[claudeStatusLineKey]; ok && len(raw) > 0 && string(raw) != "null" {
			out = append(out, p)
		}
	}
	return out
}

func statusLineRecordPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, statusLineRecordFile), nil
}

func loadStatusLineRecords() (map[string]*statusLineRecord, error) {
	path, err := statusLineRecordPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]*statusLineRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	recs := map[string]*statusLineRecord{}
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return recs, nil
}

func saveStatusLineRecords(recs map[string]*statusLineRecord) error {
	path, err := statusLineRecordPath()
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		err := os.Remove(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return config.WriteJSON(path, recs, harness.SettingsMode)
}

func loadStatusLineRecord(configPath string) (*statusLineRecord, error) {
	recs, err := loadStatusLineRecords()
	if err != nil {
		return nil, err
	}
	return recs[configPath], nil
}

// updateStatusLineRecords edits the record file under a lock: it is one file for every config on
// the machine, and unlocked, a second CLAUDE_CONFIG_DIR's install forgot what the first displaced.
func updateStatusLineRecords(edit func(map[string]*statusLineRecord)) error {
	path, err := statusLineRecordPath()
	if err != nil {
		return err
	}
	return flock.Locked(path, harness.RecordLockWait, func() error {
		recs, err := loadStatusLineRecords()
		if err != nil {
			return err
		}
		edit(recs)
		return saveStatusLineRecords(recs)
	})
}

func saveStatusLineRecord(configPath string, rec *statusLineRecord) error {
	return updateStatusLineRecords(func(recs map[string]*statusLineRecord) { recs[configPath] = rec })
}

func deleteStatusLineRecord(configPath string) error {
	return updateStatusLineRecords(func(recs map[string]*statusLineRecord) { delete(recs, configPath) })
}
