package sourcelink

import (
	"strings"
)

// Lexing a line of source. A lexer gives each line's code, its comments dropped and each
// string that names something kept as "…" (so a line's strings are the odd parts of it split
// at `"`), and, for FindText, the text its strings hold, each line's part of a string its
// own piece. What a line starts inside of (a block comment, a multi-line string) it carries
// to the next.

type lexer interface {
	line(s string) (code string, text []string)
}

// rustLexer reads Rust: its code as compatgen always has (code), and the text of its strings
// with the general lexer, which follows a string across lines.
type rustLexer struct {
	st   lexState
	text *lx
}

func (r *rustLexer) line(s string) (string, []string) {
	c := code(s, &r.st)
	if r.text == nil {
		return c, nil
	}
	_, text := r.text.line(s)
	return c, text
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

// testModule follows a Rust source file's test module (#[cfg(test)] mod … { … }, or mod
// tests { … }), whose lines are no build's code: a constant a test keeps names nothing the
// build sends.
type testModule struct {
	cfg   bool // the last attribute was #[cfg(test)]
	depth int  // inside the module, its braces open
}

func (m *testModule) skip(line string) bool {
	t := strings.TrimSpace(line)
	if m.depth > 0 {
		m.depth += braces(line)
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
		m.depth = braces(line)
		return true
	}
	return false
}

// braces counts the braces a line of code opens, less those it closes, outside its strings.
func braces(line string) int {
	n := 0
	for i, part := range strings.Split(line, `"`) {
		if i%2 == 0 {
			n += strings.Count(part, "{") - strings.Count(part, "}")
		}
	}
	return n
}

// The general lexer, for TypeScript and JavaScript, Python, and the text of Rust's strings.
// What a line is inside of is a stack: a string or a comment on top of code, and a template
// literal's interpolation (`${…}`) code again on top of the template, which may hold
// another.

const (
	kString   = iota // a string, which ends at close
	kTemplate        // a template literal, whose ${ starts code
	kInterp          // the code of a template literal's ${…}
	kBlock           // a block comment
)

type frame struct {
	kind  int
	close string // what ends a string: its quote, a triple quote, a raw string's quote and hashes
	raw   bool   // its backslashes are text
	multi bool   // it may span lines: a template literal, a triple-quoted string, a Rust string
	cont  bool   // a one-line string continued past the line's end by a backslash
	doc   bool   // a docstring: text, but no code
	depth int    // an interpolation's braces open; a nested block comment's comments
	piece int    // the line the string's current piece began on, -1 for a line before
	code  []byte // the current piece as written, for the line's code
	text  []byte // the current piece as it reads, for FindText
}

// lx is the general lexer. lang is "typescript", "python" or "rust"; text says whether to
// keep the text of strings.
type lx struct {
	lang  string
	text  bool
	stack []frame
	n     int  // the line being read
	last  byte // the last byte of code the line before ended with
}

func newLexer(lang string, text bool) lexer {
	if lang == "rust" {
		r := &rustLexer{}
		if text {
			r.text = &lx{lang: "rust", text: true}
		}
		return r
	}
	return &lx{lang: lang, text: text}
}

func (l *lx) top() *frame {
	if len(l.stack) == 0 {
		return nil
	}
	return &l.stack[len(l.stack)-1]
}

func (l *lx) line(s string) (string, []string) {
	l.n++
	var code []byte
	var text []string
	// The line's last byte of code that is not a space: what says a "/" starts a regular
	// expression, or a triple-quoted string a docstring.
	sig := func() byte {
		for i := len(code) - 1; i >= 0; i-- {
			if code[i] != ' ' && code[i] != '\t' {
				return code[i]
			}
		}
		return 0
	}
	if l.n == 1 && strings.HasPrefix(s, "#!") {
		return "", nil
	}
	i := 0
	for i < len(s) {
		c := s[i]
		next := byte(0)
		if i+1 < len(s) {
			next = s[i+1]
		}
		f := l.top()
		switch {
		case f != nil && f.kind == kBlock:
			switch {
			case c == '*' && next == '/':
				f.depth--
				i += 2
				if f.depth == 0 {
					l.pop()
				}
			case l.lang == "rust" && c == '/' && next == '*':
				f.depth++
				i += 2
			default:
				i++
			}

		case f != nil && (f.kind == kString || f.kind == kTemplate):
			switch {
			case !f.raw && c == '\\' && next == 0:
				f.cont = true // the string goes on past the line's end
				i++
			case !f.raw && c == '\\':
				f.code = append(f.code, c, next)
				f.text = append(f.text, unescape(next)...)
				i += 2
			case f.raw && l.lang == "python" && c == '\\' && next != 0:
				// A raw Python string keeps its backslashes, but one still escapes a quote.
				f.code = append(f.code, c, next)
				f.text = append(f.text, c, next)
				i += 2
			case f.kind == kTemplate && c == '$' && next == '{':
				code = l.piece(code, &text, f)
				l.stack = append(l.stack, frame{kind: kInterp})
				code = append(code, ' ')
				i += 2
			case strings.HasPrefix(s[i:], f.close):
				code = l.piece(code, &text, f)
				l.pop()
				i += len(f.close)
			default:
				f.code = append(f.code, c)
				f.text = append(f.text, c)
				i++
			}

		default: // code, at the top of the file or in an interpolation
			if f != nil && f.kind == kInterp {
				if c == '{' {
					f.depth++
				} else if c == '}' {
					if f.depth == 0 {
						l.pop()
						l.top().piece = l.n
						code = append(code, ' ')
						i++
						continue
					}
					f.depth--
				}
			}
			n, ok := l.open(s, i, &code, sig)
			if ok {
				i = n
				continue
			}
			if n < 0 {
				i = len(s) // a comment to the end of the line
				continue
			}
			code = append(code, c)
			i++
		}
	}
	// A one-line string not closed by the end of its line (JSX text's apostrophe, a quote
	// the lexer misread) ends there, and is no string; a multi-line one's piece is the line's.
	for f := l.top(); f != nil && f.kind == kString && !f.multi && !f.cont; f = l.top() {
		l.pop()
	}
	if f := l.top(); f != nil && (f.kind == kString || f.kind == kTemplate) {
		if l.text && len(f.text) > 0 {
			text = append(text, string(f.text))
		}
		f.code, f.text, f.piece, f.cont = f.code[:0], f.text[:0], -1, false
	}
	if b := sig(); b != 0 {
		l.last = b
	}
	return string(code), text
}

func (l *lx) pop() { l.stack = l.stack[:len(l.stack)-1] }

// piece ends a string's current piece: on code, as "…" where it began on this line and is
// no docstring (its quotes made plain, so the line splits at `"` into its strings), and as
// text.
func (l *lx) piece(code []byte, text *[]string, f *frame) []byte {
	if f.piece == l.n && !f.doc {
		code = append(code, '"')
		for _, c := range f.code {
			if c == '"' {
				c = '\''
			}
			code = append(code, c)
		}
		code = append(code, '"')
	} else if !f.doc {
		code = append(code, '"', '"')
	}
	if l.text && len(f.text) > 0 {
		*text = append(*text, string(f.text))
	}
	f.code, f.text, f.piece = f.code[:0], f.text[:0], l.n
	return code
}

// open starts what begins at s[i] in code, if anything does: a string, a comment, a regular
// expression. It returns where to go on from, ok if it consumed something, or -1 for a
// comment to the end of the line.
func (l *lx) open(s string, i int, code *[]byte, sig func() byte) (int, bool) {
	c := s[i]
	next := byte(0)
	if i+1 < len(s) {
		next = s[i+1]
	}
	str := func(at int, quote string, raw, multi bool) (int, bool) {
		l.stack = append(l.stack, frame{kind: kString, close: quote, raw: raw, multi: multi, piece: l.n})
		return at + len(quote), true
	}
	switch l.lang {
	case "python":
		switch {
		case c == '#':
			return -1, false
		case c == '"' || c == '\'':
			return l.pyString(s, i, i, code, sig)
		case strings.IndexByte("rRbBuUfF", c) >= 0 && (i == 0 || !isIdent(s[i-1])):
			j := i
			for j < len(s) && j-i < 2 && strings.IndexByte("rRbBuUfF", s[j]) >= 0 {
				j++
			}
			if j < len(s) && (s[j] == '"' || s[j] == '\'') {
				return l.pyString(s, i, j, code, sig)
			}
		}
	case "rust":
		switch {
		case c == '/' && next == '/':
			return -1, false
		case c == '/' && next == '*':
			l.stack = append(l.stack, frame{kind: kBlock, depth: 1})
			return i + 2, true
		case c == 'r' && (next == '"' || next == '#') && rawStart(s, i):
			hashes := 0
			for i+1+hashes < len(s) && s[i+1+hashes] == '#' {
				hashes++
			}
			if i+1+hashes < len(s) && s[i+1+hashes] == '"' {
				*code = append(*code, c)
				l.stack = append(l.stack, frame{kind: kString, close: `"` + strings.Repeat("#", hashes), raw: true, multi: true, piece: l.n})
				return i + 2 + hashes, true
			}
		case c == '\'' && i+2 < len(s) && s[i+2] == '\'':
			*code = append(*code, "'_'"...)
			return i + 3, true
		case c == '\'' && next == '\\' && i+3 < len(s) && s[i+3] == '\'':
			*code = append(*code, "'_'"...)
			return i + 4, true
		case c == '"':
			return str(i, `"`, false, true)
		}
	default: // typescript, javascript
		switch {
		case c == '/' && next == '/':
			return -1, false
		case c == '/' && next == '*':
			l.stack = append(l.stack, frame{kind: kBlock, depth: 1})
			return i + 2, true
		case c == '"' || c == '\'':
			return str(i, string(c), false, false)
		case c == '`':
			l.stack = append(l.stack, frame{kind: kTemplate, close: "`", multi: true, piece: l.n})
			return i + 1, true
		case c == '/' && regexAfter(*code, sig(), l.last):
			if end := regexEnd(s, i); end > 0 {
				*code = append(*code, "/_/"...)
				return end, true
			}
		}
	}
	return i, false
}

// pyString starts a Python string whose prefix begins at start and quote at q: a triple
// quote may span lines, and one that opens a statement is a docstring.
func (l *lx) pyString(s string, start, q int, code *[]byte, sig func() byte) (int, bool) {
	prefix := strings.ToLower(s[start:q])
	raw := strings.Contains(prefix, "r")
	quote := s[q : q+1]
	if strings.HasPrefix(s[q:], strings.Repeat(quote, 3)) {
		// A docstring opens its line, after a def, a class or nothing, not inside an
		// expression the line before left open or a run of strings.
		doc := sig() == 0 && strings.IndexByte("([{,=+%\"'", l.last) < 0
		l.stack = append(l.stack, frame{kind: kString, close: strings.Repeat(quote, 3), raw: raw, multi: true, doc: doc, piece: l.n})
		return q + 3, true
	}
	l.stack = append(l.stack, frame{kind: kString, close: quote, raw: raw, piece: l.n})
	return q + 1, true
}

// regexAfter reports whether a "/" after code starts a regular expression rather than
// dividing: it follows an operator, an opening bracket, a keyword, or nothing.
func regexAfter(code []byte, sig, lineBefore byte) bool {
	if sig == 0 {
		sig = lineBefore
		if sig == 0 {
			return true
		}
		if len(code) == 0 && (isIdent(sig) || sig == ')' || sig == ']') {
			return false
		}
	}
	if strings.IndexByte("(,=:[!&|?{};+-*%<>~^", sig) >= 0 {
		return true
	}
	t := strings.TrimRight(string(code), " \t")
	for _, kw := range []string{"return", "typeof", "case", "do", "else", "in", "of", "new", "delete", "void", "throw", "yield", "await", "instanceof"} {
		if strings.HasSuffix(t, kw) && (len(t) == len(kw) || !isIdent(t[len(t)-len(kw)-1])) {
			return true
		}
	}
	return false
}

// regexEnd is where the regular expression starting at s[i] ends, past its flags, or 0 if it
// does not end on the line (and so the "/" divides).
func regexEnd(s string, i int) int {
	class := false
	for j := i + 1; j < len(s); j++ {
		switch c := s[j]; {
		case c == '\\':
			j++
		case c == '[':
			class = true
		case c == ']':
			class = false
		case c == '/' && !class:
			j++
			for j < len(s) && isIdent(s[j]) {
				j++
			}
			return j
		}
	}
	return 0
}

// unescape is how an escaped character reads: a newline or tab as a space, a quote as itself.
func unescape(c byte) []byte {
	switch c {
	case 'n', 'r', 't':
		return []byte{' '}
	case '\\', '\'', '"', '`', '$', '{', '}':
		return []byte{c}
	}
	return []byte{'\\', c}
}
