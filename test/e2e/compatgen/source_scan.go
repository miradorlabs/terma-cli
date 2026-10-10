package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Reading a harness's source for source links (source.go): a build's release tarball, read
// once to a temporary file and scanned for the names the findings need.

// scanBuild finds the needles in version's source: read once, scanned, and scanned again for
// the constants that hold a quoted needle, where any do.
func scanBuild(s source, version string, needles map[needle]bool) (map[needle][]hit, error) {
	r, err := openSource(s.repo, s.tagOf(version))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := os.CreateTemp("", "compatgen-source-*.tar.gz")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return nil, err
	}
	again := func() (io.Reader, error) {
		_, err := f.Seek(0, io.SeekStart)
		return f, err
	}
	tree, err := again()
	if err != nil {
		return nil, err
	}
	hits, aliases, err := scan(tree, s, needles, nil)
	if err != nil || len(aliases) == 0 {
		return hits, err
	}
	if tree, err = again(); err != nil {
		return nil, err
	}
	used, _, err := scan(tree, s, needles, aliases)
	if err != nil {
		return nil, err
	}
	for n, hs := range used {
		hits[n] = append(hits[n], hs...)
	}
	return hits, nil
}

// needle is a name to find: as a quoted string, or as a name assigned to, the way a tracing
// macro names a field ("codex.turn.phase = ...").
type needle struct {
	text  string
	field bool
}

// hit is a line that names a needle, and the line of the function it is in (fnStart), 0 at
// a file's top level.
type hit struct {
	path string
	line int
	fn   int
}

var (
	assignedName = regexp.MustCompile(`(?:^|[\s,({])([A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*)\s*=[^=>]`)
	// constName is a constant that names a string ("const TOOL_CALL_METRIC: &str = "...""):
	// the code that records a surface often names it by the constant.
	constName = regexp.MustCompile(`\bconst\s+([A-Z][A-Z0-9_]*)\s*:\s*&(?:'static\s+)?str\s*=\s*"([^"]*)"`)
	// constHead is one whose string is on the next line.
	constHead = regexp.MustCompile(`\bconst\s+([A-Z][A-Z0-9_]*)\s*:\s*&(?:'static\s+)?str\s*=\s*$`)
	upperName = regexp.MustCompile(`\b[A-Z][A-Z0-9_]+\b`)
	fnStart   = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:const\s+)?(?:async\s+)?(?:unsafe\s+)?fn\s+\w`)
)

// scan reads a tarball of s's tree, as GitHub serves one (every path under one folder), and
// finds where each needle is named, outside tests and comments. A quoted needle a constant
// holds is also named wherever the constant is: aliases, from a first scan, say which.
func scan(r io.Reader, s source, needles map[needle]bool, aliases map[string]string) (map[needle][]hit, map[string]string, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, err
	}
	tr := tar.NewReader(zr)
	out, found := map[needle][]hit{}, map[string]string{}
	add := func(n needle, h hit) {
		if needles[n] && len(out[n]) < maxHits {
			out[n] = append(out[n], h)
		}
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, found, nil
		}
		if err != nil {
			return nil, nil, err
		}
		_, file, _ := strings.Cut(h.Name, "/")
		if h.Typeflag != tar.TypeReg || !strings.HasPrefix(file, s.dir) || !strings.HasSuffix(file, s.ext) || testFile(file) {
			continue
		}
		sc := bufio.NewScanner(tr)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		var tests testModule
		pending := "" // a constant whose string is on this line
		var lex lexState
		fn := 0
		for n := 1; sc.Scan(); n++ {
			line := code(sc.Text(), &lex)
			t := strings.TrimSpace(line)
			if tests.skip(line) || t == "" {
				continue
			}
			if fnStart.MatchString(line) {
				fn = n
			}
			if aliases != nil {
				// An import names the constant, not the surface's use.
				if strings.HasPrefix(t, "use ") || strings.HasPrefix(t, "pub use ") || strings.HasPrefix(t, "pub(crate) use ") {
					continue
				}
				for _, name := range upperName.FindAllString(line, -1) {
					if lit, ok := aliases[name]; ok && !constName.MatchString(line) {
						add(needle{lit, false}, hit{file, n, fn})
					}
				}
				continue
			}
			parts := strings.Split(line, `"`)
			for i := 1; i < len(parts)-1; i += 2 {
				add(needle{parts[i], false}, hit{file, n, fn})
			}
			if m := constName.FindStringSubmatch(line); m != nil && needles[needle{m[2], false}] {
				found[m[1]] = m[2]
			}
			if pending != "" && len(parts) > 2 && strings.HasPrefix(t, `"`) && needles[needle{parts[1], false}] {
				found[pending] = parts[1]
			}
			pending = ""
			if m := constHead.FindStringSubmatch(line); m != nil {
				pending = m[1]
			}
			if strings.Contains(line, "=") {
				for _, m := range assignedName.FindAllStringSubmatchIndex(line, -1) {
					before := strings.Fields(line[:m[2]])
					if len(before) > 0 && slices.Contains([]string{"let", "mut", "const", "static", "type"}, before[len(before)-1]) {
						continue
					}
					add(needle{line[m[2]:m[3]], true}, hit{file, n, fn})
				}
			}
		}
		if err := sc.Err(); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", file, err)
		}
	}
}

// lexState is what a line of Rust starts inside of: a block comment, or a raw string and the
// quote and hashes that end it.
type lexState struct {
	block  int // block comments open: Rust nests them
	rawEnd string
}

// code is a line's code: its strings kept, its comments dropped, "//" to the end of the line
// and "/* … */" across lines, and its raw strings (r#"…"#) emptied, since what they hold is
// text (JSON, a shell script) that names nothing. st carries what the line starts inside of
// to the next.
func code(line string, st *lexState) string {
	var b strings.Builder
	inString := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		next := byte(0)
		if i+1 < len(line) {
			next = line[i+1]
		}
		switch {
		case st.rawEnd != "":
			end := strings.Index(line[i:], st.rawEnd)
			if end < 0 {
				return b.String()
			}
			b.WriteString(`""`)
			i += end + len(st.rawEnd) - 1
			st.rawEnd = ""
		case st.block > 0:
			switch {
			case c == '*' && next == '/':
				st.block--
				i++
			case c == '/' && next == '*':
				st.block++
				i++
			}
		case !inString && c == 'r' && (next == '"' || next == '#') && rawStart(line, i):
			hashes := 0
			for i+1+hashes < len(line) && line[i+1+hashes] == '#' {
				hashes++
			}
			if i+1+hashes < len(line) && line[i+1+hashes] == '"' {
				st.rawEnd = `"` + strings.Repeat("#", hashes)
				i += 1 + hashes
			} else {
				b.WriteByte(c) // r#ident, a raw identifier
			}
		case inString:
			b.WriteByte(c)
			if c == '\\' && next != 0 {
				b.WriteByte(next)
				i++
			} else if c == '"' {
				inString = false
			}
		case c == '\'' && i+2 < len(line) && line[i+2] == '\'':
			b.WriteString("'_'") // a character, which names nothing: '"' is no string
			i += 2
		case c == '\'' && next == '\\' && i+3 < len(line) && line[i+3] == '\'':
			b.WriteString("'_'")
			i += 3
		case c == '"':
			inString = true
			b.WriteByte(c)
		case c == '/' && next == '/':
			return b.String()
		case c == '/' && next == '*':
			st.block = 1
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// rawStart reports whether the r at i starts a raw string, r"…" or a byte one, br"…": it does
// not end an identifier.
func rawStart(line string, i int) bool {
	if i > 0 && line[i-1] == 'b' {
		i--
	}
	return i == 0 || !isIdent(line[i-1])
}

func isIdent(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// testModule follows a source file's test module (#[cfg(test)] mod … { … }, or mod tests
// { … }), whose lines are no build's code: a constant a test keeps names nothing the build
// sends.
type testModule struct {
	cfg   bool // the last attribute was #[cfg(test)]
	depth int  // inside the module, its braces open
}

func (m *testModule) skip(line string) bool {
	t := strings.TrimSpace(line)
	braces := func() int {
		n := 0
		for i, part := range strings.Split(line, `"`) {
			if i%2 == 0 {
				n += strings.Count(part, "{") - strings.Count(part, "}")
			}
		}
		return n
	}
	if m.depth > 0 {
		m.depth += braces()
		return true
	}
	// The attribute on its own line, or on the module's ("#[cfg(test)] mod tests {").
	if rest, ok := strings.CutPrefix(t, "#[cfg(test)]"); ok {
		if m.cfg, t = true, strings.TrimSpace(rest); t == "" {
			return true
		}
	}
	if t == "" || strings.HasPrefix(t, "#[") {
		return m.cfg
	}
	mod := strings.HasPrefix(t, "mod ") || strings.HasPrefix(t, "pub mod ") || strings.HasPrefix(t, "pub(crate) mod ")
	test := (m.cfg || strings.HasPrefix(strings.TrimPrefix(t, "pub "), "mod tests")) && mod && strings.Contains(t, "{")
	m.cfg = false
	if test {
		m.depth = braces()
		return true
	}
	return false
}

func abs(n int) int { return max(n, -n) }

func testFile(file string) bool {
	base := path.Base(file)
	return strings.Contains(file, "/tests/") || strings.Contains(file, "/benches/") || base == "tests.rs" ||
		strings.HasSuffix(base, "_test.rs") || strings.HasSuffix(base, "_tests.rs")
}
