package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

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
// writes none, into any home, while the agy on PATH is too old to run them.
func (Agent) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	if why, old := tooOld(context.Background()); install && old {
		return hookmgr.Plan{}, errors.New(why)
	}
	return planUserHooks(dir, command, install)
}

// refreshHooksFile points the hooks terma wrote into each Antigravity home's file at this
// terma, as `terma update --refresh` does after the binary moved, and names each file it
// changed. A file without them is left alone, but for one: a home whose file names no terma
// hook at all gets it, when another home's file has terma's hook switched on, so a home a
// client made after setup is covered (the developer chose Antigravity with hooks, as that
// other file says). A hook already named terma there, switched off or edited, is the
// developer's and stays, and one switched off is never copied to another home. While the
// agy on PATH is too old to run them, no home gets it, as PlanUserHooks says. Not on
// Windows, where terma does not offer Antigravity and a shared home holds another system's
// hooks.
func (a Agent) refreshHooksFile() (string, bool, error) {
	if runtime.GOOS == "windows" {
		return "", false, nil
	}
	paths, err := a.UserHooksPaths()
	if err != nil {
		return "", false, err
	}
	var carry, bare []string
	var errs []error
	on := false
	for _, path := range paths {
		data, err := hookmgr.ReadFile(path)
		switch {
		case err != nil:
			errs = append(errs, err)
		case bytes.Contains(data, []byte(" hook --user antigravity-")):
			carry = append(carry, path)
			on = on || termaHookOn(data)
		case namesNoTermaHook(data):
			bare = append(bare, path)
		}
	}
	if len(carry) == 0 {
		return "", false, errors.Join(errs...)
	}
	if !on {
		bare = nil
	} else if _, old := tooOld(context.Background()); old {
		bare = nil
	}
	terma, err := procinfo.AbsExecutable()
	if err != nil {
		return "", false, errors.Join(append(errs, err)...)
	}
	var changed []string
	for _, path := range carry {
		if _, ok, err := refreshHooks(path, terma); err != nil {
			errs = append(errs, err)
		} else if ok {
			changed = append(changed, path)
		}
	}
	for _, path := range bare {
		if ok, err := installHooks(path, terma); err != nil {
			errs = append(errs, err)
		} else if ok {
			changed = append(changed, path)
		}
	}
	return strings.Join(changed, ", "), len(changed) > 0, errors.Join(errs...)
}

// namesNoTermaHook reports whether a hooks file, data as read (nil when absent), has no
// top-level terma member; one refresh cannot parse is not judged to have none.
func namesNoTermaHook(data []byte) bool {
	_, top, err := parseHooks(data)
	if err != nil {
		return false
	}
	_, named := top[antigravityHookName]
	return !named
}

// installHooks writes terma's hook into the hooks file at path as setup does, creating its
// folder, and reports whether it changed the file.
func installHooks(path, terma string) (bool, error) {
	dir := filepath.Dir(path)
	plan, err := planHooks(dir, hookmgr.UserHookCommand(terma), modeInstall)
	if err != nil || plan.Empty() {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	return true, hookmgr.ApplyThroughLinks(dir, plan)
}

// refreshHooks rewrites the entries of terma's already in the hooks file at path to run
// terma there, and nothing else: it adds no event and leaves the hook on or off as it is.
func refreshHooks(path, terma string) (string, bool, error) {
	dir := filepath.Dir(path)
	plan, err := planHooks(dir, hookmgr.UserHookCommand(terma), modeRefresh)
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

// planMode is what a plan does to terma's entries: put them in, take them out, or point
// the ones already there at another terma.
type planMode int

const (
	modeInstall planMode = iota
	modeRemove
	modeRefresh
)

// planUserHooks puts terma's entries into the one named hook terma owns, or takes them
// out; see planHooks.
func planUserHooks(dir string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	if install {
		return planHooks(dir, command, modeInstall)
	}
	return planHooks(dir, command, modeRemove)
}

// planHooks changes terma's entries in its named hook and nothing else of the developer's:
// other named hooks, their handlers inside terma's, and its other fields stay as read.
// agy reads its file as JSONC: one with comments or trailing commas is patched in place,
// event by event, its comments kept. Installing also switches terma's hook back on, as a
// note says: agy runs none of a hook set "enabled": false. Removal never fails on what it
// did not write: a file it cannot parse that holds nothing of terma's, or a part of
// terma's hook that is not the shape terma writes, is left as it is.
func planHooks(dir string, command func(event string) string, mode planMode) (hookmgr.Plan, error) {
	const path = "hooks.json"
	full := filepath.Join(dir, path)
	p := hookmgr.Plan{}
	before, err := hookmgr.ReadFile(full)
	if err != nil {
		return p, err
	}
	doc, top, err := parseHooks(before)
	if err != nil {
		if mode != modeInstall && !bytes.Contains(before, []byte(" hook --user antigravity-")) {
			return p, nil
		}
		return p, fmt.Errorf("parse %s: %w", full, err)
	}
	member := map[string]json.RawMessage{}
	current, present := top[antigravityHookName]
	if present {
		if err := json.Unmarshal(current, &member); err != nil || member == nil {
			if mode != modeInstall {
				return p, nil
			}
			return p, fmt.Errorf("parse %s: hook %q is not an object", full, antigravityHookName)
		}
	}
	read := maps.Clone(member)
	own, err := ownEntries(command)
	if err != nil {
		return p, err
	}
	changed, switchedOn, theirs := false, false, false
	if raw, ok := member["enabled"]; mode == modeInstall && ok && string(bytes.TrimSpace(raw)) == "false" {
		delete(member, "enabled")
		changed, switchedOn = true, true
	}
	for _, h := range antigravityHooks {
		var entries, kept []json.RawMessage
		if raw, ok := member[h.Event]; ok {
			if err := json.Unmarshal(raw, &entries); err != nil {
				if mode != modeInstall {
					continue
				}
				return p, fmt.Errorf("parse %s: %s.%s is not a list: %w", full, antigravityHookName, h.Event, err)
			}
		}
		ours, hadOurs := false, false
		for _, entry := range entries {
			if mode != modeRemove && !ours && hookmgr.SameJSON(entry, own[h.Event]) {
				kept, ours = append(kept, entry), true
				continue
			}
			remaining, removed, err := withoutTerma(entry)
			if err != nil {
				if mode != modeInstall {
					kept, theirs = append(kept, entry), true
					continue
				}
				return p, fmt.Errorf("parse %s: %s.%s: %w", full, antigravityHookName, h.Event, err)
			}
			changed, hadOurs = changed || removed, hadOurs || removed
			if remaining != nil {
				kept, theirs = append(kept, remaining), true
			}
		}
		if !ours && (mode == modeInstall || mode == modeRefresh && hadOurs) {
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
		if theirs || listsOtherHandlers(member) {
			note += ", and with it the handlers you added to that hook"
		}
		p.Notes = append(p.Notes, note)
	}
	if len(member) > 0 {
		value, err := hookmgr.MarshalJSON(member, "  ", "  ")
		if err != nil {
			return p, err
		}
		top[antigravityHookName] = value
	} else {
		delete(top, antigravityHookName)
	}
	if mode == modeRemove && len(top) == 0 {
		p.Changes = append(p.Changes, hookmgr.Change{Path: path, Before: before})
		return p, nil
	}
	var after []byte
	if doc == nil {
		if after, err = hookmgr.MarshalOrdered(top); err != nil {
			return p, err
		}
	} else if after, err = patchHooks(doc, present, read, member); err != nil {
		return p, fmt.Errorf("patch %s: %w", full, err)
	}
	p.Changes = append(p.Changes, hookmgr.Change{Path: path, Before: before, After: append(bytes.TrimRight(after, "\n"), '\n')})
	return p, nil
}

// listsOtherHandlers reports whether terma's hook lists handlers under an event terma does
// not write, agy's PreToolUse or SessionStart say: those are the developer's, and come back
// on with the hook. A field that is no list, or an empty list, holds none; a list's entries
// are counted as they stand, an empty group too, which errs toward the warning.
func listsOtherHandlers(member map[string]json.RawMessage) bool {
	for key, raw := range member {
		own := key == "enabled"
		for _, h := range antigravityHooks {
			own = own || h.Event == key
		}
		var handlers []json.RawMessage
		if !own && json.Unmarshal(raw, &handlers) == nil && len(handlers) > 0 {
			return true
		}
	}
	return false
}

// withoutTerma is hookmgr.WithoutTerma, keeping rather than refusing a handler of a group
// that is not an object: terma's own still go. An entry that is no object is an error.
func withoutTerma(entry json.RawMessage) (json.RawMessage, bool, error) {
	remaining, removed, err := hookmgr.WithoutTerma(entry)
	if err == nil {
		return remaining, removed, nil
	}
	var group map[string]json.RawMessage
	var handlers []json.RawMessage
	if json.Unmarshal(entry, &group) != nil || json.Unmarshal(group["hooks"], &handlers) != nil {
		return nil, false, err
	}
	var kept []json.RawMessage
	removed = false
	for _, handler := range handlers {
		rest, gone, err := hookmgr.WithoutTerma(handler)
		if err != nil {
			kept = append(kept, handler)
			continue
		}
		removed = removed || gone
		if rest != nil {
			kept = append(kept, rest)
		}
	}
	if !removed {
		return entry, false, nil
	}
	if len(kept) == 0 {
		return nil, true, nil
	}
	if group["hooks"], err = hookmgr.MarshalJSON(kept, "", ""); err != nil {
		return nil, false, err
	}
	out, err := hookmgr.MarshalJSON(group, "", "")
	return out, true, err
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

// patchHooks writes terma's named hook into a JSONC file as member, the hook read
// before (when present) as read: the whole hook when it is new or goes, else only the
// fields of it that changed, so the developer's comments elsewhere, in terma's hook too,
// stay.
func patchHooks(doc *hujson.Value, present bool, read, member map[string]json.RawMessage) ([]byte, error) {
	hook := "/" + antigravityHookName
	var ops []map[string]any
	switch {
	case len(member) == 0:
		ops = append(ops, map[string]any{"op": "remove", "path": hook})
	case !present:
		ops = append(ops, map[string]any{"op": "add", "path": hook, "value": member})
	default:
		keys := slices.Sorted(maps.Keys(read))
		for _, k := range slices.Sorted(maps.Keys(member)) {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
		for _, k := range keys {
			pointer := hook + "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(k)
			value, keep := member[k]
			switch old, had := read[k]; {
			case !keep:
				ops = append(ops, map[string]any{"op": "remove", "path": pointer})
			case !had || !hookmgr.SameJSON(old, value):
				ops = append(ops, map[string]any{"op": "add", "path": pointer, "value": value})
			}
		}
	}
	patch, err := json.Marshal(ops)
	if err != nil {
		return nil, err
	}
	if err := doc.Patch(patch); err != nil {
		return nil, err
	}
	doc.Format()
	return doc.Pack(), nil
}
