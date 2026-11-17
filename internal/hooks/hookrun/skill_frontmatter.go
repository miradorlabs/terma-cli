package hookrun

import (
	"bytes"
	"cmp"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// skillFileLimit bounds the read of a skill's file: its frontmatter is at the top, and a
// block that does not close within it is not read.
const skillFileLimit = 64 << 10

// SkillFrontmatter is what a skill's file says of itself, each value with its whitespace runs
// collapsed to single spaces, as agents read them.
type SkillFrontmatter struct {
	Name, Description, WhenToUse string
}

// ReadSkillFrontmatter reads the YAML block a skill's file opens with, between two `---`
// lines. A block that is not valid YAML is read line by line for its top-level scalars, the
// repair agents make for a plain value holding `: `. false for anything but a regular file
// that opens with such a block, so a FIFO never blocks a hook.
func ReadSkillFrontmatter(path string) (SkillFrontmatter, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return SkillFrontmatter{}, false
	}
	if st, err := os.Lstat(resolved); err != nil || !st.Mode().IsRegular() {
		return SkillFrontmatter{}, false
	}
	f, err := os.OpenFile(resolved, os.O_RDONLY|EvidenceOpenFlags, 0)
	if err != nil {
		return SkillFrontmatter{}, false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return SkillFrontmatter{}, false
	}
	data, err := io.ReadAll(io.LimitReader(f, skillFileLimit))
	if err != nil {
		return SkillFrontmatter{}, false
	}
	block, ok := frontmatterBlock(data)
	if !ok {
		return SkillFrontmatter{}, false
	}
	fields := map[string]string{}
	var doc map[string]any
	if yaml.Unmarshal(block, &doc) == nil {
		for k, v := range doc {
			if s, ok := v.(string); ok {
				fields[k] = s
			}
		}
	} else {
		fields = frontmatterLines(block)
	}
	collapse := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	return SkillFrontmatter{
		Name:        collapse(fields["name"]),
		Description: collapse(fields["description"]),
		WhenToUse:   collapse(cmp.Or(fields["when_to_use"], fields["when-to-use"])),
	}, true
}

// frontmatterBlock is what lies between data's opening `---` line and the next `---` line.
func frontmatterBlock(data []byte) ([]byte, bool) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	first, rest, ok := bytes.Cut(data, []byte{'\n'})
	if !ok || string(bytes.TrimRight(first, " \t\r")) != "---" {
		return nil, false
	}
	for off := 0; off < len(rest); {
		line, _, more := bytes.Cut(rest[off:], []byte{'\n'})
		if string(bytes.TrimRight(line, " \t\r")) == "---" {
			return rest[:off], true
		}
		if !more {
			break
		}
		off += len(line) + 1
	}
	return nil, false
}

// frontmatterLines reads a block YAML refused for its top-level `key: value` scalars, the
// rest of the line after the first `: ` taken whole and one pair of matching quotes removed.
// A value that runs past its line (an indented line after it, a quote left open) is left
// out: its first line alone would read as the whole value.
func frontmatterLines(block []byte) map[string]string {
	fields := map[string]string{}
	lines := strings.Split(string(block), "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.ContainsAny(key, " \t") {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" || value[0] == '|' || value[0] == '>' {
			continue // a block scalar YAML could not read
		}
		if continued(lines[i+1:]) {
			continue
		}
		if q := value[0]; q == '"' || q == '\'' {
			if len(value) < 2 || value[len(value)-1] != q {
				continue
			}
			value = value[1 : len(value)-1]
		}
		fields[key] = value
	}
	return fields
}

// continued reports whether the lines after a `key: value` carry its value on: the next
// line that is neither blank nor a comment is indented.
func continued(after []string) bool {
	for _, line := range after {
		if t := strings.TrimSpace(line); t == "" || t[0] == '#' {
			continue
		}
		return line[0] == ' ' || line[0] == '\t'
	}
	return false
}
