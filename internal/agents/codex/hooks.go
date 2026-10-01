package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// --- Codex project hooks --------------------------------------------------------------

// hooksPath is Codex's project-scope hooks file. Codex loads it only when the
// project's `.codex` layer is trusted, and each developer trusts a repository's hooks
// once, from inside Codex — see doctor's codex-hooks check, which is what tells them.
//
// This is the repository-specific capture surface for Codex Desktop. Native OTLP
// telemetry cannot live here: Codex strips `otel` (with `notify`, `profile` and the provider keys) out of
// project-local config and says so at startup, so a Codex export is always driven from
// outside the repository — the machine-wide user config `terma connect codex` writes, or
// the runtime `-c` overrides `terma install` routes through the shim (keeping the
// developer's own CODEX_HOME). Trusted hooks report Desktop activity directly.
const hooksPath = ".codex/hooks.json"

// committedHooks are the adapter shims for Codex. Each is a one-liner that forwards the
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
var committedHooks = []struct {
	Event   string
	Command string
	Async   bool
	Timeout int
}{
	{"SessionStart", hookCommand("codex-session-start"), false, 10},
	{"UserPromptSubmit", hookCommand("codex-user-prompt-submit"), true, 10},
	// Record the start before the tool runs. PostToolUse can then report an
	// observed elapsed time under the same tool_use_id.
	{"PreToolUse", hookCommand("codex-pre-tool-use"), false, 10},
	// This only observes that approval was requested. Codex does not send the
	// eventual user decision back to repository hooks.
	{"PermissionRequest", hookCommand("codex-permission-request"), false, 10},
	{"PostToolUse", hookCommand("codex-post-tool-use"), true, 10},
	// Finish the bounded local snapshot before codex exec can shut down. An
	// async Stop may be cancelled at exit; network delivery stays detached.
	{"Stop", hookCommand("codex-stop"), false, 3},
	{"SessionEnd", hookCommand("codex-session-end"), false, 3},
	// Subagents run inside the thread and name themselves (agent_id / agent_type).
	// SubagentStart is async like PostToolUse: nothing terma returns changes what Codex
	// does. SubagentStop stays synchronous and cheap so its event is spooled before the
	// parent's Stop.
	{"SubagentStart", hookCommand("codex-subagent-start"), true, 10},
	{"SubagentStop", hookCommand("codex-subagent-stop"), false, 3},
}

// hookCommand also finds user-installed binaries when Codex Desktop was
// launched with macOS's small GUI PATH. Its hook entry is committed, so the
// directories must be portable across developers and their install methods.
func hookCommand(event string) string {
	return `PATH="${PATH:-/usr/bin:/bin}:$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin"; ` + hookmgr.HookCommand(event)
}

// hasConfig reports whether the repository already carries Codex configuration — a
// .codex directory — which is when wiring its hooks by default is a help rather than a
// stray directory in a repository nobody opens in Codex.
func hasConfig(root string) bool {
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

// planHooks merges terma's hooks into .codex/hooks.json without disturbing
// anything else in the file.
//
// Codex parses this file with unknown fields denied, so nothing is invented at the top
// level: only `description` and `hooks` may appear, and both are written back as they
// were read.
func planHooks(root string, install bool) (hookmgr.Plan, error) {
	return planCodex(root, hooksPath, hookCommand, install)
}

// planUserHooks merges terma's hooks into Codex's user-level hooks file
// ($CODEX_HOME/hooks.json), with command naming each event: global mode's machine-wide
// hooks. Codex runs them in every workspace once the developer trusts them — or with
// no trust step when the organization deploys the same entries as managed config.
func planUserHooks(codexHome string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	return planCodex(codexHome, "hooks.json", command, install)
}

func planCodex(root, path string, command func(event string) string, install bool) (hookmgr.Plan, error) {
	// A group is terma's when one of its handlers calls the binary, which keeps a
	// developer's own group in the same event untouched. Codex trusts a hook by the hash
	// of its entry, so a group brought up to date from an older terma is one the
	// developer is asked to trust again; doctor reports that until they do.
	own := make([]hookmgr.EventHook, 0, len(committedHooks))
	for _, h := range committedHooks {
		handler, err := hookmgr.MarshalJSON(codexHookHandler{
			Type: "command", Command: command(hookmgr.HookEventOf(h.Command)), Timeout: h.Timeout, Async: h.Async,
		}, "", "")
		if err != nil {
			return hookmgr.Plan{}, err
		}
		group, err := hookmgr.MarshalJSON(codexMatcherGroup{Hooks: []json.RawMessage{handler}}, "", "")
		if err != nil {
			return hookmgr.Plan{}, err
		}
		own = append(own, hookmgr.EventHook{Event: h.Event, Entry: group})
	}
	return hookmgr.MergeEventHooks(root, hookmgr.HooksFile{Path: path}, own, install)
}

// Entry is one of terma's hook entries in .codex/hooks.json, located the way Codex
// locates it: the event, the group's position under that event, and the handler's
// position in the group.
type Entry struct {
	Event   string
	Group   int
	Handler int
	Hash    string
}

// Key is the entry's name in Codex's trust records, after the hooks file's path:
// "<event>:<group>:<handler>", with the event in snake_case (SubagentStart is
// subagent_start).
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

// TermaEntries lists terma's entries in the repository's hooks file, in the order
// CodexHooks declares their events. Codex trusts a hook entry by entry, so this is what
// lets doctor name the ones a newer terma added to a file the developer had already
// trusted: they have no record, Codex skips them, and nothing says so. A missing file
// has no entries.
func TermaEntries(root string) ([]Entry, error) {
	before, err := hookmgr.ReadFile(filepath.Join(root, filepath.FromSlash(hooksPath)))
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
		return nil, fmt.Errorf("parse %s: %w", hooksPath, err)
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

// codexEntryHash matches Codex's normalized command-hook identity: the event,
// matcher group, and one handler, serialized as canonical JSON and SHA-256.
// It is read-only; only Codex can grant trust for this hash.
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
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(identity); err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}))
	return fmt.Sprintf("sha256:%x", sum), nil
}

// managedRequirements is the [hooks] table of a Codex requirements.toml holding
// terma's hooks (codex-rs config: ManagedHooksRequirementsToml, the events flattened
// under it). Codex runs managed hooks without asking anyone to trust them.
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

// tomlString quotes s as a TOML basic string.
func tomlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}
