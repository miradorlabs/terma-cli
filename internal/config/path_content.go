package config

import (
	"encoding/json"
	"path"
	"strings"
)

// HasExcludedPath reports whether any path-like field, including JSON in a string, is
// excluded; relative names in a shell command resolve against the working directory a
// field beside it names, else root.
func (p Policy) HasExcludedPath(value any, root string) bool {
	return p.HasExcludedPathFrom(value, root, "")
}

// HasExcludedPathFrom is HasExcludedPath with cwd, where a command ran when nothing in
// value names it. A shell command is split into words and each one checked as a path, so
// `cat secrets/app.env` is excluded like a read of the file: a word that only looks like
// one of its files withholds too.
func (p Policy) HasExcludedPathFrom(value any, root, cwd string) bool {
	var walk func(v any, isPath, isCommand bool, cwd string) bool
	walk = func(v any, isPath, isCommand bool, cwd string) bool {
		switch x := v.(type) {
		case map[string]any:
			cwd = workingDirIn(x, cwd)
			for k, val := range x {
				key := strings.ToLower(k)
				if walk(val, isPath || pathKey(key), isCommand || commandKey(key), cwd) {
					return true
				}
			}
		case []any:
			for _, val := range x {
				if walk(val, isPath, isCommand, cwd) {
					return true
				}
			}
		case []string:
			for _, val := range x {
				if walk(val, isPath, isCommand, cwd) {
					return true
				}
			}
		case string:
			if isPath && p.ExcludesPath(x, root) {
				return true
			}
			if isCommand && p.commandExcluded(x, root, cwd) {
				return true
			}
			encoded := strings.TrimSpace(x)
			if strings.HasPrefix(encoded, "{") || strings.HasPrefix(encoded, "[") {
				var decoded any
				if json.Unmarshal([]byte(encoded), &decoded) == nil {
					return walk(decoded, isPath, isCommand, cwd)
				}
			}
		}
		return false
	}
	return walk(value, false, false, cwd)
}

func pathKey(key string) bool {
	return strings.Contains(key, "path") || key == "file" || key == "files" || key == "cwd" || key == "workdir" ||
		strings.HasSuffix(key, ".cwd") || strings.HasSuffix(key, "directory")
}

// commandKey names a field holding a shell command: command, cmd, bash_command,
// full_command and the like.
func commandKey(key string) bool {
	return key == "command" || key == "cmd" || key == "commands" || key == "script" || key == "argv" ||
		strings.HasSuffix(key, "_command") || strings.HasSuffix(key, ".command")
}

// workingDirIn is the directory a map says its command ran in, else cwd.
func workingDirIn(m map[string]any, cwd string) string {
	for k, v := range m {
		switch strings.ToLower(k) {
		case "cwd", "workdir", "working_directory":
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return cwd
}

// commandExcluded reports whether a word of command names an excluded path: as written,
// resolved against cwd, or, since a command can run anywhere, by any trailing part of it.
func (p Policy) commandExcluded(command, root, cwd string) bool {
	for _, word := range shellWords(command) {
		if p.ExcludesPath(word, root) {
			return true
		}
		if absolutePath(word) {
			continue
		}
		if cwd != "" && p.ExcludesPath(path.Join(strings.ReplaceAll(cwd, "\\", "/"), word), root) {
			return true
		}
		// ExcludesPath tries every trailing part of an absolute name.
		if p.ExcludesPath("/"+word, "") {
			return true
		}
	}
	return false
}

// shellWords splits a command at blanks and shell operators, drops quotes and takes the
// value of a --flag=value word. It is no shell parser: a split too many only means more
// words are checked.
func shellWords(command string) []string {
	fields := strings.FieldsFunc(command, func(r rune) bool {
		return strings.ContainsRune(" \t\r\n;|&<>(){}`'\"$,", r)
	})
	words := make([]string, 0, len(fields))
	for _, f := range fields {
		if _, value, ok := strings.Cut(f, "="); ok {
			f = value
		}
		f = strings.TrimLeft(f, "-")
		if f != "" {
			words = append(words, f)
		}
	}
	return words
}

func absolutePath(name string) bool {
	name = strings.ReplaceAll(name, "\\", "/")
	return strings.HasPrefix(name, "/") || len(name) > 2 && name[1] == ':' && name[2] == '/'
}
