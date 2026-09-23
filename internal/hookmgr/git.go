package hookmgr

// The four ways a repository runs git hooks: terma's own shims behind core.hooksPath,
// and husky, lefthook and pre-commit, each edited in the form its users keep it in.

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// --- shim scripts -------------------------------------------------------------

// ShimScript is the committed fallback hook. It is also the shape every manager's
// entry follows: guard, run terma without ever failing the commit, then chain.
func ShimScript(hook string) string {
	return fmt.Sprintf(`#!/bin/sh
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
}

func planShim(root string, install bool) (Plan, error) {
	p := Plan{Manager: GitShim}
	for _, hook := range GitHooks {
		rel := ShimDir + "/" + hook
		before, err := readFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return p, err
		}
		if install {
			want := []byte(ShimScript(hook))
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

// --- husky -----------------------------------------------------------------------

// huskyLine is the one line terma adds to a husky hook file. Husky runs hook files
// with `sh -e` and forwards git's arguments, and a hook file's exit status is its
// last line's. The line therefore ends in `|| true`: an earlier form guarded the
// call with `command -v terma && { ... }` and nothing after it, so on a machine
// without terma the guard's own status (1) became the hook's, and husky failed the
// commit of every colleague who had not installed terma. The whole point of the
// guard is that they never notice it.
func huskyLine(hook string) string {
	return fmt.Sprintf(`command -v terma >/dev/null 2>&1 && terma hook %s "$@" || true # %s`, hook, Marker)
}

func planHusky(root string, install bool) (Plan, error) {
	p := Plan{Manager: Husky}
	for _, hook := range GitHooks {
		rel := ".husky/" + hook
		before, err := readFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return p, err
		}
		line := huskyLine(hook)
		lines := splitLines(before)
		has := containsMarker(lines)
		switch {
		case install && !has:
			var after []byte
			if before == nil {
				after = []byte(line + "\n")
			} else {
				after = appendLine(before, line)
			}
			p.Changes = append(p.Changes, Change{Path: rel, Before: before, After: after, Mode: 0o755})
		case install && has:
			// A line from an older terma is rewritten where it stands, so a repository
			// that installed before a fix picks it up on the next `terma install`.
			if kept, changed := replaceMarked(lines, line); changed {
				p.Changes = append(p.Changes, Change{Path: rel, Before: before, After: []byte(strings.Join(kept, "\n") + "\n"), Mode: 0o755})
			}
		case !install && has:
			kept := removeMarked(lines)
			if len(bytes.TrimSpace([]byte(strings.Join(kept, "\n")))) == 0 {
				p.Changes = append(p.Changes, Change{Path: rel, Before: before})
			} else {
				p.Changes = append(p.Changes, Change{Path: rel, Before: before, After: []byte(strings.Join(kept, "\n") + "\n"), Mode: 0o755})
			}
		}
	}
	if install {
		p.Notes = append(p.Notes, "Husky installs these on `npm install` (its prepare script); existing clones can run `npx husky`.")
	}
	return p, nil
}

// --- lefthook -------------------------------------------------------------------

// lefthookRun is the command lefthook runs for one hook. Lefthook hands it to `sh -c`
// and forwards git's arguments as {1} {2} {3}. The `command -v` guard keeps a machine
// without terma from printing "terma: command not found" on every commit, and the
// trailing `|| true` keeps the guard's own status from failing the hook.
func lefthookRun(hook string) string {
	run := "terma hook " + hook
	if hook == "prepare-commit-msg" {
		run += " {1} {2} {3}"
	}
	return "command -v terma >/dev/null 2>&1 && " + run + " || true"
}

func planLefthook(root, configPath string, install bool) (Plan, error) {
	p := Plan{Manager: Lefthook}
	path := filepath.Join(root, configPath)
	before, err := readFile(path)
	if err != nil {
		return p, err
	}
	var doc yaml.Node
	if before != nil {
		if err := yaml.Unmarshal(before, &doc); err != nil {
			return p, fmt.Errorf("parse %s: %w", configPath, err)
		}
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	rootMap := doc.Content[0]
	if rootMap.Kind != yaml.MappingNode {
		return p, fmt.Errorf("%s: top level is not a mapping", configPath)
	}
	changed := false
	for _, hook := range GitHooks {
		hookNode := mapGet(rootMap, hook)
		if install {
			if hookNode == nil {
				hookNode = &yaml.Node{Kind: yaml.MappingNode}
				mapSet(rootMap, hook, hookNode)
			}
			commands := mapGet(hookNode, "commands")
			if commands == nil {
				commands = &yaml.Node{Kind: yaml.MappingNode}
				mapSet(hookNode, "commands", commands)
			}
			run := lefthookRun(hook)
			if entry := mapGet(commands, "terma"); entry == nil {
				entry = &yaml.Node{Kind: yaml.MappingNode}
				mapSet(entry, "run", scalar(run))
				mapSet(entry, "skip", &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{scalar("merge"), scalar("rebase")}})
				mapSet(commands, "terma", entry)
				changed = true
			} else if cur := mapGet(entry, "run"); cur != nil && cur.Kind == yaml.ScalarNode && cur.Value != run {
				// An entry from an older terma is brought up to date in place.
				cur.Value = run
				changed = true
			}
		} else if hookNode != nil {
			if commands := mapGet(hookNode, "commands"); commands != nil && mapDelete(commands, "terma") {
				changed = true
				if len(commands.Content) == 0 {
					mapDelete(hookNode, "commands")
				}
				if len(hookNode.Content) == 0 {
					mapDelete(rootMap, hook)
				}
			}
		}
	}
	if !changed {
		return p, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return p, err
	}
	_ = enc.Close()
	after := buf.Bytes()
	if !install && len(bytes.TrimSpace(after)) == 0 {
		after = []byte("{}\n")
	}
	p.Changes = append(p.Changes, Change{Path: configPath, Before: before, After: after})
	if install {
		p.Notes = append(p.Notes, "Run `lefthook install` in each clone so lefthook writes the git hooks.")
	}
	return p, nil
}

// --- pre-commit -----------------------------------------------------------------

const preCommitRepo = "local"

// preCommitEntry is the `entry` of terma's local pre-commit hook: pre-commit splits
// it and appends the stage's arguments. The `command -v` guard and the closing
// `exit 0` keep a machine without terma silent and the hook green.
func preCommitEntry(hook string) string {
	return "sh -c 'command -v terma >/dev/null 2>&1 && { terma hook " + hook + " \"$@\" || true; }; exit 0' --"
}

func planPreCommit(root string, install bool) (Plan, error) {
	p := Plan{Manager: PreCommit}
	configPath := ".pre-commit-config.yaml"
	path := filepath.Join(root, configPath)
	before, err := readFile(path)
	if err != nil {
		return p, err
	}
	var doc yaml.Node
	if before != nil {
		if err := yaml.Unmarshal(before, &doc); err != nil {
			return p, fmt.Errorf("parse %s: %w", configPath, err)
		}
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	rootMap := doc.Content[0]
	if rootMap.Kind != yaml.MappingNode {
		return p, fmt.Errorf("%s: top level is not a mapping", configPath)
	}
	repos := mapGet(rootMap, "repos")
	if repos == nil {
		repos = &yaml.Node{Kind: yaml.SequenceNode}
		mapSet(rootMap, "repos", repos)
	}
	// Find (or create) the `repo: local` entry and its hooks list.
	var local, hooks *yaml.Node
	for _, r := range repos.Content {
		if v := mapGet(r, "repo"); v != nil && v.Value == preCommitRepo {
			local = r
			hooks = mapGet(r, "hooks")
			break
		}
	}
	changed := false
	if install {
		if local == nil {
			local = &yaml.Node{Kind: yaml.MappingNode}
			mapSet(local, "repo", scalar(preCommitRepo))
			repos.Content = append(repos.Content, local)
		}
		if hooks == nil {
			hooks = &yaml.Node{Kind: yaml.SequenceNode}
			mapSet(local, "hooks", hooks)
		}
		for _, hook := range GitHooks {
			id := "terma-" + hook
			if seqHasID(hooks, id) {
				continue
			}
			entry := &yaml.Node{Kind: yaml.MappingNode}
			mapSet(entry, "id", scalar(id))
			mapSet(entry, "name", scalar("terma "+hook))
			// pre-commit passes the commit message path (and source) as arguments
			// for these stages; `|| true` keeps a terma failure from failing the hook.
			mapSet(entry, "entry", scalar(preCommitEntry(hook)))
			mapSet(entry, "language", scalar("system"))
			mapSet(entry, "stages", &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{scalar(hook)}})
			mapSet(entry, "always_run", scalar("true"))
			mapSet(entry, "pass_filenames", scalar("false"))
			hooks.Content = append(hooks.Content, entry)
			changed = true
		}
		// The hook types must be installed for these stages to fire.
		types := mapGet(rootMap, "default_install_hook_types")
		if types == nil {
			types = &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{scalar("pre-commit")}}
			mapSet(rootMap, "default_install_hook_types", types)
			changed = true
		}
		for _, hook := range GitHooks {
			if !seqHasValue(types, hook) {
				types.Content = append(types.Content, scalar(hook))
				changed = true
			}
		}
	} else if hooks != nil {
		var kept []*yaml.Node
		for _, h := range hooks.Content {
			if v := mapGet(h, "id"); v != nil && strings.HasPrefix(v.Value, "terma-") {
				changed = true
				continue
			}
			kept = append(kept, h)
		}
		hooks.Content = kept
		if len(kept) == 0 && local != nil {
			var keptRepos []*yaml.Node
			for _, r := range repos.Content {
				if r != local {
					keptRepos = append(keptRepos, r)
				}
			}
			repos.Content = keptRepos
		}
	}
	if !changed {
		return p, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return p, err
	}
	_ = enc.Close()
	p.Changes = append(p.Changes, Change{Path: configPath, Before: before, After: buf.Bytes()})
	if install {
		p.Notes = append(p.Notes, "Run `pre-commit install --hook-type prepare-commit-msg --hook-type post-commit` in each clone.")
	}
	return p, nil
}

// --- line and YAML helpers ------------------------------------------------------

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
	for _, l := range lines {
		if strings.Contains(l, Marker) {
			return true
		}
	}
	return false
}

func removeMarked(lines []string) []string {
	var out []string
	for _, l := range lines {
		if !strings.Contains(l, Marker) {
			out = append(out, l)
		}
	}
	return out
}

// replaceMarked rewrites every terma line as line, reporting whether anything
// differed. The position of the line in the file is the user's and is kept.
func replaceMarked(lines []string, line string) ([]string, bool) {
	out := make([]string, 0, len(lines))
	changed := false
	for _, l := range lines {
		if strings.Contains(l, Marker) && l != line {
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

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: v}
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapSet(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, scalar(key), v)
}

func mapDelete(m *yaml.Node, key string) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}

func seqHasID(seq *yaml.Node, id string) bool {
	for _, item := range seq.Content {
		if v := mapGet(item, "id"); v != nil && v.Value == id {
			return true
		}
	}
	return false
}

func seqHasValue(seq *yaml.Node, value string) bool {
	for _, item := range seq.Content {
		if item.Value == value {
			return true
		}
	}
	return false
}
