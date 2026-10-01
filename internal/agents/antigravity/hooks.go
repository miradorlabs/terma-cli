package antigravity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// hooksPath is agy's workspace hooks file, loaded only for a trusted workspace. agy has no
// configurable OTLP exporter, so everything terma learns arrives through these hooks.
const hooksPath = ".agents/hooks.json"

// antigravityHookName is the top-level key terma owns whole in the file, which agy keys
// by hook name; uninstall removes exactly that key.
const antigravityHookName = "terma"

// antigravityCustomizationRoots are the directories agy treats as a workspace's
// customization root.
var antigravityCustomizationRoots = []string{".agents", ".agent", "_agents", "_agent"}

// committedHooks are terma's agy hooks. PostToolUse is unmatched, since a matcher frozen
// into a committed file would go stale as agy adds tools; there is no PreToolUse, which
// demands a decision. The timeout is a ceiling for a wedged filesystem, not a budget.
var committedHooks = []struct {
	Event   string
	Command string
	// Grouped events wrap their handlers in a matcher group, as agy requires per event.
	Grouped bool
}{
	{"PreInvocation", hookmgr.HookCommand("antigravity-pre-invocation"), false},
	{"PostToolUse", hookmgr.HookCommand("antigravity-post-tool-use"), true},
	{"PostInvocation", hookmgr.HookCommand("antigravity-post-invocation"), false},
	{"Stop", hookmgr.HookCommand("antigravity-stop"), false},
}

const antigravityHookTimeout = 10

// hasConfig reports whether the repository has one of agy's customization roots, so
// wiring its hooks by default adds no stray directory.
func hasConfig(root string) bool {
	for _, dir := range antigravityCustomizationRoots {
		if info, err := os.Stat(filepath.Join(root, dir)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// antigravityHandler is one command entry; field names are agy's contract.
type antigravityHandler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// antigravityGroup is one element of a grouped event's array; an empty matcher matches all.
type antigravityGroup struct {
	Matcher string            `json:"matcher"`
	Hooks   []json.RawMessage `json:"hooks"`
}

// antigravityEntry is terma's named hook, in the file's field order; Enabled is the
// developer's, never set by terma.
type antigravityEntry struct {
	Enabled        json.RawMessage   `json:"enabled,omitempty"`
	PreInvocation  []json.RawMessage `json:"PreInvocation,omitempty"`
	PostToolUse    []json.RawMessage `json:"PostToolUse,omitempty"`
	PostInvocation []json.RawMessage `json:"PostInvocation,omitempty"`
	Stop           []json.RawMessage `json:"Stop,omitempty"`
}

// renderAntigravityEntry builds terma's entry, carrying over the developer's `enabled`
// so an install never switches their choice back on.
func renderAntigravityEntry(previous json.RawMessage) (json.RawMessage, error) {
	entry := antigravityEntry{}
	if len(previous) > 0 {
		var prev struct {
			Enabled json.RawMessage `json:"enabled"`
		}
		if json.Unmarshal(previous, &prev) == nil && len(prev.Enabled) > 0 && string(prev.Enabled) != "null" {
			entry.Enabled = prev.Enabled
		}
	}
	for _, h := range committedHooks {
		handler, err := hookmgr.MarshalJSON(antigravityHandler{Type: "command", Command: h.Command, Timeout: antigravityHookTimeout}, "", "")
		if err != nil {
			return nil, err
		}
		value := handler
		if h.Grouped {
			if value, err = hookmgr.MarshalJSON(antigravityGroup{Matcher: "", Hooks: []json.RawMessage{handler}}, "", ""); err != nil {
				return nil, err
			}
		}
		switch h.Event {
		case "PreInvocation":
			entry.PreInvocation = append(entry.PreInvocation, value)
		case "PostToolUse":
			entry.PostToolUse = append(entry.PostToolUse, value)
		case "PostInvocation":
			entry.PostInvocation = append(entry.PostInvocation, value)
		case "Stop":
			entry.Stop = append(entry.Stop, value)
		}
	}
	return hookmgr.MarshalJSON(entry, "  ", "  ")
}

// planHooks merges terma's named hook into .agents/hooks.json, writing every other named
// hook back byte-for-byte.
func planHooks(root string, install bool) (hookmgr.Plan, error) {
	p := hookmgr.Plan{}
	path := filepath.Join(root, filepath.FromSlash(hooksPath))
	before, err := hookmgr.ReadFile(path)
	if err != nil {
		return p, err
	}
	top := map[string]json.RawMessage{}
	if before != nil {
		if err := json.Unmarshal(before, &top); err != nil {
			return p, fmt.Errorf("parse %s: %w", hooksPath, err)
		}
	}
	if top == nil {
		return p, fmt.Errorf("parse %s: expected a JSON object", hooksPath)
	}
	previous, present := top[antigravityHookName]
	if !install && !present {
		return p, nil
	}
	entry := map[string]json.RawMessage{}
	if present {
		if err := json.Unmarshal(previous, &entry); err != nil {
			return p, err
		}
	}
	if entry == nil {
		return p, fmt.Errorf("parse %s terma: expected a JSON object", hooksPath)
	}
	generated, err := renderAntigravityEntry(previous)
	if err != nil {
		return p, err
	}
	var desired map[string]json.RawMessage
	if err := json.Unmarshal(generated, &desired); err != nil {
		return p, err
	}
	for event, target := range desired {
		if event == "enabled" {
			continue
		}
		var entries, wanted []json.RawMessage
		if raw, ok := entry[event]; ok {
			if err := json.Unmarshal(raw, &entries); err != nil {
				return p, err
			}
		}
		if err := json.Unmarshal(target, &wanted); err != nil {
			return p, err
		}
		var kept []json.RawMessage
		for _, handler := range entries {
			remaining, _, err := hookmgr.WithoutTerma(handler)
			if err != nil {
				return p, err
			}
			if remaining != nil {
				kept = append(kept, remaining)
			}
		}
		if install {
			kept = append(kept, wanted...)
		}
		if len(kept) == 0 {
			delete(entry, event)
		} else {
			encoded, err := hookmgr.MarshalJSON(kept, "", "")
			if err != nil {
				return p, err
			}
			entry[event] = encoded
		}
	}
	// A user-set enabled switch or any extra field remains the user's.
	if len(entry) == 0 {
		delete(top, antigravityHookName)
	} else {
		encoded, err := hookmgr.MarshalJSON(entry, "  ", "  ")
		if err != nil {
			return p, err
		}
		if present && hookmgr.SameJSON(previous, encoded) {
			return p, nil
		}
		top[antigravityHookName] = encoded
	}
	if len(top) == 0 {
		p.Changes = append(p.Changes, hookmgr.Change{Path: hooksPath, Before: before})
		return p, nil
	}
	out, err := hookmgr.MarshalOrdered(top)
	if err != nil {
		return p, err
	}
	p.Changes = append(p.Changes, hookmgr.Change{Path: hooksPath, Before: before, After: append(out, '\n')})
	return p, nil
}

// hooksEnabled is false only when terma's entry says `"enabled": false`; a missing or
// unreadable file is the plan's question.
func hooksEnabled(root string) bool {
	data, err := hookmgr.ReadFile(filepath.Join(root, filepath.FromSlash(hooksPath)))
	if err != nil || data == nil {
		return true
	}
	var top map[string]struct {
		Enabled *bool `json:"enabled"`
	}
	if json.Unmarshal(data, &top) != nil {
		return true
	}
	entry, ok := top[antigravityHookName]
	return !ok || entry.Enabled == nil || *entry.Enabled
}
