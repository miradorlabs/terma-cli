// Package hookmgr installs the git hooks that stamp commits, through whichever hook
// manager the repository already uses.
//
// Hooks are thin shims: each one shells out to `terma hook <event>` and exits.
// All logic lives in the binary, so the committed wiring never changes when terma
// updates, and every shim carries the two guardrails — `|| true` so a terma failure
// never blocks a commit, and a `command -v` guard so a clone without terma on PATH
// commits normally.
//
// The install is repo-scoped and meant to land as one PR: a plan of file changes
// is produced first (so it can be shown, or dry-run), then applied atomically.
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

// The hook managers terma installs through. A repository that already uses one keeps
// using it: terma adds its two hooks in that manager's own file, in the form its users
// keep it in, rather than taking over core.hooksPath from it.
const (
	Husky     Manager = "husky"
	Lefthook  Manager = "lefthook"
	PreCommit Manager = "pre-commit"
	// GitShim is the fallback: committed shim scripts under .terma/hooks that git
	// runs via core.hooksPath, chaining any pre-existing hook of the same name.
	GitShim Manager = "git"
)

// GitHooks are the git hooks terma installs. prepare-commit-msg stamps trailers;
// post-commit records the resulting sha and clears consumed manifests.
var GitHooks = []string{"prepare-commit-msg", "post-commit"}

// ShimDir is where the fallback shims live, relative to the repo root.
const ShimDir = ".terma/hooks"

// Marker identifies lines and entries terma wrote, so install is idempotent and
// uninstall removes only its own.
const Marker = "terma hook"

// HookCommand is the command every harness hook entry runs: the binary, behind the
// guard the git hook lines carry. A committed hooks file runs on every colleague's
// machine, and a colleague without terma must not be able to tell. Claude Code runs the
// command with `sh -c`, prints a hook's stderr in the transcript when it exits non-zero
// and, for SessionStart and PostToolUse, hands that stderr to the model as context — so
// an unguarded `terma hook` meant "command not found" after every edit, read by the
// model. Cursor and Codex run the command the same way (a shell string, JSON on stdin)
// and fail open. The guard writes nothing to either stream: SessionStart's stdout
// becomes context too.
//
// Changing this string changes every committed hooks file on its next `terma install`
// (terma rewrites its own entries in place) and, for Codex, the hash each developer
// trusted, so they trust the hooks once more; `terma doctor` says so.
func HookCommand(event string) string {
	return "command -v terma >/dev/null 2>&1 && terma hook " + event + " || true"
}

// Detection is what Detect found.
type Detection struct {
	Manager    Manager
	ConfigPath string // the file the plan will edit (relative to root), if any
	Detail     string
}

// Detect picks the hook manager from what is already in the repository.
// Preference order matters only when several are present, which is rare; the one
// with a config file committed wins over one merely listed in package.json.
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

// PlanInstall computes the changes that wire the git hooks through det's manager.
// Existing user content is preserved: terma's lines are added, never substituted.
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

// PlanUninstall computes the reverse of PlanInstall: only terma's own lines and
// files go.
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

// removeEmptyParents deletes dir and its ancestors while they are empty, stopping
// at root: an uninstall leaves no empty `.terma/hooks/` shells behind.
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

// readFile returns a file's bytes, or nil when it does not exist. Every other failure
// is returned: a file that is there and cannot be read is not an absent one, and
// planning it as a create would let Apply rename terma-only content over it.
func readFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}
