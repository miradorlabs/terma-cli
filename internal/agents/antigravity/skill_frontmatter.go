package antigravity

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"gopkg.in/yaml.v3"
)

// skillFileLimit bounds the read of a skill's file: its frontmatter is at the top, and a
// block that does not close within it is not read.
const skillFileLimit = 64 << 10

// antigravitySkillFrontmatterName reads the YAML block a skill's file opens with, between two
// `---` lines, for its name, only where YAML gives it a string, with whitespace runs collapsed
// to single spaces. ok with an empty name for a block YAML accepts that names none. false for
// anything but a regular file whose block closes within skillFileLimit and is a mapping YAML
// accepts, so a FIFO never blocks a hook and nothing is guessed from a block YAML refuses.
//
// Accepted risk: a skill whose frontmatter strict YAML refuses, such as an unquoted ": " in
// its description that agy repairs before loading it, is not counted as used. A hand-written
// second reader would take text from other keys' values and from the body. Upgrade path:
// quote the known keys' values and parse the result with YAML again, never a second reader.
func antigravitySkillFrontmatterName(path string) (name string, ok bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	if st, err := os.Lstat(resolved); err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	f, err := os.OpenFile(resolved, os.O_RDONLY|hookrun.EvidenceOpenFlags, 0)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	// One byte past the limit tells a file that ends there from one cut there.
	data, err := io.ReadAll(io.LimitReader(f, skillFileLimit+1))
	if err != nil {
		return "", false
	}
	block, ok := frontmatterBlock(data[:min(len(data), skillFileLimit)], len(data) > skillFileLimit)
	if !ok {
		return "", false
	}
	var doc map[string]any
	if yaml.Unmarshal(block, &doc) != nil {
		return "", false
	}
	s, _ := doc["name"].(string)
	return strings.Join(strings.Fields(s), " "), true
}

// frontmatterBlock is what lies between data's opening `---` line and the next `---` line.
// cut says the file goes on past data: a closing line at data's end may then be the start
// of a longer one, so the block is not closed there.
func frontmatterBlock(data []byte, cut bool) ([]byte, bool) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	first, rest, ok := bytes.Cut(data, []byte{'\n'})
	if !ok || string(bytes.TrimRight(first, " \t\r")) != "---" {
		return nil, false
	}
	for off := 0; off < len(rest); {
		line, _, more := bytes.Cut(rest[off:], []byte{'\n'})
		if string(bytes.TrimRight(line, " \t\r")) == "---" && (more || !cut) {
			return rest[:off], true
		}
		if !more {
			break
		}
		off += len(line) + 1
	}
	return nil, false
}
