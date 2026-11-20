package antigravity

import (
	"encoding/json"
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

// ownHook is terma's named hook: every event, each running command.
func ownHook(command func(event string) string) (json.RawMessage, error) {
	events := map[string][]any{}
	for _, h := range antigravityHooks {
		handler := antigravityHandler{Type: "command", Command: command(h.Hook), Timeout: 10}
		if h.Grouped {
			// An empty matcher matches every tool.
			events[h.Event] = []any{struct {
				Matcher string               `json:"matcher"`
				Hooks   []antigravityHandler `json:"hooks"`
			}{"", []antigravityHandler{handler}}}
			continue
		}
		events[h.Event] = []any{handler}
	}
	return hookmgr.MarshalJSON(events, "  ", "  ")
}

// planUserHooks sets or removes the one member terma owns, leaving every other named hook
// as it was read. A member of that name terma did not write is the developer's: neither
// replaced nor removed.
func planUserHooks(dir string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	const path = "hooks.json"
	p := hookmgr.Plan{}
	before, err := hookmgr.ReadFile(filepath.Join(dir, path))
	if err != nil {
		return p, err
	}
	top := map[string]json.RawMessage{}
	if before != nil {
		if err := json.Unmarshal(before, &top); err != nil {
			return p, fmt.Errorf("parse %s: %w", path, err)
		}
		if top == nil {
			return p, fmt.Errorf("parse %s: expected a JSON object", path)
		}
	}
	current, present := top[antigravityHookName]
	if present && !hookmgr.CallsTerma(current) {
		if install {
			return p, fmt.Errorf("%s already has a hook named %q that terma did not write", filepath.Join(dir, path), antigravityHookName)
		}
		return p, nil
	}
	if install {
		own, err := ownHook(command)
		if err != nil {
			return p, err
		}
		if present && hookmgr.SameJSON(current, own) {
			return p, nil
		}
		top[antigravityHookName] = own
	} else {
		if !present {
			return p, nil
		}
		delete(top, antigravityHookName)
		if len(top) == 0 {
			p.Changes = append(p.Changes, hookmgr.Change{Path: path, Before: before})
			return p, nil
		}
	}
	out, err := hookmgr.MarshalOrdered(top)
	if err != nil {
		return p, err
	}
	p.Changes = append(p.Changes, hookmgr.Change{Path: path, Before: before, After: append(out, '\n')})
	return p, nil
}
