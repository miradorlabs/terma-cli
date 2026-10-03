package globalmode

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
)

// A clone whose own core.hooksPath outranks git's global one (husky sets one) runs none of
// terma's git hooks. terma routes it through a directory of its own under the config
// directory, set at git's worktree scope, which outranks the clone's own setting and
// survives a hook manager writing it again; each script chains to the clone's hooks.

const (
	cloneHooksDir = "clone-hooks"
	// cloneGitDir names the clone's git directory, so teardown can find it again.
	cloneGitDir = ".git-dir"
)

func cloneHooksPath(gitDir string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(gitDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, cloneHooksDir, fmt.Sprintf("%x", sha256.Sum256([]byte(abs)))[:16]), nil
}

// WireClone routes the checkout at root through terma's git hooks when its own
// core.hooksPath would bypass them, chaining to that path; it reports whether it changed
// anything. A clone with no setting of its own, or one another tool set at worktree scope,
// is left alone.
func (m Machine) WireClone(ctx context.Context, root, gitDir string) (bool, error) {
	local, worktree := gitx.HooksPathFS(gitDir)
	dir, err := cloneHooksPath(gitDir)
	if err != nil {
		return false, err
	}
	switch {
	case worktree != "" && sameDir(worktree, dir):
		// The clone's own setting moved since: chain to where it points now.
		if previous, _ := os.ReadFile(filepath.Join(dir, globalGitPrevious)); strings.TrimSpace(string(previous)) == expandHome(local) {
			return false, nil
		}
		return true, m.writeCloneHooks(dir, gitDir, local)
	case worktree != "" || local == "":
		return false, nil
	}
	if err := m.writeCloneHooks(dir, gitDir, local); err != nil {
		return false, err
	}
	if err := enableWorktreeConfig(ctx, root, gitDir); err != nil {
		return false, err
	}
	_, err = gitx.Git(ctx, root, "config", "--worktree", "core.hooksPath", dir)
	return err == nil, err
}

func (m Machine) writeCloneHooks(dir, gitDir, local string) error {
	terma, err := m.Terma()
	if err != nil {
		return err
	}
	previous := expandHome(local)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(gitDir)
	if err != nil {
		return err
	}
	for name, data := range map[string]string{globalGitPrevious: previous + "\n", cloneGitDir: abs + "\n"} {
		if err := config.WriteFileAtomic(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			return err
		}
	}
	for _, h := range gitHookNames {
		if err := config.WriteFileAtomic(filepath.Join(dir, h), []byte(globalGitHookScript(h, terma, previous)), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// enableWorktreeConfig turns on git's per-worktree scope, which first needs these
// main-worktree-only settings out of the shared config.
func enableWorktreeConfig(ctx context.Context, root, gitDir string) error {
	if gitx.ConfigGet(ctx, root, "extensions.worktreeConfig") == "true" {
		return nil
	}
	common := gitx.CommonDirFS(gitDir)
	for _, key := range []string{"core.worktree", "core.bare"} {
		value, err := gitx.Git(ctx, root, "config", "--local", "--get", key)
		if err != nil || (key == "core.bare" && !strings.EqualFold(value, "true")) {
			continue
		}
		if _, err := gitx.Git(ctx, root, "config", "--file", filepath.Join(common, "config.worktree"), key, value); err != nil {
			return err
		}
		if err := gitx.ConfigUnset(ctx, root, key); err != nil {
			return err
		}
	}
	return gitx.ConfigSet(ctx, root, "extensions.worktreeConfig", "true")
}

// unwireClones takes terma's setting out of every clone it routed, leaving each clone's
// own, and removes the directories; a clone gone since is skipped.
func unwireClones(ctx context.Context) error {
	base, err := config.Dir()
	if err != nil {
		return err
	}
	base = filepath.Join(base, cloneHooksDir)
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		dir := filepath.Join(base, e.Name())
		data, _ := os.ReadFile(filepath.Join(dir, cloneGitDir))
		if gitDir := strings.TrimSpace(string(data)); gitDir != "" {
			if _, worktree := gitx.HooksPathFS(gitDir); worktree != "" && sameDir(worktree, dir) {
				if _, err := gitx.Git(ctx, "", "--git-dir="+gitDir, "config", "--worktree", "--unset", "core.hooksPath"); err != nil {
					return err
				}
			}
		}
	}
	return os.RemoveAll(base)
}

// IsCloneHooksDir reports whether path, a core.hooksPath as git reads it, is one terma set
// for a clone.
func IsCloneHooksDir(path string) bool {
	dir, err := config.Dir()
	return err == nil && path != "" && filepath.Dir(filepath.Clean(expandHome(path))) == filepath.Join(dir, cloneHooksDir)
}
