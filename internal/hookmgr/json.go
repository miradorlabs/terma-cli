package hookmgr

// JSON as the agents' hook files need it: terma's entries recognised, documents compared
// by meaning, and written without HTML escaping and in a stable key order.

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// callsTerma reports whether an entry contains a recognized Terma command, including
// older unguarded commands. Mentions inside user scripts are not ownership evidence.
func callsTerma(entry json.RawMessage) bool {
	var v any
	if json.Unmarshal(entry, &v) != nil {
		return false
	}
	return anyOwnedCommand(v)
}

func anyOwnedCommand(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for key, value := range t {
			if key == "command" {
				if s, ok := value.(string); ok && ownedHookCommand(s) {
					return true
				}
				continue
			}
			if anyOwnedCommand(value) {
				return true
			}
		}
	case []any:
		for _, value := range t {
			if anyOwnedCommand(value) {
				return true
			}
		}
	}
	return false
}

// sameJSON compares two documents by value, so a reformatted but unchanged entry does
// not read as a change.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// marshalJSON encodes without HTML escaping. A hook command carries `>` and `&&`, and
// encoding/json's default would commit them as `\u003e` and `\u0026`: valid JSON that
// nobody reviewing the file can read, and a rewrite of any user hook that carries a
// redirect. An empty indent compacts.
func marshalJSON(v any, prefix, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent != "" {
		enc.SetIndent(prefix, indent)
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// marshalOrdered writes the top-level object with sorted keys and each value's
// bytes verbatim, so settings terma does not own survive byte-for-byte (their
// indentation included) and the committed file diffs stably across installs.
func marshalOrdered(m map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, k := range keys {
		kb, _ := json.Marshal(k)
		buf.WriteString("  ")
		buf.Write(kb)
		buf.WriteString(": ")
		buf.Write(bytes.TrimSpace(m[k]))
		if i < len(keys)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("}")
	return buf.Bytes(), nil
}

// ownedHookCommand recognizes generated commands and their older unguarded forms.
// Merely mentioning "terma hook" in a user's script is not ownership evidence.
func ownedHookCommand(command string) bool {
	command = strings.TrimSpace(command)
	var commands []string
	for _, h := range ClaudeHooks {
		commands = append(commands, h.Command)
	}
	for _, h := range CursorHooks {
		commands = append(commands, h.Command)
	}
	for _, h := range CodexHooks {
		commands = append(commands, h.Command)
	}
	for _, h := range AntigravityHooks {
		commands = append(commands, h.Command)
	}
	for _, guarded := range commands {
		// Codex's commands lead with a PATH assignment (CodexHookCommand); the forms an
		// older terma wrote did not, and must still be recognized to be upgraded in place.
		plain := guarded
		if i := strings.Index(plain, "; command -v terma "); i >= 0 {
			plain = plain[i+2:]
		}
		bare := strings.TrimSuffix(strings.TrimPrefix(plain, "command -v terma >/dev/null 2>&1 && "), " || true")
		if command == guarded || command == plain || command == bare || command == bare+" || true" {
			return true
		}
	}
	return false
}

// withoutTerma removes only owned command leaves, retaining unrelated handlers in
// the same group and their matcher/options. A nil result is an entirely owned entry.
func withoutTerma(entry json.RawMessage) (json.RawMessage, bool, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(entry, &obj); err != nil {
		return nil, false, err
	}
	var command string
	if json.Unmarshal(obj["command"], &command) == nil && ownedHookCommand(command) {
		return nil, true, nil
	}
	raw, ok := obj["hooks"]
	if !ok {
		return entry, false, nil
	}
	var handlers []json.RawMessage
	if err := json.Unmarshal(raw, &handlers); err != nil {
		return nil, false, err
	}
	var kept []json.RawMessage
	changed := false
	for _, handler := range handlers {
		remaining, removed, err := withoutTerma(handler)
		if err != nil {
			return nil, false, err
		}
		changed = changed || removed
		if remaining != nil {
			kept = append(kept, remaining)
		}
	}
	if !changed {
		return entry, false, nil
	}
	if len(kept) == 0 {
		return nil, true, nil
	}
	encoded, err := marshalJSON(kept, "", "")
	if err != nil {
		return nil, false, err
	}
	obj["hooks"] = encoded
	out, err := marshalJSON(obj, "", "")
	return out, true, err
}
