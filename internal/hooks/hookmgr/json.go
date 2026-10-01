package hookmgr

// JSON as the agents' hook files need it: terma's entries recognised, documents compared
// by meaning, and written without HTML escaping and in a stable key order.

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// committedHookShape is HookCommand's output, with CodexHookCommand's PATH prefix or
// without it, for any event.
var committedHookShape = regexp.MustCompile(`^(PATH="[^"]*"; )?command -v terma >/dev/null 2>&1 && terma hook [a-z0-9-]+ \|\| true$`)

// userHookShape is UserHookCommand's (and ManagedHookCommand's) output for any path
// and event.
var userHookShape = regexp.MustCompile(`^\[ -x ('(?:[^']|'\\'')+'|"(?:[^"\\]|\\.)+") \] && ('(?:[^']|'\\'')+'|"(?:[^"\\]|\\.)+") hook --user [a-z0-9-]+ \|\| true$`)

// CallsTerma reports whether an entry contains a command terma generated.
func CallsTerma(entry json.RawMessage) bool {
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
		if slices.ContainsFunc(t, anyOwnedCommand) {
			return true
		}
	}
	return false
}

// SameJSON compares two documents by value, so a reformatted but unchanged entry does
// not read as a change.
func SameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// MarshalJSON encodes without HTML escaping. A hook command carries `>` and `&&`, and
// encoding/json's default would commit them as `\u003e` and `\u0026`: valid JSON that
// nobody reviewing the file can read, and a rewrite of any user hook that carries a
// redirect. An empty indent compacts.
func MarshalJSON(v any, prefix, indent string) ([]byte, error) {
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

// MarshalOrdered writes the top-level object with sorted keys and each value's
// bytes verbatim, so settings terma does not own survive byte-for-byte (their
// indentation included) and the committed file diffs stably across installs.
func MarshalOrdered(m map[string]json.RawMessage) ([]byte, error) {
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

// ownedHookCommand recognizes a command terma generated, by shape: a committed entry
// (HookCommand, CodexHookCommand), or a machine-wide one (UserHookCommand,
// ManagedHookCommand), for any event. Merely mentioning "terma hook" in a user's
// script is not ownership evidence.
func ownedHookCommand(command string) bool {
	command = strings.TrimSpace(command)
	return committedHookShape.MatchString(command) || userHookShape.MatchString(command)
}

// WithoutTerma removes only owned command leaves, retaining unrelated handlers in
// the same group and their matcher/options. A nil result is an entirely owned entry.
func WithoutTerma(entry json.RawMessage) (json.RawMessage, bool, error) {
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
		remaining, removed, err := WithoutTerma(handler)
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
	encoded, err := MarshalJSON(kept, "", "")
	if err != nil {
		return nil, false, err
	}
	obj["hooks"] = encoded
	out, err := MarshalJSON(obj, "", "")
	return out, true, err
}
