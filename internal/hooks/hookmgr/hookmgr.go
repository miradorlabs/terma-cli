// Package hookmgr plans and applies terma's entries in agents' hooks files. Every entry is
// a guarded command that calls `terma hook <event>` and never fails, so all logic stays in
// the binary.
package hookmgr

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/project"
)

// Marker identifies lines terma wrote, so their removal takes only its own.
const Marker = "terma hook"

// HookCommand is the guarded command every committed agent hook entry runs; it prints
// nothing without terma, since an agent may hand a hook's output to the model. Changing
// it changes every committed hooks file and any hash-keyed trust a developer granted.
func HookCommand(event string) string {
	return "command -v terma >/dev/null 2>&1 && terma hook " + event + " || true"
}

// PathHookCommand is HookCommand behind the small GUI PATH a desktop- or IDE-launched agent
// gets, extended with the directories terma installs to, portable across developers since
// the entry is committed. A hooks file switching to it changes its bytes, and so any trust
// granted to it.
func PathHookCommand(event string) string {
	return `PATH="${PATH:-/usr/bin:/bin}:` + strings.Join(hookPathDirs, ":") + `"; ` + HookCommand(event)
}

// hookPathDirs are the directories PathHookCommand adds, in its order.
var hookPathDirs = []string{"$HOME/.local/bin", "/opt/homebrew/bin", "/usr/local/bin"}

// HookPathDirs are the directories PathHookCommand adds to PATH, with $HOME as home.
func HookPathDirs(home string) []string {
	dirs := make([]string, len(hookPathDirs))
	for i, d := range hookPathDirs {
		dirs[i] = d
		if rest, ok := strings.CutPrefix(d, "$HOME/"); ok {
			dirs[i] = filepath.Join(home, rest)
		}
	}
	return dirs
}

// HookEventOf is the `terma hook <event>` name a committed command runs.
func HookEventOf(command string) string {
	_, rest, ok := strings.Cut(command, "terma hook ")
	if !ok {
		return ""
	}
	event, _, _ := strings.Cut(rest, " ")
	return event
}

// UserHookCommand is the command a machine-wide hook entry runs: terma by absolute path,
// since a desktop-started agent has the system PATH, with --user.
func UserHookCommand(terma string) func(event string) string {
	return userHookCommand("'" + strings.ReplaceAll(terma, "'", `'\''`) + "'")
}

// userHookCommand runs terma, quoted as q, when it is there to run.
func userHookCommand(q string) func(event string) string {
	return func(event string) string {
		return "[ -x " + q + " ] && " + q + " hook --user " + event + " || true"
	}
}

// Change is one file the plan touches. Before is nil for a new file; After is nil
// for a deletion.
type Change struct {
	Path   string // relative to root
	Before []byte
	After  []byte
	Mode   fs.FileMode
}

// Action names what the change does.
func (c Change) Action() string {
	switch {
	case c.Before == nil && c.After != nil:
		return "create"
	case c.After == nil:
		return "delete"
	default:
		return "modify"
	}
}

// Plan is the set of file changes plus what the user still has to do by hand.
type Plan struct {
	Changes []Change
	Notes   []string
}

// Empty reports whether the plan changes nothing.
func (p Plan) Empty() bool { return len(p.Changes) == 0 }

// Validate checks all destinations before applying any part of a plan.
func Validate(root string, p Plan) error {
	for _, c := range p.Changes {
		if err := project.CheckPath(root, filepath.FromSlash(c.Path)); err != nil {
			return err
		}
	}
	return nil
}

// Apply writes every change atomically (temp file + rename per file).
func Apply(root string, p Plan) error {
	if err := Validate(root, p); err != nil {
		return err
	}
	for _, c := range p.Changes {
		path := filepath.Join(root, filepath.FromSlash(c.Path))
		if c.After == nil {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			removeEmptyParents(root, filepath.Dir(path))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		mode := c.Mode
		if mode == 0 {
			mode = 0o644
			if info, err := os.Stat(path); err == nil {
				mode = info.Mode().Perm()
			}
		}
		if err := config.WriteFileAtomic(path, c.After, mode); err != nil {
			return err
		}
	}
	return nil
}

func removeEmptyParents(root, dir string) {
	root = filepath.Clean(root)
	for dir = filepath.Clean(dir); dir != root && strings.HasPrefix(dir, root); dir = filepath.Dir(dir) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if os.Remove(dir) != nil {
			return
		}
	}
}

// ReadFile returns a file's bytes, or nil when it does not exist; any other failure is an
// error, since planning an unreadable file as a create would overwrite it.
func ReadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}
