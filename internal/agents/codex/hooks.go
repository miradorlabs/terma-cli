package codex

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// hooksPath loads only in a trusted project, after each developer trusts its entries
// from inside Codex. Export cannot live here: Codex strips `otel` from project config.
const hooksPath = ".codex/hooks.json"

// committedHooks forward each event's JSON to the binary. PostToolUse is async because it
// fires on every tool call and nothing terma returns changes what Codex does; SessionEnd
// asks for Codex's 3-second maximum (default 1).
var committedHooks = []struct {
	Event   string
	Command string
	Async   bool
	Timeout int
}{
	{"SessionStart", hookCommand("codex-session-start"), false, 10},
	{"UserPromptSubmit", hookCommand("codex-user-prompt-submit"), true, 10},
	// Recorded before the tool runs, so PostToolUse can report the elapsed time.
	{"PreToolUse", hookCommand("codex-pre-tool-use"), false, 10},
	// Codex sends repository hooks no approval decision, only the request.
	{"PermissionRequest", hookCommand("codex-permission-request"), false, 10},
	{"PostToolUse", hookCommand("codex-post-tool-use"), true, 10},
	// Synchronous so the local snapshot finishes before codex exec exits.
	{"Stop", hookCommand("codex-stop"), false, 3},
	{"SessionEnd", hookCommand("codex-session-end"), false, 3},
	// SubagentStop stays synchronous so its event is spooled before the parent's Stop.
	{"SubagentStart", hookCommand("codex-subagent-start"), true, 10},
	{"SubagentStop", hookCommand("codex-subagent-stop"), false, 3},
}

// hookCommand finds terma with the small GUI PATH a desktop-launched Codex gets.
func hookCommand(event string) string { return hookmgr.PathHookCommand(event) }

// hasConfig is when wiring hooks by default helps rather than leaves a stray directory.
func hasConfig(root string) bool {
	info, err := os.Stat(filepath.Join(root, ".codex"))
	return err == nil && info.IsDir()
}

type codexHookHandler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
	Async   bool   `json:"async,omitempty"`
}

// planHooks merges terma's hooks into .codex/hooks.json; Codex denies unknown fields, so
// only `description` and `hooks` are written at the top level.
func planHooks(root string, install bool) (hookmgr.Plan, error) {
	return planCodex(root, hooksPath, hookCommand, install)
}

// planUserHooks writes global mode's machine-wide hooks to $CODEX_HOME/hooks.json.
func planUserHooks(codexHome string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	return planCodex(codexHome, "hooks.json", command, install)
}

func planCodex(root, path string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	// A group is terma's when a handler calls the binary. Codex trusts by entry hash, so an
	// updated group must be trusted again; doctor reports it until then.
	own := make([]hookmgr.EventHook, 0, len(committedHooks))
	for _, h := range committedHooks {
		// No matcher from terma: which calls edit files is the binary's to decide.
		entry, err := hookmgr.Group(h.Event, "", codexHookHandler{
			Type: "command", Command: command(hookmgr.HookEventOf(h.Command)), Timeout: h.Timeout, Async: h.Async,
		})
		if err != nil {
			return hookmgr.Plan{}, err
		}
		own = append(own, entry)
	}
	return hookmgr.MergeEventHooks(root, hookmgr.HooksFile{Path: path}, own, install)
}

// Entry is one of terma's hook entries in .codex/hooks.json, located by event, group and
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

// TermaEntries lists terma's entries in committedHooks order, so doctor can name the ones
// Codex skips in silence because they were added after the file was trusted.
func TermaEntries(root string) ([]Entry, error) {
	return termaEntriesIn(filepath.Join(root, filepath.FromSlash(hooksPath)))
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
	for _, h := range committedHooks {
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

// codexEntryHash matches Codex's command-hook identity: event, group and handler as
// canonical JSON, SHA-256. Only Codex can grant trust for it.
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
	b.WriteString("# terma: global mode's hooks for every Codex session on this machine.\n[hooks]\n")
	for _, h := range committedHooks {
		fmt.Fprintf(&b, "\n[[hooks.%s]]\n\n[[hooks.%s.hooks]]\ntype = \"command\"\ncommand = %s\ntimeout = %d\n", h.Event, h.Event, tomlString(command(hookmgr.HookEventOf(h.Command))), h.Timeout)
		if h.Async {
			b.WriteString("async = true\n")
		}
	}
	return b.String()
}

func tomlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}
