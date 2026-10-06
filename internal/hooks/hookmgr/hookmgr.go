// Package hookmgr plans and applies terma's entries in agents' hooks files. Every entry is
// a guarded command that calls `terma hook <event>` and never fails, so all logic stays in
// the binary.
package hookmgr

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/project"
)

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

// plainCommandPath is a drive path PowerShell and cmd both read, unquoted, as one command
// token: no space, quote, brace or other character either shell gives meaning to.
var plainCommandPath = regexp.MustCompile(`^[A-Za-z]:(\\[A-Za-z0-9._-]+)+$`)

// userHookParts takes UserHookCommand's output apart: terma's quoted path, twice, and the event.
var userHookParts = regexp.MustCompile(`^\[ -x ('(?:[^']|'\\'')+') \] && ('(?:[^']|'\\'')+') hook --user ([a-z0-9-]+) \|\| true$`)

// SystemCmd is Windows' command interpreter by absolute path, since cmd resolves a bare
// name from the working directory first, and that may be a checkout.
func SystemCmd() string {
	return cmp.Or(os.Getenv("SystemRoot"), `C:\Windows`) + `\System32\cmd.exe`
}

// WindowsHookCommand is the native Windows twin of a UserHookCommand entry, for an agent
// that runs it with PowerShell's -Command or with cmd /C "…". The one line parses under
// both and hands itself to cmd, here the interpreter at cmdPath, which runs terma when it
// is there and then exits 0 either way: PowerShell reads "`&" as a plain "&" for cmd, and
// cmd /C splits at it. False for any other command, the managed one included, and for a
// path one of the shells would rewrite.
func WindowsHookCommand(cmdPath, command string) (string, bool) {
	m := userHookParts.FindStringSubmatch(command)
	if m == nil || m[1] != m[2] {
		return "", false
	}
	terma := strings.ReplaceAll(m[1][1:len(m[1])-1], `'\''`, "'")
	// cmdPath leads the line unquoted: a quoted one is a string to PowerShell, not a command.
	if !shellSafe(terma) || !plainCommandPath.MatchString(cmdPath) {
		return "", false
	}
	q := `"` + terma + `"`
	return cmdPath + " /d /c if exist " + q + " " + q + " hook --user " + m[3] + " `& exit 0", true
}

// shellSafe reports whether path holds only characters PowerShell and cmd both pass on
// as written: inside double quotes PowerShell expands $ and `, cmd expands % and !, and
// PowerShell reads typographic quotes as quotes. Parentheses need a space, since only then
// does PowerShell quote the argument it hands cmd.
func shellSafe(path string) bool {
	spaced := strings.Contains(path, " ")
	for _, r := range path {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), strings.ContainsRune(` \:._-'~+#@[]{}`, r):
		case (r == '(' || r == ')') && spaced:
		default:
			return false
		}
	}
	return true
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
	return apply(root, p, false)
}

// ApplyThroughLinks is Apply for the developer's own settings: a symlinked file is written through, keeping the link.
func ApplyThroughLinks(root string, p Plan) error {
	for _, c := range p.Changes {
		if err := project.CheckEscape(filepath.FromSlash(c.Path)); err != nil {
			return err
		}
	}
	return apply(root, p, true)
}

func apply(root string, p Plan, throughLinks bool) error {
	for _, c := range p.Changes {
		path := filepath.Join(root, filepath.FromSlash(c.Path))
		if throughLinks {
			resolved, linked, err := config.ResolveWritePath(path)
			if err != nil {
				return err
			}
			path = resolved
			// Deleting the target would leave the link dangling.
			if linked && c.After == nil {
				c.After = []byte("{}\n")
			}
		}
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
