// Package project reads and writes .terma/settings.json, the committed, secret-free
// file that binds a repository to a Terma project.
//
// It carries only what every clone needs to agree on: which project telemetry
// belongs to and which environment it lives in. Credentials never go here — they
// stay in each developer's home directory (see `terma install`).
//
// The file lives inside the same .terma/ directory that holds the committed git-hook
// shims (.terma/hooks/), mirroring the .claude/settings.json and .cursor/hooks.json
// layout other agents use. Older repositories carried the binding as a top-level
// .terma.toml; Load still reads that, and the next Save migrates it to the new
// location and removes the legacy file.
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

	"github.com/pelletier/go-toml/v2"
)

const (
	// Dir is the committed directory at the repository root that holds the binding
	// file and the git-hook shims.
	Dir = ".terma"
	// SettingsName is the binding file inside Dir.
	SettingsName = "settings.json"
	// FileName is the binding file relative to the repository root, for display.
	FileName = Dir + "/" + SettingsName
	// LegacyFileName is the pre-2026-09 top-level binding file, read for migration.
	LegacyFileName = ".terma.toml"
)

// File is the on-disk shape. Fields carry both json (the current format) and toml
// (the legacy .terma.toml) tags so the one struct serves reads of either.
type File struct {
	Project Project `json:"project" toml:"project"`
	Install Install `json:"install" toml:"install"`
}

// Project identifies the Terma project this repository reports to.
type Project struct {
	ID   string `json:"id" toml:"id"`
	Name string `json:"name,omitempty" toml:"name,omitempty"`
	// OrganizationID is informational — the project id is globally unique — but
	// lets status/doctor name the org without a round trip.
	OrganizationID string `json:"organization_id,omitempty" toml:"organization_id,omitempty"`
	// Environment is the built-in environment (prod when empty). Committed so a
	// repo pointed at pre-production says so in one place.
	Environment string `json:"environment,omitempty" toml:"environment,omitempty"`
}

// Install records what `terma install` wired, so uninstall is exact and doctor
// knows what to check.
type Install struct {
	HookManager string   `json:"hook_manager,omitempty" toml:"hook_manager,omitempty"`
	Hooks       []string `json:"hooks,omitempty" toml:"hooks,omitempty"`
	// Adapters are the harness adapters wired at repo scope (e.g. "claude") — the ones
	// that write a committed hooks file. This is a team decision (the hooks are
	// committed), so it lives here. Which telemetry harnesses each developer connects,
	// and how they route (wrapper vs PATH shim), is per-developer state that lives in
	// the home directory (the routing record and keystore), never in this committed file.
	Adapters    []string  `json:"adapters,omitempty" toml:"adapters,omitempty"`
	Version     string    `json:"terma_version,omitempty" toml:"terma_version,omitempty"`
	InstalledAt time.Time `json:"installed_at" toml:"installed_at,omitempty"`
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

// LegacyPath is the pre-migration .terma.toml location.
func LegacyPath(root string) string { return filepath.Join(root, LegacyFileName) }

// Load reads the binding at root: the JSON file first, then the legacy TOML file.
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
		return loadLegacy(root)
	default:
		return nil, err
	}
}

// loadLegacy reads a pre-migration .terma.toml.
func loadLegacy(root string) (*File, error) {
	data, err := os.ReadFile(LegacyPath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", LegacyFileName, err)
	}
	return validate(&f, LegacyFileName)
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

// Save writes the binding to .terma/settings.json atomically and removes any legacy
// .terma.toml it supersedes, migrating a repository to the new layout in place.
func Save(root string, f *File) error {
	dir := filepath.Join(root, Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(Path(root), append(body, '\n'), 0o644); err != nil {
		return err
	}
	// Migration: a repo that carried the old top-level file now carries the new one.
	if err := os.Remove(LegacyPath(root)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Remove deletes the binding — both the new and legacy locations — and the .terma
// directory when nothing else (the hook shims) is left in it. A missing file is not
// an error.
func Remove(root string) error {
	for _, p := range []string{Path(root), LegacyPath(root)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	// Best effort: os.Remove of a non-empty directory fails, which is the intent —
	// the hook shims under .terma/hooks/ must survive removing the binding.
	_ = os.Remove(filepath.Join(root, Dir))
	return nil
}

// Find walks up from dir to the first directory holding a binding file (new or
// legacy) and returns that directory. Used by hooks invoked from a subdirectory.
func Find(dir string) (string, error) {
	dir = filepath.Clean(dir)
	for {
		if _, err := os.Stat(Path(dir)); err == nil {
			return dir, nil
		}
		if _, err := os.Stat(LegacyPath(dir)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotFound
		}
		dir = parent
	}
}
