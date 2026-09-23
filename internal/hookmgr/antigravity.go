package hookmgr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// --- Antigravity project hooks ----------------------------------------------------------

// AntigravityHooksPath is Antigravity CLI's workspace hooks file. agy discovers a
// repository's customizations under `.agents/` (rules, skills, plugins and this file),
// walking up from the working directory to the repository root, and loads them only for
// a workspace the developer has trusted from inside agy. The file is meant to be
// committed; doctor reports the trust gap while it is pending.
//
// This is the only thing terma can put in a repository for Antigravity. agy has no
// configurable OTLP exporter — its one telemetry switch reports to Google — so there is
// no export to point anywhere, and everything terma learns about an agy session arrives
// through these hooks.
const AntigravityHooksPath = ".agents/hooks.json"

// antigravityHookName is the top-level key terma owns in the file. agy's hooks.json is
// keyed by a hook *name*, each name holding its own per-event handler lists, and named
// hooks from different sources are merged at load time. Owning one name means an
// install never has to search another author's arrays for its own entries: the whole
// entry is terma's to write, and uninstall removes exactly that key.
const antigravityHookName = "terma"

// antigravityCustomizationRoots are the directories agy treats as a workspace's
// customization root. Any one of them marks a repository people open in Antigravity.
var antigravityCustomizationRoots = []string{".agents", ".agent", "_agents", "_agent"}

// AntigravityHooks are the adapter shims for Antigravity. Each is a one-liner that
// forwards the hook's JSON to the binary; no logic lives here.
//
// PreInvocation fires before each model call and carries the invocation number, which
// is how a turn's start is recognised. PostToolUse fires after every tool step —
// unmatched, because which tool names carry a file edit is a question for the binary
// (agy adds tools per model family) and a regex frozen into a committed file would go
// stale. PostInvocation and Stop carry the turn's shape: invocation count, termination
// reason, whether the loop is idle. There is no PreToolUse entry: agy requires a
// decision from that hook and terma never decides anything for an agent.
//
// agy runs hooks synchronously with a 30-second default timeout; terma's handlers
// return in milliseconds and the network flush after Stop is detached, so 10 seconds is
// a ceiling for a wedged filesystem, not a budget.
var AntigravityHooks = []struct {
	Event   string
	Command string
	// Grouped events wrap their handlers in a matcher group; flat events list the
	// handlers directly. The shape is agy's, per event, not a choice.
	Grouped bool
}{
	{"PreInvocation", HookCommand("antigravity-pre-invocation"), false},
	{"PostToolUse", HookCommand("antigravity-post-tool-use"), true},
	{"PostInvocation", HookCommand("antigravity-post-invocation"), false},
	{"Stop", HookCommand("antigravity-stop"), false},
}

const antigravityHookTimeout = 10

// HasAntigravity reports whether the repository already carries Antigravity
// configuration — one of agy's customization roots — which is when wiring its hooks by
// default is a help rather than a stray directory in a repository nobody opens in agy.
func HasAntigravity(root string) bool {
	for _, dir := range antigravityCustomizationRoots {
		if info, err := os.Stat(filepath.Join(root, dir)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// antigravityHandler is one command entry. The field names are agy's own contract.
type antigravityHandler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// antigravityGroup is one element of a grouped event's array. An empty matcher matches
// every tool.
type antigravityGroup struct {
	Matcher string            `json:"matcher"`
	Hooks   []json.RawMessage `json:"hooks"`
}

// antigravityEntry is terma's named hook. Field order is the file's order; Enabled is
// copied through from what the developer wrote, never set by terma.
type antigravityEntry struct {
	Enabled        json.RawMessage   `json:"enabled,omitempty"`
	PreInvocation  []json.RawMessage `json:"PreInvocation,omitempty"`
	PostToolUse    []json.RawMessage `json:"PostToolUse,omitempty"`
	PostInvocation []json.RawMessage `json:"PostInvocation,omitempty"`
	Stop           []json.RawMessage `json:"Stop,omitempty"`
}

// renderAntigravityEntry builds terma's entry, carrying over an `enabled` value the
// developer set on the previous one: switching terma's hooks off is their call, and an
// install must not silently switch them back on. doctor reports the switch.
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
	for _, h := range AntigravityHooks {
		handler, err := marshalJSON(antigravityHandler{Type: "command", Command: h.Command, Timeout: antigravityHookTimeout}, "", "")
		if err != nil {
			return nil, err
		}
		value := handler
		if h.Grouped {
			if value, err = marshalJSON(antigravityGroup{Matcher: "", Hooks: []json.RawMessage{handler}}, "", ""); err != nil {
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
	return marshalJSON(entry, "  ", "  ")
}

// PlanAntigravityHooks merges terma's named hook into .agents/hooks.json without
// disturbing anything else in the file: every other named hook is written back
// byte-for-byte.
func PlanAntigravityHooks(root string, install bool) (Plan, error) {
	p := Plan{}
	path := filepath.Join(root, filepath.FromSlash(AntigravityHooksPath))
	before, err := readFile(path)
	if err != nil {
		return p, err
	}
	top := map[string]json.RawMessage{}
	if before != nil {
		if err := json.Unmarshal(before, &top); err != nil {
			return p, fmt.Errorf("parse %s: %w", AntigravityHooksPath, err)
		}
	}
	previous, present := top[antigravityHookName]
	if install {
		entry, err := renderAntigravityEntry(previous)
		if err != nil {
			return p, err
		}
		if present && sameJSON(previous, entry) {
			return p, nil
		}
		top[antigravityHookName] = entry
	} else {
		if !present {
			return p, nil
		}
		delete(top, antigravityHookName)
		// Uninstall from a file that held nothing but terma's hook: remove it rather
		// than leave an empty object behind.
		if len(top) == 0 {
			p.Changes = append(p.Changes, Change{Path: AntigravityHooksPath, Before: before})
			return p, nil
		}
	}
	out, err := marshalOrdered(top)
	if err != nil {
		return p, err
	}
	p.Changes = append(p.Changes, Change{Path: AntigravityHooksPath, Before: before, After: append(out, '\n')})
	return p, nil
}

// AntigravityHooksEnabled reports whether terma's entry in the repository's hooks file
// is switched on. A missing file or entry is "enabled": there is nothing switched off,
// and whether the hooks are present is a separate question the plan answers. A file
// that cannot be read reports enabled for the same reason: this only ever says "you
// switched it off", and an unreadable file is the plan's error to raise.
func AntigravityHooksEnabled(root string) bool {
	data, err := readFile(filepath.Join(root, filepath.FromSlash(AntigravityHooksPath)))
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
