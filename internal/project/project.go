// Package project reads and writes .terma/settings.json, the committed, secret-free
// file that binds a repository to a Terma project; nothing per-developer goes there.
package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
)

const (
	// Dir is the committed directory holding the binding file and the git-hook shims.
	Dir = ".terma"
	// SettingsName is the binding file inside Dir.
	SettingsName = "settings.json"
	// FileName is the binding file relative to the repository root, for display.
	FileName = Dir + "/" + SettingsName
)

// File is the on-disk JSON shape.
type File struct {
	Project Project `json:"project"`
	Install Install `json:"install"`
}

// Project identifies the Terma project this repository reports to.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// OrganizationID is informational: it names the org without a round trip.
	OrganizationID string `json:"organization_id,omitempty"`
	// Environment is the built-in environment; empty is production.
	Environment string `json:"environment,omitempty"`
}

// Install records what `terma install` wired; which agents are wired is left to the
// committed hooks files, so a colleague's install never churns this one.
type Install struct {
	HookManager string    `json:"hook_manager,omitempty"`
	Hooks       []string  `json:"hooks,omitempty"`
	Version     string    `json:"terma_version,omitempty"`
	InstalledAt time.Time `json:"installed_at"`
}

// ErrNotFound is returned when the repository has no binding file.
var ErrNotFound = errors.New(FileName + " not found")

// safeID matches session.ValidID's charset: both ids become file names.
var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// maxIDLen bounds an id long before any filesystem does.
const maxIDLen = 128

// ValidID reports whether id is safe as a path component: the file comes from whoever
// wrote the repository, and an id like "../../.zshenv" would place a key-holding script there.
func ValidID(id string) bool {
	return id != "" && len(id) <= maxIDLen && safeID.MatchString(id) && !strings.HasPrefix(id, ".")
}

// Path is the binding file's location for a repository root.
func Path(root string) string { return filepath.Join(root, Dir, SettingsName) }

// Load reads the binding at root.
func Load(root string) (*File, error) {
	data, err := os.ReadFile(Path(root))
	switch {
	case err == nil:
		var f File
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("parse %s: %w", FileName, err)
		}
		return validate(&f, FileName)
	case errors.Is(err, fs.ErrNotExist):
		return nil, ErrNotFound
	default:
		return nil, err
	}
}

// Resolve reads the binding for the checkout at root (gitDir "" to find it), else, in a
// linked worktree, its main checkout's, since worktrees do not copy a gitignored binding.
func Resolve(root, gitDir string) (f *File, from string, err error) {
	f, err = Load(root)
	if !errors.Is(err, ErrNotFound) {
		return f, root, err
	}
	if gitDir == "" {
		_, gitDir, _ = gitx.LocateFS(root)
	}
	_, main, ok := gitx.LinkedWorktreeFS(gitDir)
	if gitDir == "" || !ok || main == "" {
		return nil, root, err
	}
	mf, merr := Load(main)
	if errors.Is(merr, ErrNotFound) {
		return nil, root, err
	}
	return mf, main, merr
}

// ResolveDir finds the binding for dir through Find, else Resolve; root is the checkout
// it applies to, a worktree's own root included.
func ResolveDir(dir string) (f *File, root string, err error) {
	if root, err := Find(dir); err == nil {
		f, err := Load(root)
		return f, root, err
	}
	root, gitDir, ok := gitx.LocateFS(dir)
	if !ok {
		return nil, "", ErrNotFound
	}
	f, _, err = Resolve(root, gitDir)
	return f, root, err
}

func validate(f *File, source string) (*File, error) {
	f.Project.ID = strings.TrimSpace(f.Project.ID)
	if f.Project.ID == "" {
		return nil, fmt.Errorf("%s has no team id", source)
	}
	if !ValidID(f.Project.ID) {
		return nil, fmt.Errorf("%s has an invalid team id %q: expected letters, digits, dot, dash or underscore", source, f.Project.ID)
	}
	return f, nil
}

// Save writes the binding to .terma/settings.json atomically.
func Save(root string, f *File) error {
	dir := filepath.Join(root, Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(Path(root), append(body, '\n'), 0o644)
}

// Remove deletes the binding, and the .terma directory when nothing else is in it.
func Remove(root string) error {
	if err := os.Remove(Path(root)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Fails, as intended, while hook shims remain under .terma/hooks/.
	_ = os.Remove(filepath.Join(root, Dir))
	return nil
}

// Find walks up from dir to the first directory holding a binding file.
func Find(dir string) (string, error) {
	dir = filepath.Clean(dir)
	for {
		if _, err := os.Stat(Path(dir)); err == nil {
			return dir, nil
		}
		// Do not inherit the parent workspace's binding across a nested checkout.
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return "", ErrNotFound
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotFound
		}
		dir = parent
	}
}
