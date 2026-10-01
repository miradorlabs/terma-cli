package hookmgr

import (
	"bytes"
	"fmt"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const preCommitRepo = "local"

// preCommitEntry is guarded and ends in `exit 0`, so a machine without terma stays silent and green.
func preCommitEntry(hook string) string {
	return "sh -c 'command -v terma >/dev/null 2>&1 && { terma hook " + hook + " \"$@\" || true; }; exit 0' --"
}

func planPreCommit(root string, install bool) (Plan, error) {
	p := Plan{Manager: PreCommit}
	configPath := ".pre-commit-config.yaml"
	path := filepath.Join(root, configPath)
	before, err := ReadFile(path)
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
				for _, entry := range hooks.Content {
					if v := mapGet(entry, "id"); v != nil && v.Value == id && !ownedPreCommitEntry(entry) {
						return p, fmt.Errorf("%s: hook id %s belongs to another command", configPath, id)
					}
				}
				continue
			}
			entry := &yaml.Node{Kind: yaml.MappingNode}
			mapSet(entry, "id", scalar(id))
			mapSet(entry, "name", scalar("terma "+hook))
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
			types = &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{termaHookType("pre-commit")}}
			mapSet(rootMap, "default_install_hook_types", types)
			changed = true
		}
		for _, hook := range GitHooks {
			if !seqHasValue(types, hook) {
				types.Content = append(types.Content, termaHookType(hook))
				changed = true
			}
		}
	} else if hooks != nil {
		var kept []*yaml.Node
		for _, h := range hooks.Content {
			if ownedPreCommitEntry(h) {
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
	if !install {
		if types := mapGet(rootMap, "default_install_hook_types"); types != nil {
			var kept []*yaml.Node
			for _, item := range types.Content {
				if item.LineComment == "# added by terma install" && (item.Value == "pre-commit" || item.Value == "prepare-commit-msg" || item.Value == "post-commit") {
					changed = true
					continue
				}
				kept = append(kept, item)
			}
			types.Content = kept
			if len(kept) == 0 {
				mapDelete(rootMap, "default_install_hook_types")
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
	p.Changes = append(p.Changes, Change{Path: configPath, Before: before, After: buf.Bytes()})
	if install {
		p.Notes = append(p.Notes, "Run `pre-commit install --hook-type prepare-commit-msg --hook-type post-commit` in each clone.")
	}
	return p, nil
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

// termaHookType marks only a hook type we add, so uninstall preserves existing types.
func termaHookType(value string) *yaml.Node {
	n := scalar(value)
	n.LineComment = "# added by terma install"
	return n
}

func ownedPreCommitEntry(entry *yaml.Node) bool {
	id, command := mapGet(entry, "id"), mapGet(entry, "entry")
	if id == nil || command == nil {
		return false
	}
	for _, hook := range GitHooks {
		if id.Value == "terma-"+hook && command.Value == preCommitEntry(hook) {
			return true
		}
	}
	return false
}
