package project

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/gitx"
)

// Locate returns Git's worktree root, else outside Git dir itself.
func Locate(ctx context.Context, dir string) (root, gitDir string, err error) {
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", err
	}
	root, gitDir, err = gitx.Locate(ctx, dir)
	if err == nil {
		return root, gitDir, nil
	}
	if !errors.Is(err, gitx.ErrNotRepo) {
		if !errors.Is(err, exec.ErrNotFound) {
			return "", "", err
		}
		if _, _, found := gitx.LocateFS(dir); found {
			return "", "", err
		}
	}
	// A broken .git marker is not an ordinary non-Git folder.
	for cur := dir; ; cur = filepath.Dir(cur) {
		if _, statErr := os.Lstat(filepath.Join(cur, ".git")); statErr == nil {
			return "", "", fmt.Errorf("cannot locate Git worktree at %s: %w", cur, err)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", "", statErr
		}
		if filepath.Dir(cur) == cur {
			break
		}
	}
	return dir, "", nil
}

// WorkspacesDir is where, under terma's state directory, StoreDir keeps private session stores.
const WorkspacesDir = "workspaces"

// GitStoreDir is the session store's folder inside a repository's git directory.
const GitStoreDir = "terma"

// StoreDir is the session store for the workspace at root: terma's folder in the Git
// directory, or a private store under terma's state directory termaState that a workspace
// begun outside Git keeps after git init so hooks never split across stores.
func StoreDir(termaState, root, gitDir string) (string, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	private := filepath.Join(termaState, WorkspacesDir, fmt.Sprintf("%x", sha256.Sum256([]byte(root))))
	if gitDir == "" {
		return private, nil
	}
	info, err := os.Stat(private)
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Join(gitDir, GitStoreDir), nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace session state is not a directory: %s", private)
	}
	return private, nil
}

// CheckEscape refuses a relative path that leaves the directory it is relative to.
func CheckEscape(relative string) error {
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(filepath.Clean(relative), ".."+string(filepath.Separator)) {
		return fmt.Errorf("path leaves its directory: %s", relative)
	}
	return nil
}

// CheckPath refuses a path that escapes root or passes through a symlink.
func CheckPath(root, relative string) error {
	if err := CheckEscape(relative); err != nil {
		return err
	}
	path := root
	for _, part := range strings.Split(filepath.Clean(relative), string(filepath.Separator)) {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to modify symlinked configuration: %s", path)
		}
	}
	return nil
}
