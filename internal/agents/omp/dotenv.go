package omp

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// envFile is omp's agent .env, edited line by line: a write replaces only the entries of
// the keys it changes, so the developer's other lines, comments and order survive byte for
// byte. It is read with the dotenv grammar omp parses it with (node:util parseEnv):
// `KEY=value`, an optional `export `, quoted values that may span lines, and an unquoted
// value ending at ` #`. Of two entries for one key the last is the one omp reads.
type envFile struct {
	path string
	// writePath is path with symlinks resolved, so a write goes through a symlinked file.
	writePath string
	lines     []string
	existed   bool
}

// envEntry is one KEY=value entry, on lines [start, end).
type envEntry struct {
	key, value string
	start, end int
}

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func loadEnvFile(path string) (*envFile, error) {
	writePath, _, err := config.ResolveWritePath(path)
	if err != nil {
		return nil, err
	}
	f := &envFile{path: path, writePath: writePath}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	f.existed = true
	text := strings.TrimSuffix(string(data), "\n")
	if text != "" {
		f.lines = strings.Split(text, "\n")
	}
	return f, nil
}

// values is what omp reads from the file: each key's last entry.
func (f *envFile) values() map[string]string {
	out := map[string]string{}
	for _, e := range parseEnvEntries(f.lines) {
		out[e.key] = e.value
	}
	return out
}

// apply sets each key in changes to its value, or removes it where the value is nil. A key's
// first entry is replaced in place and any later one dropped, so the file holds it once; a
// key not in the file is appended.
func (f *envFile) apply(changes map[string]*string) {
	byStart := map[int]envEntry{}
	for _, e := range parseEnvEntries(f.lines) {
		byStart[e.start] = e
	}
	var out []string
	written := map[string]bool{}
	for i := 0; i < len(f.lines); {
		e, ok := byStart[i]
		if !ok {
			out = append(out, f.lines[i])
			i++
			continue
		}
		value, changed := changes[e.key]
		switch {
		case !changed:
			out = append(out, f.lines[e.start:e.end]...)
		case !written[e.key] && value != nil:
			out = append(out, envLine(e.key, *value))
		}
		if changed {
			written[e.key] = true
		}
		i = e.end
	}
	for _, key := range slices.Sorted(maps.Keys(changes)) {
		if value := changes[key]; !written[key] && value != nil {
			out = append(out, envLine(key, *value))
		}
	}
	f.lines = out
}

// save writes the file private to this user, since it holds the relay's token, and removes a
// file left with nothing in it.
func (f *envFile) save() error {
	if strings.TrimSpace(strings.Join(f.lines, "")) == "" {
		if err := os.Remove(f.writePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(f.writePath), 0o700); err != nil {
		return err
	}
	return config.WriteFileAtomic(f.writePath, []byte(strings.Join(f.lines, "\n")+"\n"), harness.SettingsMode)
}

// envLine renders one entry so the dotenv grammar reads value back unchanged.
func envLine(key, value string) string {
	switch {
	case !strings.ContainsAny(value, " \t#'\"`\\\n\r$"):
		return key + "=" + value
	case !strings.ContainsAny(value, "'\n\r"):
		return key + "='" + value + "'"
	case !strings.Contains(value, "`"):
		return key + "=`" + value + "`"
	default:
		return key + `="` + strings.ReplaceAll(value, "\n", `\n`) + `"`
	}
}

// parseEnvEntries reads the entries of lines; a line that is no entry is skipped, as omp
// skips it.
func parseEnvEntries(lines []string) []envEntry {
	var out []envEntry
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(strings.TrimSuffix(lines[i], "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, rest, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !envKeyRE.MatchString(key) {
			continue
		}
		rest = strings.TrimSpace(rest)
		e := envEntry{key: key, start: i, end: i + 1}
		if rest != "" && strings.ContainsRune("\"'`", rune(rest[0])) {
			q := rest[0]
			body := rest[1:]
			end := i
			for !strings.ContainsRune(body, rune(q)) && end+1 < len(lines) {
				end++
				body += "\n" + strings.TrimSuffix(lines[end], "\r")
			}
			if j := strings.IndexByte(body, q); j >= 0 {
				e.value, e.end = body[:j], end+1
				if q == '"' {
					e.value = strings.ReplaceAll(e.value, `\n`, "\n")
				}
				out = append(out, e)
				i = end
				continue
			}
			// An unclosed quote: dotenv reads the line as it stands.
		}
		if j := strings.Index(rest, " #"); j >= 0 {
			rest = rest[:j]
		}
		e.value = strings.TrimSpace(rest)
		out = append(out, e)
	}
	return out
}
