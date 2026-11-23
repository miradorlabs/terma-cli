package antigravity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/miradorlabs/terma-cli/internal/agents/internal/statusline"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/tailscale/hujson"
)

// agy's status line payload carries the plan, its quota buckets and the signed-in email, and
// the only way to see it is to be the statusLine command. So terma puts `terma hook
// antigravity-statusline` in agy's settings, in front of the developer's own command, which
// it runs with the same bytes; with none, terma's draws nothing and stacks under agy's
// built-in line, which stays as it was. The settings file is edited member by member, so
// the rest of it is kept byte for byte, and teardown puts back the file as it was when
// nothing else changed it since, else the statusLine member as it was.

const (
	antigravityStatusLineKey = "statusLine"
	// antigravityStatusLineEvent is the render hook terma's command runs.
	antigravityStatusLineEvent  = "antigravity-statusline"
	antigravityStatusLineMarker = "terma hook " + antigravityStatusLineEvent
	// antigravityStatusLineRecord keeps, per settings path, what terma installed and replaced.
	antigravityStatusLineRecord = "antigravity-statusline.json"
)

// antigravityStatusLineRecordEntry is what terma installed in one settings file, and what it
// replaced there.
type antigravityStatusLineRecordEntry struct {
	// Previous is the statusLine value terma replaced, as written, "" when there was none;
	// text, since the record's own indentation would reformat raw JSON.
	Previous string `json:"previous,omitempty"`
	// Renderer is the command terma runs behind its own, "" when none ran before.
	Renderer string `json:"renderer,omitempty"`
	// Before is the whole file before terma's edit; Existed says whether there was one.
	Before  []byte `json:"before,omitempty"`
	Existed bool   `json:"existed"`
	// After hashes the file as terma wrote it: a file still that way is put back as Before.
	After string `json:"after"`
}

// antigravitySettingsPath is agy's user settings file.
func antigravitySettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), err
}

// antigravityStatusLineCommand is the installed command: terma when it is on the PATH, else
// the developer's own, else nothing.
func antigravityStatusLineCommand(previous string) string {
	// /bin/sh by absolute path: the fallback runs precisely when the PATH is not what it was.
	fallback := "exit 0"
	if strings.TrimSpace(previous) != "" {
		fallback = "exec /bin/sh -c " + statusline.Quote(previous)
	}
	return "command -v terma >/dev/null 2>&1 && exec " + antigravityStatusLineMarker + " || " + fallback
}

// isAntigravityStatusLineOurs recognizes the installed guard and a direct invocation, never
// a substring: a renderer may print terma's command name.
func isAntigravityStatusLineOurs(command string) bool {
	command = strings.TrimSpace(command)
	if strings.HasPrefix(command, "command -v terma >/dev/null 2>&1 && exec "+antigravityStatusLineMarker+" || ") {
		return true
	}
	fields := strings.Fields(command)
	if len(fields) > 0 && fields[0] == "exec" {
		fields = fields[1:]
	}
	return len(fields) >= 3 && fields[0] == "terma" && fields[1] == "hook" && fields[2] == antigravityStatusLineEvent
}

// antigravityStatusLineEntry is a statusLine member's fields, each as written.
type antigravityStatusLineEntry map[string]json.RawMessage

func (s antigravityStatusLineEntry) str(key string) (string, error) {
	raw, ok := s[key]
	if !ok {
		return "", nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("statusLine.%s is not a string", key)
	}
	return v, nil
}

// disabled is enabled: false, under which agy draws only its built-in line.
func (s antigravityStatusLineEntry) disabled() bool {
	var on bool
	raw, ok := s["enabled"]
	return ok && json.Unmarshal(raw, &on) == nil && !on
}

func parseAntigravityStatusLine(raw string) (antigravityStatusLineEntry, error) {
	if raw == "" || raw == "null" {
		return nil, nil
	}
	std, err := hujson.Standardize([]byte(raw))
	if err != nil {
		return nil, errors.New("statusLine is not an object")
	}
	var s antigravityStatusLineEntry
	if err := json.Unmarshal(std, &s); err != nil || s == nil {
		return nil, errors.New("statusLine is not an object")
	}
	return s, nil
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// InstallStatusLine puts terma's command in agy's status line, in front of the developer's
// own, idempotently, and reports whether the file changed.
func (a Agent) InstallStatusLine() (bool, error) {
	if runtime.GOOS == "windows" {
		// agy runs a status line there with cmd; terma's command is POSIX.
		return false, errors.New("the status line of agy is not wrapped on Windows")
	}
	path, err := antigravitySettingsPath()
	if err != nil {
		return false, err
	}
	s, err := loadAntigravitySettings(path)
	if err != nil {
		return false, err
	}
	previous := s.raw(antigravityStatusLineKey)
	prev, err := parseAntigravityStatusLine(previous)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	renderer := ""
	entry := map[string]json.RawMessage{}
	if prev != nil {
		typ, err := prev.str("type")
		if err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
		if typ != "" && typ != "command" {
			return false, fmt.Errorf("%s: statusLine.type %q is not one Terma can wrap", path, typ)
		}
		command, err := prev.str("command")
		if err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
		if isAntigravityStatusLineOurs(command) {
			if !prev.disabled() {
				return false, nil
			}
			// agy draws nothing of a line set "enabled": false: setup switches terma's back on.
			delete(prev, "enabled")
			return a.rewriteStatusLine(path, s, prev)
		}
		maps.Copy(entry, prev)
		if !prev.disabled() {
			renderer = command
		}
	}
	if renderer == "" {
		// Nothing of the developer's draws: terma's command prints nothing, under agy's own line.
		delete(entry, "enabled")
		entry["stack_with_default"] = json.RawMessage("true")
	}
	entry["type"] = json.RawMessage(`"command"`)
	entry["command"], _ = hookmgr.MarshalJSON(antigravityStatusLineCommand(renderer), "", "")
	installed, err := hookmgr.MarshalJSON(entry, "", "")
	if err != nil {
		return false, err
	}
	if err := s.set(antigravityStatusLineKey, string(installed)); err != nil {
		return false, err
	}
	after := s.pack()
	rec := antigravityStatusLineRecordEntry{Previous: previous, Renderer: renderer, Before: s.data, Existed: s.existed, After: hashBytes(after)}
	// Record first: agy runs the new command the moment the settings change.
	if err := a.updateStatusLineRecords(func(recs map[string]*antigravityStatusLineRecordEntry) { recs[path] = &rec }); err != nil {
		return false, err
	}
	if err := s.write(after); err != nil {
		return false, err
	}
	return true, nil
}

// RefreshMachine rewrites what terma wrote into agy's files to this build's: its hooks in
// the hooks file and its status line command, each only where terma wrote it, carrying on
// past a failure of either. It names each file it changed.
func (a Agent) RefreshMachine() (string, bool, error) {
	var changed []string
	var errs []error
	for _, refresh := range []func() (string, bool, error){a.refreshHooksFile, a.refreshStatusLine} {
		path, ok, err := refresh()
		if err != nil {
			errs = append(errs, err)
		} else if ok {
			changed = append(changed, path)
		}
	}
	return strings.Join(changed, ", "), len(changed) > 0, errors.Join(errs...)
}

// refreshStatusLine rewrites an older form of terma's status line command, never
// installing one, and leaves a line switched off as it is.
func (a Agent) refreshStatusLine() (string, bool, error) {
	path, err := antigravitySettingsPath()
	if err != nil || runtime.GOOS == "windows" {
		return path, false, err
	}
	rec, err := a.statusLineRecord(path)
	if err != nil || rec == nil {
		return path, false, err
	}
	s, err := loadAntigravitySettings(path)
	if err != nil {
		return path, false, err
	}
	cur, err := parseAntigravityStatusLine(s.raw(antigravityStatusLineKey))
	if err != nil || cur == nil {
		return path, false, nil
	}
	command, _ := cur.str("command")
	want := antigravityStatusLineCommand(rec.Renderer)
	if !isAntigravityStatusLineOurs(command) || command == want {
		return path, false, nil
	}
	cur["command"], _ = hookmgr.MarshalJSON(want, "", "")
	changed, err := a.rewriteStatusLine(path, s, cur)
	return path, changed, err
}

// rewriteStatusLine writes entry, terma's own status line changed in place, and keeps the
// record able to restore the file: whole while nothing else changed it since setup, else
// the statusLine member alone.
func (a Agent) rewriteStatusLine(path string, s *antigravitySettings, entry antigravityStatusLineEntry) (bool, error) {
	installed, err := hookmgr.MarshalJSON(map[string]json.RawMessage(entry), "", "")
	if err != nil {
		return false, err
	}
	if err := s.set(antigravityStatusLineKey, string(installed)); err != nil {
		return false, err
	}
	after := s.pack()
	rec, err := a.statusLineRecord(path)
	if err != nil {
		return false, err
	}
	if rec != nil {
		if hashBytes(s.data) == rec.After {
			rec.After = hashBytes(after)
		} else {
			rec.After, rec.Before = "", nil
		}
		if err := a.updateStatusLineRecords(func(recs map[string]*antigravityStatusLineRecordEntry) { recs[path] = rec }); err != nil {
			return false, err
		}
	}
	return true, s.write(after)
}

// RemoveStatusLine puts back what terma replaced, if the file still holds terma's command:
// the whole file when nothing else changed it since, else the statusLine member alone.
func (a Agent) RemoveStatusLine() (bool, error) {
	path, err := antigravitySettingsPath()
	if err != nil {
		return false, err
	}
	rec, err := a.statusLineRecord(path)
	if err != nil {
		return false, err
	}
	s, err := loadAntigravitySettings(path)
	var cur antigravityStatusLineEntry
	if err == nil {
		if cur, err = parseAntigravityStatusLine(s.raw(antigravityStatusLineKey)); err != nil {
			err = fmt.Errorf("%s: %w", path, err)
		}
	}
	if err != nil {
		// Teardown, and setup for any other agent, run this: a file terma cannot read that
		// holds nothing of terma's is not its to judge.
		if data, readErr := os.ReadFile(path); readErr != nil || !bytes.Contains(data, []byte(antigravityStatusLineMarker)) {
			if rec == nil {
				return false, nil
			}
			return false, a.deleteStatusLineRecord(path)
		}
		return false, err
	}
	command, _ := cur.str("command")
	if cur == nil || !isAntigravityStatusLineOurs(command) {
		// A leftover record describes a wrap the developer has since undone.
		return false, a.deleteStatusLineRecord(path)
	}
	switch {
	case rec != nil && s.existed && hashBytes(s.data) == rec.After && !rec.Existed:
		if err := os.Remove(s.writePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	case rec != nil && s.existed && hashBytes(s.data) == rec.After:
		if err := s.write(rec.Before); err != nil {
			return false, err
		}
	default:
		if rec == nil || rec.Previous == "" {
			s.remove(antigravityStatusLineKey)
		} else if err := s.set(antigravityStatusLineKey, rec.Previous); err != nil {
			return false, err
		}
		if err := s.write(s.pack()); err != nil {
			return false, err
		}
	}
	return true, a.deleteStatusLineRecord(path)
}

// StatusLineState is agy's status line as terma sees it. agy has no settings file that
// outranks the user's, but a session's --settings, which is never saved.
func (a Agent) StatusLineState(string) (agents.StatusLineState, error) {
	path, err := antigravitySettingsPath()
	st := agents.StatusLineState{ConfigPath: path}
	if err != nil {
		return st, err
	}
	s, err := loadAntigravitySettings(path)
	if err != nil {
		return st, err
	}
	rec, err := a.statusLineRecord(path)
	if err != nil {
		return st, err
	}
	cur, err := parseAntigravityStatusLine(s.raw(antigravityStatusLineKey))
	if err != nil {
		return st, err
	}
	command, _ := cur.str("command")
	switch {
	case cur != nil && isAntigravityStatusLineOurs(command) && !cur.disabled():
		st.Installed = true
		if rec != nil {
			st.Renderer, st.Stacked = rec.Renderer, rec.Renderer == ""
		}
	case rec != nil:
		st.Replaced = true
	}
	return st, nil
}

// statusLineRenderer is the command terma runs behind its own, from the records under dir.
func statusLineRenderer(dir string) (string, error) {
	path, err := antigravitySettingsPath()
	if err != nil {
		return "", err
	}
	rec, err := Agent{ConfigDir: dir}.statusLineRecord(path)
	if err != nil || rec == nil {
		return "", err
	}
	return rec.Renderer, nil
}
