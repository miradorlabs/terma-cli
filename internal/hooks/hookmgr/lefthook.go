package hookmgr

import (
	"bytes"
	"fmt"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// lefthookRun is guarded and ends in `|| true`, so a machine without terma prints
// nothing and never fails the hook.
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
				// Never overwrite a user's command just because they named it terma.
				if !ownedLefthookRun(cur.Value, hook) {
					return p, fmt.Errorf("%s: %s.commands.terma is not a Terma hook", configPath, hook)
				}
				cur.Value = run
				changed = true
			}
		} else if hookNode != nil {
			commands := mapGet(hookNode, "commands")
			entry := mapGet(commands, "terma")
			run := mapGet(entry, "run")
			if run != nil && ownedLefthookRun(run.Value, hook) && mapDelete(commands, "terma") {
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

func ownedLefthookRun(run, hook string) bool {
	bare := "terma hook " + hook
	if hook == "prepare-commit-msg" {
		bare += " {1} {2} {3}"
	}
	return run == lefthookRun(hook) || run == bare || run == bare+" || true"
}
