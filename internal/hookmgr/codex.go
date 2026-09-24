package hookmgr

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// --- Codex project hooks --------------------------------------------------------------

// CodexHooksPath is Codex's project-scope hooks file. Codex loads it only when the
// project's `.codex` layer is trusted, and each developer trusts a repository's hooks
// once, from inside Codex — see doctor's codex-hooks check, which is what tells them.
//
// This is the repository-specific capture surface for Codex Desktop. Native OTLP
// telemetry cannot live here: Codex strips `otel` (with `notify`, `profile` and the provider keys) out of
// project-local config and says so at startup, so a Codex export is always driven from
// outside the repository — the machine-wide user config `terma connect codex` writes, or
// the runtime `-c` overrides `terma install` routes through the shim (keeping the
// developer's own CODEX_HOME). Trusted hooks report Desktop activity directly.
const CodexHooksPath = ".codex/hooks.json"

// CodexHooks are the adapter shims for Codex. Each is a one-liner that forwards the
// hook's JSON to the binary; no logic lives here.
//
// PostToolUse is asynchronous because it fires on every tool call — every shell command
// a session runs — and nothing terma returns can change what Codex does with the result.
// Making the agent wait on a spawn it has no use for would be a latency tax on every
// command. SessionStart stays synchronous: it happens once, and the session should be
// recorded before the first edit arrives.
//
// SessionEnd's timeout is Codex's maximum. Codex allows 3 seconds there and defaults to
// 1; the handler only clears local state and appends to the spool, and the network flush
// it triggers is detached, so the hook returns long before either bound.
var CodexHooks = []struct {
	Event   string
	Command string
	Async   bool
	Timeout int
}{
	{"SessionStart", CodexHookCommand("codex-session-start"), false, 10},
	{"UserPromptSubmit", CodexHookCommand("codex-user-prompt-submit"), true, 10},
	{"PostToolUse", CodexHookCommand("codex-post-tool-use"), true, 10},
	// Finish the bounded local snapshot before codex exec can shut down. An
	// async Stop may be cancelled at exit; network delivery stays detached.
	{"Stop", CodexHookCommand("codex-stop"), false, 3},
	{"SessionEnd", CodexHookCommand("codex-session-end"), false, 3},
	// Subagents run inside the thread and name themselves (agent_id / agent_type).
	// SubagentStart is async like PostToolUse: nothing terma returns changes what Codex
	// does. SubagentStop stays synchronous and cheap so its event is spooled before the
	// parent's Stop.
	{"SubagentStart", CodexHookCommand("codex-subagent-start"), true, 10},
	{"SubagentStop", CodexHookCommand("codex-subagent-stop"), false, 3},
}

// CodexHookCommand also finds user-installed binaries when Codex Desktop was
// launched with macOS's small GUI PATH. Its hook entry is committed, so the
// directories must be portable across developers and their install methods.
func CodexHookCommand(event string) string {
	return `PATH="${PATH:-/usr/bin:/bin}:$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin"; ` + HookCommand(event)
}

// HasCodex reports whether the repository already carries Codex configuration — a
// .codex directory — which is when wiring its hooks by default is a help rather than a
// stray directory in a repository nobody opens in Codex.
func HasCodex(root string) bool {
	info, err := os.Stat(filepath.Join(root, ".codex"))
	return err == nil && info.IsDir()
}

// codexHookHandler is one entry in a matcher group's `hooks` array. The field names are
// Codex's own contract (codex-rs/config, HookHandlerConfig).
type codexHookHandler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
	Async   bool   `json:"async,omitempty"`
}

// codexMatcherGroup is one element of an event's array: an optional matcher and the
// handlers that run when it matches. terma writes no matcher — which tool calls carry a
// file edit is a question for the binary, not for a regex frozen into a committed file,
// and a matcher would go stale the moment Codex adds an edit tool.
type codexMatcherGroup struct {
	Matcher *string           `json:"matcher,omitempty"`
	Hooks   []json.RawMessage `json:"hooks"`
}

// PlanCodexHooks merges terma's hooks into .codex/hooks.json without disturbing
// anything else in the file.
//
// Codex parses this file with unknown fields denied, so nothing is invented at the top
// level: only `description` and `hooks` may appear, and both are written back as they
// were read.
func PlanCodexHooks(root string, install bool) (Plan, error) {
	// A group is terma's when one of its handlers calls the binary, which keeps a
	// developer's own group in the same event untouched. Codex trusts a hook by the hash
	// of its entry, so a group brought up to date from an older terma is one the
	// developer is asked to trust again; doctor reports that until they do.
	own := make([]eventHook, 0, len(CodexHooks))
	for _, h := range CodexHooks {
		handler, err := marshalJSON(codexHookHandler{
			Type: "command", Command: h.Command, Timeout: h.Timeout, Async: h.Async,
		}, "", "")
		if err != nil {
			return Plan{}, err
		}
		group, err := marshalJSON(codexMatcherGroup{Hooks: []json.RawMessage{handler}}, "", "")
		if err != nil {
			return Plan{}, err
		}
		own = append(own, eventHook{Event: h.Event, Entry: group})
	}
	return mergeEventHooks(root, hooksFile{Path: CodexHooksPath}, own, install)
}

// CodexEntry is one of terma's hook entries in .codex/hooks.json, located the way Codex
// locates it: the event, the group's position under that event, and the handler's
// position in the group.
type CodexEntry struct {
	Event   string
	Group   int
	Handler int
	Hash    string
}

// Key is the entry's name in Codex's trust records, after the hooks file's path:
// "<event>:<group>:<handler>", with the event in snake_case (SubagentStart is
// subagent_start).
func (e CodexEntry) Key() string {
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

// CodexTermaEntries lists terma's entries in the repository's hooks file, in the order
// CodexHooks declares their events. Codex trusts a hook entry by entry, so this is what
// lets doctor name the ones a newer terma added to a file the developer had already
// trusted: they have no record, Codex skips them, and nothing says so. A missing file
// has no entries.
func CodexTermaEntries(root string) ([]CodexEntry, error) {
	before, err := readFile(filepath.Join(root, filepath.FromSlash(CodexHooksPath)))
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
		return nil, fmt.Errorf("parse %s: %w", CodexHooksPath, err)
	}
	var out []CodexEntry
	for _, h := range CodexHooks {
		for g, group := range doc.Hooks[h.Event] {
			for i, handler := range group.Hooks {
				if callsTerma(handler) {
					entry := CodexEntry{Event: h.Event, Group: g, Handler: i}
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

// codexEntryHash matches Codex's normalized command-hook identity: the event,
// matcher group, and one handler, serialized as canonical JSON and SHA-256.
// It is read-only; only Codex can grant trust for this hash.
func codexEntryHash(entry CodexEntry, matcher *string, raw json.RawMessage) (string, error) {
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
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(identity); err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}))
	return fmt.Sprintf("sha256:%x", sum), nil
}
