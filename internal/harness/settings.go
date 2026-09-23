package harness

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
)

// settingsFile is a harness config file whose telemetry lives in a JSON `env` object —
// the shape Claude Code uses. Codex keeps its telemetry in a TOML table instead; see
// tomlFile.
//
// Every value is held as json.RawMessage so a merge is non-destructive: settings this
// CLI has never heard of survive byte-for-byte, including their nested key order. Only
// top-level ordering is lost, since Go marshals map keys alphabetically. That churns a
// hand-written file once and is stable forever after.
type settingsFile struct {
	path string
	// writePath is path with symlinks resolved. Writing goes here rather than to path,
	// because the atomic rename replaces whatever name it is given — and for anyone
	// keeping ~/.claude/settings.json as a link into a dotfiles repo, that would swap
	// the link for a regular file and quietly detach the file from the repo.
	writePath string
	// root is the whole document; env is the nested object telemetry keys live in.
	root map[string]json.RawMessage
	env  map[string]string
	// existed distinguishes "no telemetry configured" from "no file at all", which
	// status reports differently.
	existed bool
	// symlinked records that path is a link. A disconnect that empties the document
	// deletes the file — but deleting the *target* of a link leaves the link dangling,
	// so a linked file is emptied to `{}` instead.
	symlinked bool
	// mode is the file's mode as found, so a file that was already tighter than 0600
	// is not loosened by writing it back.
	mode fs.FileMode
}

const (
	envKey = "env"

	// settingsMode is what a settings file is written as once it holds a server key.
	// The default 0644 would leave a live credential readable by every account on the
	// machine, and the file is only ever read by the harness running as this user.
	settingsMode fs.FileMode = 0o600
)

// marshalJSON encodes without HTML escaping; an empty indent compacts. A settings file
// holds hook commands and a status line that carry `>` and `&&`, and encoding/json's
// default rewrites them as `\u003e` and `\u0026` — inside a json.RawMessage too, so a
// value this CLI never touched did not survive byte-for-byte after all. hookmgr writes
// the same file unescaped, and the two must not take turns re-encoding it.
func marshalJSON(v any, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// loadSettings reads a settings file, tolerating its absence.
func loadSettings(path string) (*settingsFile, error) {
	s := &settingsFile{
		path:      path,
		writePath: path,
		root:      map[string]json.RawMessage{},
		env:       map[string]string{},
		mode:      settingsMode,
	}

	writePath, symlinked, err := resolveWritePath(path)
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

	// An empty file is a valid starting point; json.Unmarshal would reject it.
	if len(bytes.TrimSpace(data)) == 0 {
		return s, nil
	}

	if err := json.Unmarshal(data, &s.root); err != nil {
		// Refusing here is the whole point: a merge into a file we cannot parse would
		// mean overwriting settings we cannot see.
		return nil, fmt.Errorf("parse %s: %w (fix or move the file, then retry)", path, err)
	}

	if raw, ok := s.root[envKey]; ok && len(raw) > 0 {
		// Claude Code requires env values to be strings. A file with a non-string value
		// is already broken for the harness, so say so rather than silently discarding it.
		if err := json.Unmarshal(raw, &s.env); err != nil {
			return nil, fmt.Errorf("parse %q in %s: %w (values must be strings)", envKey, path, err)
		}
	}
	if s.env == nil {
		s.env = map[string]string{}
	}
	return s, nil
}

// merge applies env, overwriting only the keys given.
func (s *settingsFile) merge(env map[string]string) {
	maps.Copy(s.env, env)
}

// remove deletes the named keys and reports how many were actually present, so a
// disconnect can tell "removed 12 settings" from "there was nothing to remove".
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

// save writes the document back atomically.
//
// tighten is set when the file now carries a credential; it clamps the mode to 0600
// rather than preserving a permissive one. A file already at 0400 keeps that.
func (s *settingsFile) save(tighten bool) error {
	// An env object emptied by disconnect is dropped entirely rather than left as `{}`,
	// so a full disconnect restores the file to what it looked like before.
	if len(s.env) == 0 {
		delete(s.root, envKey)
	} else {
		encoded, err := marshalJSON(s.env, "")
		if err != nil {
			return fmt.Errorf("encode %q: %w", envKey, err)
		}
		s.root[envKey] = encoded
	}

	// A document with nothing left in it is removed rather than written as `{}` — unless
	// it is reached through a symlink, where removing the target would leave the link
	// dangling and break the next read. There, an empty object is written instead: it
	// says the same thing and keeps the file the link points at.
	if len(s.root) == 0 && !s.symlinked {
		if !s.existed {
			return nil
		}
		if err := os.Remove(s.writePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", s.writePath, err)
		}
		return nil
	}

	data, err := marshalJSON(s.root, "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", s.writePath, err)
	}
	data = append(data, '\n')

	mode := s.mode
	if tighten && mode&0o077 != 0 {
		mode = settingsMode
	}

	if err := os.MkdirAll(filepath.Dir(s.writePath), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(s.writePath), err)
	}
	return config.WriteFileAtomic(s.writePath, data, mode)
}

// backup copies the current file alongside itself before the first modification. This
// is a user's own configuration, possibly hand-written and possibly in a dotfiles repo;
// a mangled merge should never be the only copy left.
//
// replace decides whether an existing backup may be overwritten, and the caller sets it
// from whether the current file is Terma's own work.
//
// Neither "always" nor "never" is right. Always overwriting means a re-connect replaces
// the record of the user's original collector with a copy of Terma's settings. Never
// overwriting means the record goes stale the moment the user reconfigures: connect,
// disconnect, set up a different collector, re-connect — the backup still holds the
// first configuration while the second is overwritten and then deleted, unrecoverable.
// So the rule is to snapshot whatever is not already Terma's, and leave the snapshot
// alone when re-connecting over a config this CLI wrote.
//
// Best-effort by design: a failure to write the backup must not block the connect the
// user asked for, so the caller reports it as a warning.
func (s *settingsFile) backup(replace bool) (string, error) {
	return backupFile(s.writePath, s.existed, replace)
}

// resolveWritePath is where a config file should actually be written, given the path
// the harness reads it from.
//
// A symlink is resolved so the write lands on the real file and the link survives. A
// *dangling* link is refused rather than followed: writing to the link path would
// replace it with a regular file, silently detaching a dotfiles setup from its repo,
// and there is no safe way to guess where the missing target was meant to live.
func resolveWritePath(path string) (writePath string, symlinked bool, err error) {
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
		// Not a link itself, but a parent directory may be one; resolve so the atomic
		// rename happens in the directory the file actually lives in.
		return resolved, false, nil
	}
	return path, false, nil
}

// backupFile copies writePath to writePath.terma.bak — alongside the real file, not
// the link that points at it. See settingsFile.backup for when replace is set.
func backupFile(writePath string, existed, replace bool) (string, error) {
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
	// Written 0600 regardless of the source's mode: a backup of a connected config holds
	// a server key, and inheriting a permissive mode would copy it somewhere readable.
	if err := config.WriteFileAtomic(path, data, settingsMode); err != nil {
		return "", err
	}
	return path, nil
}
