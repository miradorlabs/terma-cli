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
// byte. It is read with the dotenv grammar omp parses it with (envParser): `KEY=value`, an
// optional `export `, quoted values that may span lines, and an unquoted value ending at `#`.
// Of two entries for one key the last is the one omp reads.
type envFile struct {
	path string
	// writePath is path with symlinks resolved, so a write goes through a symlinked file.
	writePath string
	symlinked bool
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
	writePath, symlinked, err := config.ResolveWritePath(path)
	if err != nil {
		return nil, err
	}
	f := &envFile{path: path, writePath: writePath, symlinked: symlinked}
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

// values is what omp reads from the file: each key's last entry, where omp keeps the name and
// the value.
func (f *envFile) values() map[string]string {
	out := readEnv(f.lines)
	maps.DeleteFunc(out, func(k, v string) bool { return !envKeyRE.MatchString(k) || strings.Contains(v, "\x00") })
	return out
}

// entries is the text of each key's entries, in order, as the developer wrote them: what a
// teardown puts back, where apply leaves the key's first entry.
func (f *envFile) entries() map[string]string {
	out := map[string]string{}
	for _, e := range parseEnvEntries(f.lines) {
		text := strings.Join(f.lines[e.start:e.end], "\n")
		if prior, ok := out[e.key]; ok {
			text = prior + "\n" + text
		}
		out[e.key] = text
	}
	return out
}

// apply sets each key in changes to its entry text, or removes it where the text is nil. A
// key's first entry is replaced in place and any later one dropped, so the file holds it
// once; a key not in the file is appended. It refuses a file whose lines it cannot map to
// omp's entries, a lone \r being a line break to omp, and an edit that would change what omp
// reads for any key but those in changes, leaving the file as it was.
func (f *envFile) apply(changes map[string]*string) error {
	if slices.ContainsFunc(f.lines, func(l string) bool { return strings.Contains(strings.TrimSuffix(l, "\r"), "\r") }) {
		return fmt.Errorf("%s has a carriage return that is not part of a line ending, which OMP reads as a line break: terma will not edit it; remove it and run terma setup again", f.path)
	}
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
			out = append(out, strings.Split(*value, "\n")...)
		}
		if changed {
			written[e.key] = true
		}
		i = e.end
	}
	for _, key := range slices.Sorted(maps.Keys(changes)) {
		if value := changes[key]; !written[key] && value != nil {
			out = append(out, strings.Split(*value, "\n")...)
		}
	}
	want := readEnv(f.lines)
	got := readEnv(out)
	for key, text := range changes {
		delete(want, key)
		if text != nil {
			want[key] = readEnv(strings.Split(*text, "\n"))[key]
		}
	}
	if !maps.Equal(got, want) {
		return fmt.Errorf("terma cannot edit %s without changing how OMP reads its other entries; check its quoting and run terma setup again", f.path)
	}
	f.lines = out
	return nil
}

// save writes the file private to this user, since it holds the relay's token, and removes a
// file left with nothing in it; a symlinked one is emptied instead, so the link still resolves.
func (f *envFile) save() error {
	if strings.TrimSpace(strings.Join(f.lines, "")) == "" && !f.symlinked {
		if err := os.Remove(f.writePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(f.writePath), 0o700); err != nil {
		return err
	}
	text := strings.Join(f.lines, "\n")
	if text != "" {
		text += "\n"
	}
	return config.WriteFileAtomic(f.writePath, []byte(text), harness.SettingsMode)
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

// journaledEntry is the entry text and value of what a journal holds for key: the key's
// entries as the developer wrote them, the last one read, or, from a journal that holds a bare
// value, that value rendered.
func journaledEntry(key, journaled string) (text, value string) {
	lines := strings.Split(journaled, "\n")
	es := parseEnvEntries(lines)
	end := 0
	for _, e := range es {
		if e.key != key || e.start != end {
			end = -1
			break
		}
		end = e.end
	}
	if len(es) > 0 && end == len(lines) {
		return journaled, es[len(es)-1].value
	}
	return envLine(key, journaled), journaled
}

// envSpace is the whitespace Bun's dotenv parser skips and trims: ASCII only, line breaks included.
const envSpace = "\t\v\f \n\r"

// envParser is a transcription of the dotenv parser of the Bun 1.4.4 runtime OMP 19.1.0 ships
// (src/dotenv/env_loader.rs, Parser), which OMP's node:util parseEnv runs: a byte scanner
// whose whitespace skips cross line breaks around a key, its separator and a value's start,
// where a backslash keeps any quote from closing a quoted value, and where a lone \r ends a
// line.
// Accepted risk: the grammar is pinned to that Bun; an OMP on another Bun can read a .env
// differently, and the upgrade path is re-transcribing it from that Bun's env_loader.rs.
type envParser struct {
	src string
	pos int
}

func (p *envParser) skipLine() {
	if i := strings.IndexAny(p.src[p.pos:], "\n\r"); i >= 0 {
		p.pos += i + 1
	} else {
		p.pos = len(p.src)
	}
}

func (p *envParser) skipSpace() {
	for p.pos < len(p.src) && strings.IndexByte(envSpace, p.src[p.pos]) >= 0 {
		p.pos++
	}
}

// key reads a key and its separator, `=` or `:` and a space, after an optional `export`, and
// returns where the key began.
func (p *envParser) key(checkExport bool) (key string, start int, ok bool) {
	if checkExport {
		p.skipSpace()
	}
	start = p.pos
	end := start
	for end < len(p.src) && isEnvKeyByte(p.src[end]) {
		end++
	}
	if end < len(p.src) && start < end {
		p.pos = end
		p.skipSpace()
		if p.pos < len(p.src) {
			if checkExport && end < p.pos && p.src[start:end] == "export" {
				if k, _, ok := p.key(false); ok {
					return k, start, true
				}
			}
			switch p.src[p.pos] {
			case '=':
				p.pos++
				return p.src[start:end], start, true
			case ':':
				if p.pos+1 < len(p.src) && strings.IndexByte(envSpace, p.src[p.pos+1]) >= 0 {
					p.pos += 2
					return p.src[start:end], start, true
				}
			}
		}
	}
	p.pos = start
	return "", start, false
}

// value reads a quoted value, or else the rest of the line up to a `#`, trimmed.
func (p *envParser) value() string {
	start := p.pos
	p.skipSpace()
	if p.pos >= len(p.src) {
		return ""
	}
	if q := p.src[p.pos]; q == '`' || q == '"' || q == '\'' {
		if v, ok := p.quoted(q); ok {
			return v
		}
	}
	end := start
	for end < len(p.src) && p.src[end] != '#' && p.src[end] != '\r' && p.src[end] != '\n' {
		end++
	}
	p.pos = end
	return strings.Trim(p.src[start:end], envSpace)
}

// quoted reads a value quoted with q up to the first quote no backslash escapes, discarding
// the rest of its line. Within double quotes \n and \r decode and any other escape stays as
// written; a line break inside reads as \n.
func (p *envParser) quoted(q byte) (string, bool) {
	start := p.pos
	for end := start + 1; end < len(p.src); end++ {
		switch p.src[end] {
		case '\\':
			end++
		case q:
			p.pos = end + 1
			p.skipLine()
			var b strings.Builder
			for i := start + 1; i < end; {
				switch c := p.src[i]; {
				case c == '\\' && q == '"':
					switch p.src[i+1] {
					case 'n':
						b.WriteByte('\n')
					case 'r':
						b.WriteByte('\r')
					default:
						b.WriteString(p.src[i : i+2])
					}
					i += 2
				case c == '\r':
					i++
					if i >= end || p.src[i] != '\n' {
						b.WriteByte('\n')
					}
				default:
					b.WriteByte(c)
					i++
				}
			}
			return b.String(), true
		}
	}
	return "", false
}

// readEnv is what Bun reads from lines: each key's last entry.
func readEnv(lines []string) map[string]string {
	out := map[string]string{}
	for _, e := range parseEnvEntries(lines) {
		out[e.key] = e.value
	}
	return out
}

// parseEnvEntries reads the entries of lines as OMP's Bun does, each spanning the lines from
// its key to the end of its value; text that is no entry is skipped, as Bun skips it. Keys
// are any Bun reads, including names OMP then drops.
func parseEnvEntries(lines []string) []envEntry {
	// The file as save writes it, ending in a line break, which a separator `:` can need.
	src := strings.Join(lines, "\n") + "\n"
	lineOf := func(pos int) int { return strings.Count(src[:pos], "\n") }
	p := envParser{src: src}
	if strings.HasPrefix(src, "\uFEFF") {
		p.pos = len("\uFEFF")
	}
	var out []envEntry
	for p.pos < len(p.src) {
		key, start, ok := p.key(true)
		if !ok {
			p.skipLine()
			continue
		}
		value := p.value()
		out = append(out, envEntry{key: key, value: value, start: lineOf(start), end: lineOf(p.pos-1) + 1})
	}
	return out
}

func isEnvKeyByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '-' || c == '.'
}
