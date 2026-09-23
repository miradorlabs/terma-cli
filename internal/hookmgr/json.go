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

// callsTerma reports whether a raw hook entry runs the binary: any "command" string in
// it names `terma hook`, whatever guard surrounds it. Matching the command rather than
// its exact text keeps a user's own entry in the same event untouched and recognizes
// entries written before the guard, so an install can rewrite them in place.
func callsTerma(entry json.RawMessage) bool {
	var v any
	if json.Unmarshal(entry, &v) != nil {
		return false
	}
	return anyCommandContains(v, Marker)
}

func anyCommandContains(v any, needle string) bool {
	switch t := v.(type) {
	case map[string]any:
		for key, value := range t {
			if key == "command" {
				if s, ok := value.(string); ok && strings.Contains(s, needle) {
					return true
				}
				continue
			}
			if anyCommandContains(value, needle) {
				return true
			}
		}
	case []any:
		for _, value := range t {
			if anyCommandContains(value, needle) {
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
