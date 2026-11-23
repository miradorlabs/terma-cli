package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// antigravityHookName is the member terma owns in agy's hooks.json. agy names each hook
// at the top level, {"<name>": {"<Event>": [...]}}, and runs every named hook's handlers;
// a top-level "hooks" would load as one hook called "hooks".
const antigravityHookName = "terma"

// antigravityHooks are terma's agy hooks. PreToolUse is not written: terma never decides
// for an agent, and agy reads a reply without a decision as a denial.
var antigravityHooks = []struct {
	Event   string
	Hook    string
	Grouped bool // PostToolUse takes matcher groups; the others a flat list of handlers
}{
	{"PreInvocation", "antigravity-pre-invocation", false},
	{"PostToolUse", "antigravity-post-tool-use", true},
	{"PostInvocation", "antigravity-post-invocation", false},
	{"Stop", "antigravity-stop", false},
}

// UserHooksPath is agy's global customization root's hooks.json, which every agy session
// on the machine loads beside the workspace's .agents/hooks.json.
func (Agent) UserHooksPath() (string, error) {
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "config", "hooks.json"), err
}

// PlanUserHooks writes terma's named hook into <dir>/hooks.json, or takes it out.
func (Agent) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	return planUserHooks(dir, command, install)
}

type antigravityHandler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// ownEntries are terma's entries in its named hook, by event, each running command.
func ownEntries(command func(event string) string) (map[string]json.RawMessage, error) {
	own := map[string]json.RawMessage{}
	for _, h := range antigravityHooks {
		var entry any = antigravityHandler{Type: "command", Command: command(h.Hook), Timeout: 10}
		if h.Grouped {
			// An empty matcher matches every tool.
			entry = struct {
				Matcher string `json:"matcher"`
				Hooks   []any  `json:"hooks"`
			}{"", []any{entry}}
		}
		raw, err := hookmgr.MarshalJSON(entry, "", "")
		if err != nil {
			return nil, err
		}
		own[h.Event] = raw
	}
	return own, nil
}

// planUserHooks puts terma's entries into the one named hook terma owns, or takes them
// out, leaving every other named hook, and any handler of the developer's inside terma's,
// as it was read. Installing also switches the hook back on: agy runs none of a hook set
// "enabled": false. Removal leaves alone a file it cannot read that holds nothing of
// terma's: agy reads comments and trailing commas, and setup for any other agent, or
// teardown, must not fail on them.
func planUserHooks(dir string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	const path = "hooks.json"
	full := filepath.Join(dir, path)
	p := hookmgr.Plan{}
	before, err := hookmgr.ReadFile(full)
	if err != nil {
		return p, err
	}
	top := map[string]json.RawMessage{}
	if before != nil {
		if err := json.Unmarshal(before, &top); err != nil || top == nil {
			if !install && !bytes.Contains(before, []byte(" hook --user antigravity-")) {
				return p, nil
			}
			if err == nil {
				err = errors.New("expected a JSON object")
			}
			return p, fmt.Errorf("parse %s: %w", full, err)
		}
	}
	member := map[string]json.RawMessage{}
	if current, present := top[antigravityHookName]; present {
		if err := json.Unmarshal(current, &member); err != nil || member == nil {
			if !install {
				return p, nil
			}
			return p, fmt.Errorf("parse %s: hook %q is not an object", full, antigravityHookName)
		}
	}
	own, err := ownEntries(command)
	if err != nil {
		return p, err
	}
	changed := false
	if raw, ok := member["enabled"]; install && ok && string(bytes.TrimSpace(raw)) == "false" {
		delete(member, "enabled")
		changed = true
	}
	for _, h := range antigravityHooks {
		var entries, kept []json.RawMessage
		if raw, ok := member[h.Event]; ok {
			if err := json.Unmarshal(raw, &entries); err != nil {
				return p, fmt.Errorf("parse %s: %s.%s: %w", full, antigravityHookName, h.Event, err)
			}
		}
		ours := false
		for _, entry := range entries {
			if install && !ours && hookmgr.SameJSON(entry, own[h.Event]) {
				kept, ours = append(kept, entry), true
				continue
			}
			remaining, removed, err := hookmgr.WithoutTerma(entry)
			if err != nil {
				return p, fmt.Errorf("parse %s: %s.%s: %w", full, antigravityHookName, h.Event, err)
			}
			changed = changed || removed
			if remaining != nil {
				kept = append(kept, remaining)
			}
		}
		if install && !ours {
			kept, changed = append(kept, own[h.Event]), true
		}
		if len(kept) == 0 {
			delete(member, h.Event)
			continue
		}
		raw, err := hookmgr.MarshalJSON(kept, "", "")
		if err != nil {
			return p, err
		}
		member[h.Event] = raw
	}
	if !changed {
		return p, nil
	}
	if hasEvents(member) {
		raw, err := hookmgr.MarshalJSON(member, "  ", "  ")
		if err != nil {
			return p, err
		}
		top[antigravityHookName] = raw
	} else {
		delete(top, antigravityHookName)
	}
	if !install && len(top) == 0 {
		p.Changes = append(p.Changes, hookmgr.Change{Path: path, Before: before})
		return p, nil
	}
	out, err := hookmgr.MarshalOrdered(top)
	if err != nil {
		return p, err
	}
	p.Changes = append(p.Changes, hookmgr.Change{Path: path, Before: before, After: append(out, '\n')})
	return p, nil
}

// hasEvents reports whether a named hook still lists handlers for an event: its other
// members, such as "enabled", hold none.
func hasEvents(member map[string]json.RawMessage) bool {
	for _, raw := range member {
		if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] == '[' {
			return true
		}
	}
	return false
}
