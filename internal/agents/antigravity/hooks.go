package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/tailscale/hujson"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
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

// PlanUserHooks writes terma's named hook into <dir>/hooks.json, or takes it out; it
// writes none for an agy too old to run them.
func (Agent) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	if why, old := tooOld(context.Background()); install && old {
		return hookmgr.Plan{}, errors.New(why)
	}
	return planUserHooks(dir, command, install)
}

// RefreshMachine points the hooks terma wrote into agy's file at this terma, as `terma
// update --refresh` does after the binary moved; a file without them is left alone. Not on
// Windows, where terma does not offer Antigravity and a shared home holds another
// system's hooks.
func (a Agent) RefreshMachine() (string, bool, error) {
	if runtime.GOOS == "windows" {
		return "", false, nil
	}
	path, err := a.UserHooksPath()
	if err != nil {
		return "", false, err
	}
	data, err := hookmgr.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte(" hook --user antigravity-")) {
		return path, false, err
	}
	terma, err := procinfo.AbsExecutable()
	if err != nil {
		return path, false, err
	}
	return refreshHooks(path, terma)
}

// refreshHooks rewrites terma's entries in the hooks file at path for terma there.
func refreshHooks(path, terma string) (string, bool, error) {
	dir := filepath.Dir(path)
	plan, err := planUserHooks(dir, hookmgr.UserHookCommand(terma), true)
	if err != nil || plan.Empty() {
		return path, false, err
	}
	return path, true, hookmgr.ApplyThroughLinks(dir, plan)
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
// as it was read. agy reads its file as JSONC: one with comments or trailing commas is
// patched in place, its comments kept. Installing also switches terma's hook back on, as
// a note says: agy runs none of a hook set "enabled": false. Removal never fails on what
// it did not write: a file it cannot parse that holds nothing of terma's, or a part of
// terma's hook that is not the shape terma writes, is left as it is.
func planUserHooks(dir string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	const path = "hooks.json"
	full := filepath.Join(dir, path)
	p := hookmgr.Plan{}
	before, err := hookmgr.ReadFile(full)
	if err != nil {
		return p, err
	}
	doc, top, err := parseHooks(before)
	if err != nil {
		if !install && !bytes.Contains(before, []byte(" hook --user antigravity-")) {
			return p, nil
		}
		return p, fmt.Errorf("parse %s: %w", full, err)
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
	changed, switchedOn, theirs := false, false, false
	if raw, ok := member["enabled"]; install && ok && string(bytes.TrimSpace(raw)) == "false" {
		delete(member, "enabled")
		changed, switchedOn = true, true
	}
	for _, h := range antigravityHooks {
		var entries, kept []json.RawMessage
		if raw, ok := member[h.Event]; ok {
			if err := json.Unmarshal(raw, &entries); err != nil {
				if !install {
					continue
				}
				return p, fmt.Errorf("parse %s: %s.%s is not a list: %w", full, antigravityHookName, h.Event, err)
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
				if !install {
					kept, theirs = append(kept, entry), true
					continue
				}
				return p, fmt.Errorf("parse %s: %s.%s: %w", full, antigravityHookName, h.Event, err)
			}
			changed = changed || removed
			if remaining != nil {
				kept, theirs = append(kept, remaining), true
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
	if switchedOn {
		note := "switched terma's hook in " + full + ` back on (it was "enabled": false)`
		if theirs {
			note += ", and with it the handlers you added to that hook"
		}
		p.Notes = append(p.Notes, note)
	}
	var value json.RawMessage
	if hasEvents(member) {
		if value, err = hookmgr.MarshalJSON(member, "  ", "  "); err != nil {
			return p, err
		}
		top[antigravityHookName] = value
	} else {
		delete(top, antigravityHookName)
	}
	if !install && len(top) == 0 {
		p.Changes = append(p.Changes, hookmgr.Change{Path: path, Before: before})
		return p, nil
	}
	var after []byte
	if doc == nil {
		if after, err = hookmgr.MarshalOrdered(top); err != nil {
			return p, err
		}
	} else if after, err = patchHooks(doc, value); err != nil {
		return p, fmt.Errorf("patch %s: %w", full, err)
	}
	p.Changes = append(p.Changes, hookmgr.Change{Path: path, Before: before, After: append(bytes.TrimRight(after, "\n"), '\n')})
	return p, nil
}

// parseHooks reads agy's hooks file, its named hooks by name. doc is the file as JSONC
// when it holds comments or trailing commas, for patchHooks to keep them; nil for plain
// JSON, which is rewritten whole.
func parseHooks(data []byte) (*hujson.Value, map[string]json.RawMessage, error) {
	top := map[string]json.RawMessage{}
	if data == nil {
		return nil, top, nil
	}
	v, err := hujson.Parse(data)
	if err != nil {
		return nil, nil, err
	}
	std := v.Clone()
	std.Standardize()
	if err := json.Unmarshal(std.Pack(), &top); err != nil || top == nil {
		return nil, nil, errors.New("expected a JSON object")
	}
	if v.IsStandard() {
		return nil, top, nil
	}
	return &v, top, nil
}

// patchHooks sets terma's named hook in a JSONC file to value, or removes it when value is
// nil, leaving the developer's comments and the rest of the file as they were.
func patchHooks(doc *hujson.Value, value json.RawMessage) ([]byte, error) {
	pointer := "/" + antigravityHookName
	op := map[string]any{"op": "add", "path": pointer, "value": value}
	if value == nil {
		op = map[string]any{"op": "remove", "path": pointer}
	}
	patch, err := json.Marshal([]any{op})
	if err != nil {
		return nil, err
	}
	if err := doc.Patch(patch); err != nil {
		return nil, err
	}
	doc.Format()
	return doc.Pack(), nil
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
