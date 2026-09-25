package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// Claude Code's statusLine is one command that draws one row, and many people
// have one they care about: a script from a dotfiles repository, ccstatusline
// through npx or bun, something hand-written with jq. Terma needs to see the
// payload that command receives, because it carries the provider's own
// rate-limit windows, and the only way to see it is to be that command. So
// `terma connect claude` puts `terma hook statusline` in front of whatever was
// configured and records what it replaced; the hook runs the previous command
// with the same bytes and everything else untouched (internal/hookrun/
// statusline.go), and `terma disconnect claude` puts the previous entry back.
//
// The rules that keep this invisible:
//
//   - Only the `command` changes. padding, refreshInterval, hideVimModeIndicator
//     and any option added later are copied onto Terma's entry verbatim, so
//     Claude Code lays the output out exactly as before.
//   - The installed command falls back to the previous one by itself when terma
//     is not on the PATH: `command -v terma ... && exec terma hook statusline ||
//     exec /bin/sh -c '<previous>'`. Uninstalling the binary without
//     disconnecting leaves the status line working.
//   - The previous entry is recorded whole, in Terma's own directory, and
//     restored only while the file still holds what Terma wrote. An entry the
//     user has since replaced is theirs and is left alone; status and doctor
//     report that capture has stopped rather than fighting them for it.
//   - A statusLine in a project or local settings file, or in a managed file,
//     outranks the user file; Terma neither writes there nor complains, it
//     reports the override so the missing capture has a name.

const (
	claudeStatusLineKey = "statusLine"
	// statusLineMarker identifies Terma's command however it is guarded.
	statusLineMarker = "terma hook statusline"
	// statusLineRecordFile keeps, per Claude config path, the entry Terma
	// installed and the one it replaced.
	statusLineRecordFile = "statusline.json"
)

// statusLineRecord is one wrapped config.
type statusLineRecord struct {
	// Installed is the statusLine object Terma wrote.
	Installed json.RawMessage `json:"installed"`
	// Previous is the object it replaced, or null when there was none.
	Previous json.RawMessage `json:"previous"`
}

// StatusLineState is what a config's status line looks like to Terma.
type StatusLineState struct {
	ConfigPath string
	// Installed: the file holds Terma's command.
	Installed bool
	// Renderer is the previous command Terma passes through ("" when none).
	Renderer string
	// Replaced: Terma wrapped this file once, and the file now holds a status
	// line that is not Terma's. Capture has stopped; the user's entry stands.
	Replaced bool
	// Overrides lists settings files that outrank this one and define their own
	// statusLine, so Terma's never runs there.
	Overrides []string
}

// StatusLineCommand is the command Terma installs. previous is the command it
// falls back to when terma is not installed; empty draws nothing in that case.
func StatusLineCommand(previous string) string {
	// /bin/sh by absolute path: the fallback runs precisely when the PATH is not
	// what it was, and every POSIX system has that one.
	fallback := "exit 0"
	if strings.TrimSpace(previous) != "" {
		fallback = "exec /bin/sh -c " + shellSingleQuote(previous)
	}
	return "command -v terma >/dev/null 2>&1 && exec " + statusLineMarker + " || " + fallback
}

// shellSingleQuote wraps s in single quotes for a POSIX shell; an embedded
// quote becomes the '\” sequence. Newlines survive inside single quotes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// IsStatusLineCommand recognizes our installed guard and direct invocations.
// A renderer may legitimately print our command name; a substring match would
// silently discard such a renderer or mistake it for an existing installation.
func IsStatusLineCommand(command string) bool {
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

func isStatusLineOurs(command string) bool { return IsStatusLineCommand(command) }

// statusLineEntry is the parsed object, keeping unknown options as raw JSON.
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

// InstallStatusLine puts Terma's command in front of the configured one in the
// user's Claude settings. It is idempotent: a file already holding Terma's
// command is left as it is. It returns whether the file changed.
func (c Claude) InstallStatusLine() (bool, error) {
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
	cmdJSON, _ := marshalJSON(StatusLineCommand(previousCommand), "")
	entry["command"] = cmdJSON
	installed, err := marshalJSON(entry, "")
	if err != nil {
		return false, err
	}

	// Record first: the moment the settings change, Claude Code runs the new
	// command, and the hook must already know what to pass through to.
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

// RefreshStatusLine rewrites Terma's command in the user's Claude settings when it is
// an older form of the one this build installs, falling back to the same recorded
// renderer. It never installs one: a file without Terma's command, or a wrap with no
// record of what it replaced, is left as it is. It returns the settings path and
// whether the file changed.
func (c Claude) RefreshStatusLine() (string, bool, error) {
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
	want := StatusLineCommand(previousCommand)
	if cur.Command == want {
		return path, false, nil
	}
	entry := maps.Clone(cur.Options)
	entry["type"] = json.RawMessage(`"command"`)
	entry["command"], _ = marshalJSON(want, "")
	installed, err := marshalJSON(entry, "")
	if err != nil {
		return path, false, err
	}
	// Record first, as InstallStatusLine does: the hook reads it the moment the
	// settings change.
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

// RemoveStatusLine restores the entry Terma replaced, if the file still holds
// Terma's command. It returns whether the file changed.
func (c Claude) RemoveStatusLine() (bool, error) {
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
		// Nothing of Terma's in the file. A leftover record describes a wrap the
		// user has since undone or replaced; it has nothing left to restore.
		return false, deleteStatusLineRecord(path)
	}
	switch {
	case rec == nil:
		// A manually copied Terma command has no displaced renderer to restore.
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

// StatusLineState reports the file's status line as Terma sees it. cwd names
// the repository whose project and local settings are checked for an override.
func (c Claude) StatusLineState(cwd string) (StatusLineState, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return StatusLineState{}, err
	}
	st := StatusLineState{ConfigPath: path}
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

// StatusLineRenderer is what the hook passes through to: the command Terma
// replaced in the Claude config in effect for this process.
func StatusLineRenderer() (string, error) {
	path, err := Claude{}.ConfigPath()
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

// statusLineOverrides lists the settings files that outrank the user file and
// define their own statusLine: the repository's project and local settings, and
// the platform's managed settings file.
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

// --- record file -------------------------------------------------------------

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
	return config.WriteJSON(path, recs, settingsMode)
}

func loadStatusLineRecord(configPath string) (*statusLineRecord, error) {
	recs, err := loadStatusLineRecords()
	if err != nil {
		return nil, err
	}
	return recs[configPath], nil
}

// updateStatusLineRecords is the one way the record file changes: load, edit, save,
// under a lock. It is one file for every Claude config on the machine, so two installs
// under different CLAUDE_CONFIG_DIRs each read it, set their own entry and renamed
// their copy back, and the later rename forgot what the other had displaced.
func updateStatusLineRecords(edit func(map[string]*statusLineRecord)) error {
	path, err := statusLineRecordPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordLockWait)
	defer cancel()
	unlock, err := flock.Lock(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	recs, err := loadStatusLineRecords()
	if err != nil {
		return err
	}
	edit(recs)
	return saveStatusLineRecords(recs)
}

func saveStatusLineRecord(configPath string, rec *statusLineRecord) error {
	return updateStatusLineRecords(func(recs map[string]*statusLineRecord) { recs[configPath] = rec })
}

func deleteStatusLineRecord(configPath string) error {
	return updateStatusLineRecords(func(recs map[string]*statusLineRecord) { delete(recs, configPath) })
}
