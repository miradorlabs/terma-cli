package hookmgr

// The four ways a repository runs git hooks: terma's own shims behind core.hooksPath,
// and husky, lefthook and pre-commit, each edited in the form its users keep it in.

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// ShimScript is the committed fallback hook: guard, run terma without ever failing the commit, then chain.
func ShimScript(hook string) string {
	return shimScript(hook, true)
}

func shimScript(hook string, remember bool) string {
	script := fmt.Sprintf(`#!/bin/sh
# Installed by "terma install". Thin shim: all logic lives in the terma binary,
# so this file never needs to change when terma updates. It must never block a
# commit: a missing or failing terma is ignored.
if command -v terma >/dev/null 2>&1; then
  terma hook %[1]s "$@" || true
fi
# Chain a pre-existing hook of the same name: an explicit directory first, else
# the repository's own hooks dir (never core.hooksPath, which points back here).
# git runs hooks from the worktree root, so .git/hooks is right without asking
# git (a subprocess this shim cannot afford); linked worktrees ask.
chain_dir="${TERMA_CHAIN_HOOKS_DIR:-}"
if [ -z "$chain_dir" ]; then
  if [ -d "${GIT_DIR:-.git}/hooks" ]; then
    chain_dir="${GIT_DIR:-.git}/hooks"
  else
    chain_dir="$(git rev-parse --git-common-dir 2>/dev/null)/hooks"
  fi
fi
chained="$chain_dir/%[1]s"
if [ -x "$chained" ] && ! [ "$chained" -ef "$0" ]; then
  exec "$chained" "$@"
fi
exit 0
`, hook)
	if !remember {
		return script
	}
	return strings.Replace(script, `chain_dir="${TERMA_CHAIN_HOOKS_DIR:-}"`, `chain_dir="${TERMA_CHAIN_HOOKS_DIR:-}"
if [ -z "$chain_dir" ]; then
  state_git_dir="${GIT_DIR:-.git}"
  if [ ! -d "$state_git_dir" ]; then
    state_git_dir="$(git rev-parse --absolute-git-dir 2>/dev/null)"
  fi
  if [ -f "$state_git_dir/terma/previous-hooks-path" ]; then
    IFS= read -r chain_dir < "$state_git_dir/terma/previous-hooks-path" || true
  fi
fi`, 1)
}

func planShim(root string, install bool) (Plan, error) {
	p := Plan{Manager: GitShim}
	for _, hook := range GitHooks {
		rel := ShimDir + "/" + hook
		before, err := ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return p, err
		}
		want := []byte(ShimScript(hook))
		if before != nil && !bytes.Equal(before, want) && !bytes.Equal(before, []byte(shimScript(hook, false))) {
			if install {
				return p, fmt.Errorf("%s contains an unrecognized or modified hook; preserve or move it before retrying", rel)
			}
			p.Notes = append(p.Notes, "Left modified or unrecognized hook: "+rel)
			continue
		}
		if install {
			if !bytes.Equal(before, want) {
				p.Changes = append(p.Changes, Change{Path: rel, Before: before, After: want, Mode: 0o755})
			}
		} else if before != nil {
			p.Changes = append(p.Changes, Change{Path: rel, Before: before})
		}
	}
	if install {
		p.Notes = append(p.Notes,
			"Each clone must point git at the shims once: `terma install` runs `git config core.hooksPath "+ShimDir+"` in this repository (uninstall reverts it).")
	}
	return p, nil
}

func splitLines(data []byte) []string {
	if data == nil {
		return nil
	}
	s := strings.TrimRight(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func containsMarker(lines []string) bool {
	return slices.ContainsFunc(lines, ownedHuskyLine)
}

func removeMarked(lines []string) []string {
	var out []string
	for _, l := range lines {
		if !ownedHuskyLine(l) {
			out = append(out, l)
		}
	}
	return out
}

// replaceMarked rewrites every terma line as line in place, reporting whether anything differed.
func replaceMarked(lines []string, line string) ([]string, bool) {
	out := make([]string, 0, len(lines))
	changed := false
	for _, l := range lines {
		if ownedHuskyLine(l) && l != line {
			l = line
			changed = true
		}
		out = append(out, l)
	}
	return out, changed
}
func appendLine(data []byte, line string) []byte {
	out := bytes.TrimRight(data, "\n")
	if len(out) > 0 {
		out = append(out, '\n')
	}
	return append(append(out, line...), '\n')
}
