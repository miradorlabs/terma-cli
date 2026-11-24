package gitx

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Which checkout holds a path, read from the filesystem without running git. It is two steps,
// so a caller that places many paths can remember the second, the walk, per folder:
// ResolveFolderFS finds the real folder a path lies in, and PlaceFolderFS the checkout that
// holds that folder.

// Place is where PlaceFolderFS puts a folder.
type Place int

const (
	// Unplaced is a folder whose checkout cannot be told: GIT_DIR is set, or the layout is
	// unusual enough to ask git, or the filesystem could not be read.
	Unplaced Place = iota
	// NoCheckout is a folder in no git checkout.
	NoCheckout
	// InCheckout is a folder in the checkout PlaceFolderFS names.
	InCheckout
)

// ResolveFolderFS is the folder an absolute path lies in, its symlinks resolved: the path
// itself when it is a folder, else the folder holding it, and for a path not there yet the
// nearest existing folder above it. It is false for a relative path, for a missing path that
// climbs with "..", since past a link ".." names another folder than the text does, and for a
// path through a dangling link, the path itself or a folder on the way, since where it leads
// is unknown.
func ResolveFolderFS(path string) (string, bool) {
	if !filepath.IsAbs(path) {
		return "", false
	}
	resolved, ok := resolveFS(path)
	if !ok {
		return "", false
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", false
	}
	if !info.IsDir() {
		resolved = filepath.Dir(resolved)
	}
	return resolved, true
}

// resolveFS is path with its symlinks resolved, or for a path that does not exist yet, its
// nearest existing folder's, under ResolveFolderFS's rules.
func resolveFS(path string) (string, bool) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, true
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if slices.Contains(strings.Split(filepath.ToSlash(path), "/"), "..") {
		return "", false
	}
	for cur := filepath.Clean(path); ; cur = filepath.Dir(cur) {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return resolved, true
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(cur) == cur {
			return "", false
		}
		// Something there that does not resolve is a link to nothing (or cannot be read).
		if _, err := os.Lstat(cur); !errors.Is(err, os.ErrNotExist) {
			return "", false
		}
	}
}

// PlaceFolderFS is the checkout that holds dir, a folder ResolveFolderFS returned: the
// nearest one above it, so a nested clone or a submodule is its own and a linked worktree is
// its own checkout, with its root and git directory. With GIT_DIR in the environment LocateFS
// pairs any folder with that git directory, so no folder's checkout can be told: every one is
// Unplaced.
func PlaceFolderFS(dir string) (root, gitDir string, place Place) {
	if os.Getenv("GIT_DIR") != "" || !filepath.IsAbs(dir) {
		return "", "", Unplaced
	}
	for cur := filepath.Clean(dir); ; cur = filepath.Dir(cur) {
		_, err := os.Lstat(filepath.Join(cur, ".git"))
		if err == nil {
			root, gitDir, found := LocateFS(cur)
			if !found || root != cur {
				return "", "", Unplaced
			}
			return root, gitDir, InCheckout
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", "", Unplaced
		}
		if filepath.Dir(cur) == cur {
			return "", "", NoCheckout
		}
	}
}
