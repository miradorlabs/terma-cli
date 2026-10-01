package config

import (
	"encoding/json"
	"strings"
)

// HasExcludedPath reports whether any path-like field, including JSON in a string, is excluded.
func (p Policy) HasExcludedPath(value any, root string) bool {
	var walk func(any, bool) bool
	walk = func(v any, isPath bool) bool {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				key := strings.ToLower(k)
				if walk(val, isPath || strings.Contains(key, "path") || key == "file" || key == "files" || key == "cwd" || strings.HasSuffix(key, ".cwd") || strings.HasSuffix(key, "directory")) {
					return true
				}
			}
		case []any:
			for _, val := range x {
				if walk(val, isPath) {
					return true
				}
			}
		case []string:
			for _, val := range x {
				if isPath && p.ExcludesPath(val, root) {
					return true
				}
			}
		case string:
			if isPath && p.ExcludesPath(x, root) {
				return true
			}
			encoded := strings.TrimSpace(x)
			if strings.HasPrefix(encoded, "{") || strings.HasPrefix(encoded, "[") {
				var decoded any
				if json.Unmarshal([]byte(encoded), &decoded) == nil {
					return walk(decoded, isPath)
				}
			}
		}
		return false
	}
	return walk(value, false)
}
