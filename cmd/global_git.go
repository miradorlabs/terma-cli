package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
)

// Global mode's commit stamping. A commit is stamped by prepare-commit-msg and recorded
// by post-commit; per repository that is a committed hook line or the shims `terma
// install` points one clone at. In global mode every repository counts, so `terma
// setup` points git itself at terma's hooks: `git config --global core.hooksPath` names
// a directory terma writes (config dir, git-hooks/).
//
// A global core.hooksPath replaces each repository's own .git/hooks for every hook name,
// so that directory holds a script for each one: the two terma uses call terma first,
// and every one then runs the repository's own hook of that name — or, when the
// developer had a global hooks directory of their own, that one's, which is what git ran
// before. A repository that sets core.hooksPath itself (husky, lefthook, `terma
// install`'s shims) is unaffected: its local setting outranks the global one, and those
// repositories' committed lines are what stamp them.

const (
	globalGitHooksDir = "git-hooks"
	// globalGitPrevious records the global core.hooksPath terma replaced ("" for none),
	// restored when global mode ends.
	globalGitPrevious = ".previous-hooks-path"
)

// gitHookNames are the client-side hooks git runs; each gets a script, so none of a
// repository's own stops running under the global hooks path.
var gitHookNames = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit", "pre-merge-commit",
	"prepare-commit-msg", "commit-msg", "post-commit", "pre-rebase", "post-checkout",
	"post-merge", "pre-push", "pre-auto-gc", "post-rewrite", "sendemail-validate",
	"fsmonitor-watchman", "reference-transaction", "post-index-change", "push-to-checkout",
}

// termaGitHooks are the hooks terma itself runs.
var termaGitHooks = map[string]bool{"prepare-commit-msg": true, "post-commit": true}

func globalGitHooksPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, globalGitHooksDir), nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// globalGitHookScript is one hook's script: terma (for the two it uses; a terma that is
// gone, or fails, never blocks git), then the hook git would have run without terma.
func globalGitHookScript(hook, terma, previous string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Written by `terma setup` for global mode (git config --global core.hooksPath).\n")
	b.WriteString("# Removed when the organization leaves global mode. Never blocks git.\n")
	if termaGitHooks[hook] {
		fmt.Fprintf(&b, "[ -x %[1]s ] && %[1]s hook %[2]s \"$@\" || true\n", shellQuote(terma), hook)
	}
	if previous != "" {
		// The developer's own global hooks directory, which git ran before terma's.
		fmt.Fprintf(&b, "chained=%s/%s\n", shellQuote(previous), hook)
	} else {
		// The repository's own hooks directory (git runs hooks from the worktree root;
		// a linked worktree asks git for the common directory).
		fmt.Fprintf(&b, "if [ -d \"${GIT_DIR:-.git}/hooks\" ]; then chained=\"${GIT_DIR:-.git}/hooks/%[1]s\"; else chained=\"$(git rev-parse --git-common-dir 2>/dev/null)/hooks/%[1]s\"; fi\n", hook)
	}
	b.WriteString("if [ -x \"$chained\" ] && ! [ \"$chained\" -ef \"$0\" ]; then exec \"$chained\" \"$@\"; fi\nexit 0\n")
	return b.String()
}

// applyGlobalGitHooks points git's global core.hooksPath at terma's hooks (install),
// remembering what it replaces, or puts back what was there (not install). It reports
// whether it changed git's configuration.
func (app *App) applyGlobalGitHooks(ctx context.Context, install bool) (bool, error) {
	dir, err := globalGitHooksPath()
	if err != nil {
		return false, err
	}
	current, _ := gitx.Git(ctx, "", "config", "--global", "--get", "core.hooksPath")
	current = strings.TrimSpace(current)
	ours := current != "" && sameDir(current, dir)
	if !install {
		if !ours {
			return false, os.RemoveAll(dir)
		}
		previous, _ := os.ReadFile(filepath.Join(dir, globalGitPrevious))
		if p := strings.TrimSpace(string(previous)); p != "" {
			_, err = gitx.Git(ctx, "", "config", "--global", "core.hooksPath", p)
		} else {
			_, err = gitx.Git(ctx, "", "config", "--global", "--unset", "core.hooksPath")
		}
		if err != nil {
			return false, err
		}
		return true, os.RemoveAll(dir)
	}
	terma, err := app.hookExecutable()
	if err != nil {
		return false, err
	}
	previous := ""
	if ours {
		data, _ := os.ReadFile(filepath.Join(dir, globalGitPrevious))
		previous = strings.TrimSpace(string(data))
	} else if current != "" {
		previous = expandHome(current)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	if err := config.WriteFileAtomic(filepath.Join(dir, globalGitPrevious), []byte(previous+"\n"), 0o600); err != nil {
		return false, err
	}
	for _, h := range gitHookNames {
		if err := config.WriteFileAtomic(filepath.Join(dir, h), []byte(globalGitHookScript(h, terma, previous)), 0o755); err != nil {
			return false, err
		}
	}
	if ours {
		return false, nil
	}
	if _, err := gitx.Git(ctx, "", "config", "--global", "core.hooksPath", dir); err != nil {
		return false, err
	}
	return true, nil
}

// sameDir reports whether a and b name the same directory.
func sameDir(a, b string) bool {
	a, b = expandHome(a), expandHome(b)
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// expandHome resolves a leading ~/ the way git reads core.hooksPath.
func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}
