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

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
)

// Locate uses Git's worktree root when available. Outside Git it uses the nearest
// binding, or dir for a first install. Git errors other than absence remain errors.
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
	if root, err = Find(dir); err == nil {
		return root, "", nil
	} else if !errors.Is(err, ErrNotFound) {
		return "", "", err
	}
	return dir, "", nil
}

// StateDir addresses local session storage. A workspace that started outside Git
// keeps its private store after git init, including before the next install. Its
// existing writers and new Git hooks must never split attribution across stores.
// Workspaces installed with Git from the start use their own Git metadata.
func StateDir(root, gitDir string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		if gitDir != "" {
			return gitDir, nil
		}
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	private := filepath.Join(dir, "workspaces", fmt.Sprintf("%x", sha256.Sum256([]byte(root))))
	if gitDir == "" {
		return private, nil
	}
	info, err := os.Stat(private)
	if errors.Is(err, os.ErrNotExist) {
		return gitDir, nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace session state is not a directory: %s", private)
	}
	return private, nil
}

// CheckPath refuses mutations through symlinked configuration files/directories.
// A symlink used to enter the workspace itself is already canonicalized by Locate.
func CheckPath(root, relative string) error {
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(filepath.Clean(relative), ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes workspace: %s", relative)
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
