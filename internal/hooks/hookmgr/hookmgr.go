// Package hookmgr plans and applies the committed hook wiring, through whichever git hook
// manager the repository already uses. Every hook is a guarded shim that calls
// `terma hook <event>` and never fails, so all logic stays in the binary.
package hookmgr

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/project"
)

// Manager is the hook manager a repository uses.
type Manager string

// The hook managers terma installs through, in the manager's own file rather than taking over core.hooksPath.
const (
	Husky     Manager = "husky"
	Lefthook  Manager = "lefthook"
	PreCommit Manager = "pre-commit"
	// GitShim is the fallback: shims under .terma/hooks via core.hooksPath, chaining any existing hook.
	GitShim Manager = "git"
)

// GitHooks are the git hooks terma installs.
var GitHooks = []string{"prepare-commit-msg", "post-commit"}

// ShimDir is where the fallback shims live, relative to the repo root.
const ShimDir = ".terma/hooks"

// Marker identifies lines terma wrote, so uninstall removes only its own.
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

// Detection is what Detect found.
type Detection struct {
	Manager    Manager
	ConfigPath string // the file the plan will edit (relative to root), if any
	Detail     string
}

// Detect picks the hook manager from what is already in the repository; a committed
// config file wins over one merely listed in package.json.
func Detect(root string) Detection {
	if _, err := os.Stat(filepath.Join(root, ".husky")); err == nil {
		return Detection{Manager: Husky, ConfigPath: ".husky", Detail: ".husky/ directory present"}
	}
	for _, name := range []string{"lefthook.yml", "lefthook.yaml", ".lefthook.yml", ".lefthook.yaml"} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			return Detection{Manager: Lefthook, ConfigPath: name, Detail: name + " present"}
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".pre-commit-config.yaml")); err == nil {
		return Detection{Manager: PreCommit, ConfigPath: ".pre-commit-config.yaml", Detail: ".pre-commit-config.yaml present"}
	}
	if hasDevDependency(root, "husky") {
		return Detection{Manager: Husky, ConfigPath: ".husky", Detail: "husky in package.json"}
	}
	return Detection{Manager: GitShim, ConfigPath: ShimDir, Detail: "no hook manager found; using a core.hooksPath shim"}
}

func hasDevDependency(root, name string) bool {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Dev  map[string]json.RawMessage `json:"devDependencies"`
		Deps map[string]json.RawMessage `json:"dependencies"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return false
	}
	_, a := pkg.Dev[name]
	_, b := pkg.Deps[name]
	return a || b
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
	Manager Manager
	Changes []Change
	Notes   []string
}

// Empty reports whether the plan changes nothing.
func (p Plan) Empty() bool { return len(p.Changes) == 0 }

// PlanInstall computes the changes that wire the git hooks through det's manager, preserving user content.
func PlanInstall(root string, det Detection) (Plan, error) {
	switch det.Manager {
	case Husky:
		return planHusky(root, true)
	case Lefthook:
		return planLefthook(root, det.ConfigPath, true)
	case PreCommit:
		return planPreCommit(root, true)
	default:
		return planShim(root, true)
	}
}

// PlanUninstall computes the reverse of PlanInstall: only terma's own lines and files go.
func PlanUninstall(root string, det Detection) (Plan, error) {
	switch det.Manager {
	case Husky:
		return planHusky(root, false)
	case Lefthook:
		return planLefthook(root, det.ConfigPath, false)
	case PreCommit:
		return planPreCommit(root, false)
	default:
		return planShim(root, false)
	}
}

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
