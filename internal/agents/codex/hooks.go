package codex

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// codexHooks forward each event's JSON to the binary. PostToolUse is async because it
// fires on every tool call and nothing terma returns changes what Codex does; SessionEnd
// asks for Codex's 3-second maximum (default 1).
var codexHooks = []struct {
	Event   string
	Hook    string
	Async   bool
	Timeout int
}{
	{"SessionStart", "codex-session-start", false, 10},
	{"UserPromptSubmit", "codex-user-prompt-submit", true, 10},
	// Recorded before the tool runs, so PostToolUse can report the elapsed time.
	{"PreToolUse", "codex-pre-tool-use", false, 10},
	// Codex sends repository hooks no approval decision, only the request.
	{"PermissionRequest", "codex-permission-request", false, 10},
	{"PostToolUse", "codex-post-tool-use", true, 10},
	// Synchronous so the local snapshot finishes before codex exec exits.
	{"Stop", "codex-stop", false, 3},
	{"SessionEnd", "codex-session-end", false, 3},
	// SubagentStop stays synchronous so its event is spooled before the parent's Stop.
	{"SubagentStart", "codex-subagent-start", true, 10},
	{"SubagentStop", "codex-subagent-stop", false, 3},
}

type codexHookHandler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
	Async   bool   `json:"async,omitempty"`
}

// planUserHooks merges terma's machine-wide hooks into $CODEX_HOME/hooks.json; Codex
// denies unknown fields, so only `description` and `hooks` are written at the top level.
func planUserHooks(codexHome string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	// A group is terma's when a handler calls the binary. Codex trusts by entry hash, so an
	// updated group must be trusted again; doctor reports it until then.
	own, err := ownGroups(command)
	if err != nil {
		return hookmgr.Plan{}, err
	}
	return hookmgr.MergeEventHooks(codexHome, hookmgr.HooksFile{Path: "hooks.json"}, own, install)
}

// ownGroups are the groups terma writes, one per hook, running command.
func ownGroups(command func(event string) string) ([]hookmgr.EventHook, error) {
	own := make([]hookmgr.EventHook, 0, len(codexHooks))
	for _, h := range codexHooks {
		// No matcher from terma: which calls edit files is the binary's to decide.
		entry, err := hookmgr.Group(h.Event, "", codexHookHandler{
			Type: "command", Command: command(h.Hook), Timeout: h.Timeout, Async: h.Async,
		})
		if err != nil {
			return nil, err
		}
		own = append(own, entry)
	}
	return own, nil
}

// Entry is one of terma's hook entries in a hooks file, located by event, group and
// handler position.
type Entry struct {
	Event   string
	Group   int
	Handler int
	Hash    string
}

// Key is the entry's name in Codex's trust records: "<event>:<group>:<handler>", the
// event in snake_case.
func (e Entry) Key() string {
	var b strings.Builder
	for i, r := range e.Event {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return fmt.Sprintf("%s:%d:%d", b.String(), e.Group, e.Handler)
}

// termaEntriesIn lists terma's entries in the hooks file at path.
func termaEntriesIn(path string) ([]Entry, error) {
	before, err := hookmgr.ReadFile(path)
	if err != nil || before == nil {
		return nil, err
	}
	var doc struct {
		Hooks map[string][]struct {
			Matcher *string           `json:"matcher"`
			Hooks   []json.RawMessage `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(before, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var out []Entry
	for _, h := range codexHooks {
		for g, group := range doc.Hooks[h.Event] {
			for i, handler := range group.Hooks {
				if hookmgr.CallsTerma(handler) {
					entry := Entry{Event: h.Event, Group: g, Handler: i}
					entry.Hash, err = codexEntryHash(entry, group.Matcher, handler)
					if err != nil {
						return nil, err
					}
					out = append(out, entry)
				}
			}
		}
	}
	return out, nil
}

// ownEntries finds, in the hooks file at path, the groups that are exactly ones terma
// writes with command: a group someone else wrote or edited is not terma's to approve,
// even when it calls terma. A missing file has none.
func ownEntries(path string, command func(event string) string) ([]Entry, error) {
	data, err := hookmgr.ReadFile(path)
	if err != nil || data == nil {
		return nil, err
	}
	var doc struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	own, err := ownGroups(command)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, h := range own {
		for g, raw := range doc.Hooks[h.Event] {
			if !hookmgr.SameJSON(raw, h.Entry) {
				continue
			}
			var group struct {
				Hooks []json.RawMessage `json:"hooks"`
			}
			if err := json.Unmarshal(raw, &group); err != nil || len(group.Hooks) != 1 {
				continue
			}
			entry := Entry{Event: h.Event, Group: g}
			if entry.Hash, err = codexEntryHash(entry, nil, group.Hooks[0]); err != nil {
				return nil, err
			}
			out = append(out, entry)
		}
	}
	return out, nil
}

// codexEntryHash matches Codex's command-hook identity: event, group and handler as
// canonical JSON, SHA-256, as Codex records it once the entry is approved.
func codexEntryHash(entry Entry, matcher *string, raw json.RawMessage) (string, error) {
	var handler map[string]any
	if err := json.Unmarshal(raw, &handler); err != nil {
		return "", err
	}
	normalized := map[string]any{
		"type":    handler["type"],
		"command": handler["command"],
		"async":   false,
	}
	for _, key := range []string{"async", "timeout", "commandWindows", "statusMessage", "additionalContextLimit"} {
		if v, ok := handler[key]; ok {
			normalized[key] = v
		}
	}
	identity := map[string]any{"event_name": strings.SplitN(entry.Key(), ":", 2)[0], "hooks": []any{normalized}}
	if matcher != nil {
		identity["matcher"] = *matcher
	}
	b, err := hookmgr.MarshalJSON(identity, "", "")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%x", sum), nil
}

// managedRequirements is the [hooks] table of a requirements.toml; Codex runs managed
// hooks with no trust step.
func managedRequirements(command func(event string) string) string {
	var b strings.Builder
	b.WriteString("# terma: hooks for every Codex session on this machine.\n[hooks]\n")
	for _, h := range codexHooks {
		fmt.Fprintf(&b, "\n[[hooks.%s]]\n\n[[hooks.%s.hooks]]\ntype = \"command\"\ncommand = %s\ntimeout = %d\n", h.Event, h.Event, tomlString(command(h.Hook)), h.Timeout)
		if h.Async {
			b.WriteString("async = true\n")
		}
	}
	return b.String()
}

func tomlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}
