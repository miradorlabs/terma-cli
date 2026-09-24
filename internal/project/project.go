// Package project reads and writes .terma/settings.json, the committed, secret-free
// file that binds a repository to a Terma project.
//
// It carries only what every clone needs to agree on: which project telemetry
// belongs to and which environment it lives in. Credentials never go here — they
// stay in each developer's home directory (see `terma install`).
//
// The file lives inside the same .terma/ directory that holds the committed git-hook
// shims (.terma/hooks/), mirroring the .claude/settings.json and .cursor/hooks.json
// layout other agents use.
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
	// Dir is the committed directory at the repository root that holds the binding
	// file and the git-hook shims.
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
	// OrganizationID is informational — the project id is globally unique — but
	// lets status/doctor name the org without a round trip.
	OrganizationID string `json:"organization_id,omitempty"`
	// Environment is the built-in environment (prod when empty). Committed so a
	// repo pointed at pre-production says so in one place.
	Environment string `json:"environment,omitempty"`
}

// Install records what `terma install` wired, so uninstall is exact and doctor
// knows what to check.
//
// Which agents' hooks are wired is not recorded: the committed hooks files say it
// themselves (adapter.WiredNames), and which agents a developer uses is home-directory
// state (config.Profile.Harnesses). A list here once copied the second into the first,
// so each colleague's own agents churned a file the whole team shares. A binding that
// still carries "adapters" loads, and loses it on the next Save.
type Install struct {
	HookManager string    `json:"hook_manager,omitempty"`
	Hooks       []string  `json:"hooks,omitempty"`
	Version     string    `json:"terma_version,omitempty"`
	InstalledAt time.Time `json:"installed_at"`
}

// ErrNotFound is returned when the repository has no binding file.
var ErrNotFound = errors.New(FileName + " not found")

// safeID is what a project id is allowed to look like. Real ids are UUIDs; the
// charset is widened to the slug shapes the backend has also issued, and matches
// session.ValidID so the two identifiers that become file names obey one rule.
var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// maxIDLen bounds an id long before any filesystem does.
const maxIDLen = 128

// ValidID reports whether id is safe to use as a path component.
//
// This file is committed, so it arrives from whoever wrote the repository — cloning
// an untrusted one and running `terma install` is the ordinary workflow, not an edge
// case. The id becomes a path component in the helpers directory (see
// harness.HelperFilePath), and filepath.Join collapses "..", so an unchecked id like
// "../../../../.zshenv" would place a 0700 script holding a live server key at a
// path the repository chose. A leading dot is refused as well: a hidden file is not
// a project id, and the pattern would otherwise admit "." and "..".
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

// Resolve reads the binding for the checkout at root, whose git directory is gitDir ("" to
// find it). The checkout's own binding wins. A linked git worktree without one uses its
// main checkout's: a new worktree does not get a gitignored binding, and without this
// every event from it carried no project and was dropped at the next flush. It follows
// git's own link between the two, never directory nesting, so a separate repository
// inside a bound one still does not inherit it. from is the root the binding was read
// from; a missing binding is ErrNotFound, as from Load.
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

// ResolveDir finds the binding for dir: the nearest one above it (Find), else, when dir
// is in a linked worktree with none of its own, the main checkout's (Resolve). root is
// the checkout it applies to — Find's directory, or the worktree's own root, which is
// where that worktree's agents run and are trusted.
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
		return nil, fmt.Errorf("%s has no project id", source)
	}
	if !ValidID(f.Project.ID) {
		return nil, fmt.Errorf("%s has an invalid project id %q: expected letters, digits, dot, dash or underscore", source, f.Project.ID)
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

// Remove deletes the binding and the .terma
// directory when nothing else (the hook shims) is left in it. A missing file is not
// an error.
func Remove(root string) error {
	if err := os.Remove(Path(root)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Best effort: os.Remove of a non-empty directory fails, which is the intent —
	// the hook shims under .terma/hooks/ must survive removing the binding.
	_ = os.Remove(filepath.Join(root, Dir))
	return nil
}

// Find walks up from dir to the first directory holding a binding file and returns
// that directory. Used by hooks invoked from a subdirectory.
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
