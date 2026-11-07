package configschema

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Reading JavaScript and TypeScript, as much of it as a schema written in code needs: its
// tokens (comments dropped, the JSDoc before a token kept on it), and the brackets they nest
// in. A group is a bracketed list, its items the token runs between its top-level commas; what
// a schema says is read from those runs (schema.go), never by evaluating anything.

// tok is a token, or a bracketed group of them.
type tok struct {
	kind  byte   // 'i' a name, 's' a string, 'n' a number, 'p' punctuation, 'r' a regular expression, '(' '[' '{' a group
	text  string // a name, a string's value, a number, punctuation
	doc   string // the JSDoc comment just before it
	items [][]*tok
	at    int  // its offset
	file  int  // the file it is in, of those a reader of modules was given
	nl    bool // a line ended before it
}

func (t *tok) is(kind byte, text string) bool { return t != nil && t.kind == kind && t.text == text }

type lexJS struct {
	src  []byte
	i    int
	doc  string
	prev *tok // the last token read at this level, which says whether "/" starts a regex
	file int
}

// punct3, punct2 are the punctuation of more than one character a schema's code uses.
var (
	punct4 = []string{">>>="}
	punct3 = []string{"...", "===", "!==", "**=", "<<=", ">>=", ">>>", "&&=", "||=", "??="}
	punct2 = []string{"=>", "?.", "??", "&&", "||", "==", "!=", "<=", ">=", "++", "--", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "**", "<<", ">>"}
)

// parseJS reads src from offset at: one group where at is a bracket (to its close), or the
// whole of it as one group's single item otherwise. It reads at most limit bytes (none: all).
func parseJS(src []byte, at, limit int) *tok { return parseFile(src, at, limit, 0) }

// parseFile is parseJS, its tokens marked as the file'th of a reader's.
func parseFile(src []byte, at, limit, file int) *tok {
	end := len(src)
	if limit > 0 && at+limit < end {
		end = at + limit
	}
	l := &lexJS{src: src[:end], i: at, file: file}
	if at < end && (src[at] == '{' || src[at] == '(' || src[at] == '[') {
		open := l.next()
		if open == nil || open.kind != 'p' {
			return nil
		}
		return l.group(open)
	}
	return l.group(&tok{kind: 'p', text: "", at: at})
}

// group reads the items of the group open starts, to its closing bracket.
func (l *lexJS) group(open *tok) *tok {
	closer := map[string]string{"(": ")", "[": "]", "{": "}", "": ""}[open.text]
	g := &tok{kind: '(', at: open.at, doc: open.doc, file: l.file}
	if open.text != "" {
		g.kind = open.text[0]
	}
	var item []*tok
	l.prev = nil
	for {
		t := l.next()
		if t == nil {
			break
		}
		if t.kind == 'p' {
			switch t.text {
			case closer:
				if closer != "" {
					g.items = append(g.items, item)
					l.prev = g
					return g
				}
			case "(", "[", "{":
				sub := l.group(t)
				item = append(item, sub)
				l.prev = sub
				continue
			case ")", "]", "}":
				// A stray closer: the text was not what this reads well. End the group here.
				g.items = append(g.items, item)
				return g
			case ",":
				g.items = append(g.items, item)
				item = nil
				l.prev = t
				continue
			}
		}
		item = append(item, t)
		l.prev = t
	}
	if len(item) > 0 || len(g.items) > 0 {
		g.items = append(g.items, item)
	}
	return g
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentPart(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

// regexAllowed reports whether a "/" after prev starts a regular expression.
func (l *lexJS) regexAllowed() bool {
	p := l.prev
	if p == nil {
		return true
	}
	switch p.kind {
	case 'i':
		switch p.text {
		case "return", "typeof", "instanceof", "in", "of", "new", "delete", "void", "throw", "case", "do", "else", "yield", "await":
			return true
		}
		return false
	case 's', 'n', 'r', '(', '[':
		return false
	case '{':
		return true // a block's end: a statement may start
	case 'p':
		return p.text != ")" && p.text != "]" && p.text != "}" && p.text != "++" && p.text != "--"
	}
	return true
}

// next is the next token, nil at the end.
func (l *lexJS) next() *tok {
	src := l.src
	nl := false
	for l.i < len(src) {
		c := src[l.i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			nl = nl || c == '\n'
			l.i++
		case c == '/' && l.i+1 < len(src) && src[l.i+1] == '/':
			for l.i < len(src) && src[l.i] != '\n' {
				l.i++
			}
		case c == '/' && l.i+1 < len(src) && src[l.i+1] == '*':
			end := bytes.Index(src[l.i+2:min(len(src), l.i+2+1<<20)], []byte("*/"))
			if end < 0 {
				l.i = len(src)
				break
			}
			if body := src[l.i+2 : l.i+2+end]; len(body) > 0 && body[0] == '*' {
				l.doc = jsDocText(string(body[1:]))
			}
			l.i += end + 4
		case c == '#' && l.i == 0 && l.i+1 < len(src) && src[l.i+1] == '!':
			for l.i < len(src) && src[l.i] != '\n' {
				l.i++
			}
		default:
			t := l.token()
			t.doc, l.doc, t.file, t.nl = l.doc, "", l.file, nl
			return t
		}
	}
	return nil
}

func (l *lexJS) token() *tok {
	src := l.src
	start := l.i
	c := src[l.i]
	switch {
	case isIdentStart(c):
		for l.i < len(src) && isIdentPart(src[l.i]) {
			l.i++
		}
		return &tok{kind: 'i', text: string(src[start:l.i]), at: start}
	case c >= '0' && c <= '9' || c == '.' && l.i+1 < len(src) && src[l.i+1] >= '0' && src[l.i+1] <= '9':
		for l.i < len(src) && (isIdentPart(src[l.i]) || src[l.i] == '.' ||
			(src[l.i] == '-' || src[l.i] == '+') && (src[l.i-1] == 'e' || src[l.i-1] == 'E') && !strings.HasPrefix(string(src[start:l.i]), "0x")) {
			l.i++
		}
		return &tok{kind: 'n', text: string(src[start:l.i]), at: start}
	case c == '"' || c == '\'':
		return &tok{kind: 's', text: l.quoted(c), at: start}
	case c == '`':
		return &tok{kind: 's', text: l.template(), at: start}
	case c == '/' && l.regexAllowed():
		l.i++
		inClass := false
		for l.i < len(src) && src[l.i] != '\n' {
			ch := src[l.i]
			if ch == '\\' {
				l.i += 2
				continue
			}
			if ch == '[' {
				inClass = true
			} else if ch == ']' {
				inClass = false
			} else if ch == '/' && !inClass {
				break
			}
			l.i++
		}
		l.i++
		for l.i < len(src) && isIdentPart(src[l.i]) {
			l.i++
		}
		return &tok{kind: 'r', text: string(src[start:min(l.i, len(src))]), at: start}
	}
	for _, set := range [][]string{punct4, punct3, punct2} {
		for _, p := range set {
			if strings.HasPrefix(string(src[l.i:min(len(src), l.i+len(p))]), p) {
				l.i += len(p)
				return &tok{kind: 'p', text: p, at: start}
			}
		}
	}
	l.i++
	return &tok{kind: 'p', text: string(c), at: start}
}

// quoted reads a string in quote q, its escapes undone.
func (l *lexJS) quoted(q byte) string {
	src := l.src
	l.i++
	var b strings.Builder
	for l.i < len(src) {
		c := src[l.i]
		switch {
		case c == q:
			l.i++
			return b.String()
		case c == '\\' && l.i+1 < len(src):
			l.i++
			l.escape(&b)
		case c == '\n':
			return b.String() // unterminated: what was misread ends at the line
		default:
			b.WriteByte(c)
			l.i++
		}
	}
	return b.String()
}

// escape reads the escape after a backslash into b.
func (l *lexJS) escape(b *strings.Builder) {
	src := l.src
	c := src[l.i]
	l.i++
	switch c {
	case 'n':
		b.WriteByte('\n')
	case 't':
		b.WriteByte('\t')
	case 'r':
		b.WriteByte('\r')
	case 'b', 'f', 'v', '0':
	case '\n':
	case 'x':
		if l.i+2 <= len(src) {
			if v, err := strconv.ParseUint(string(src[l.i:l.i+2]), 16, 8); err == nil {
				b.WriteRune(rune(v))
				l.i += 2
			}
		}
	case 'u':
		if l.i < len(src) && src[l.i] == '{' {
			end := strings.IndexByte(string(src[l.i:min(len(src), l.i+10)]), '}')
			if end > 0 {
				if v, err := strconv.ParseUint(string(src[l.i+1:l.i+end]), 16, 32); err == nil {
					b.WriteRune(rune(v))
				}
				l.i += end + 1
			}
		} else if l.i+4 <= len(src) {
			if v, err := strconv.ParseUint(string(src[l.i:l.i+4]), 16, 16); err == nil {
				r := rune(v)
				l.i += 4
				// A surrogate pair, as a bundle writes an emoji.
				if r >= 0xD800 && r < 0xDC00 && l.i+6 <= len(src) && src[l.i] == '\\' && src[l.i+1] == 'u' {
					if lo, err := strconv.ParseUint(string(src[l.i+2:l.i+6]), 16, 16); err == nil && lo >= 0xDC00 && lo < 0xE000 {
						r = (r-0xD800)<<10 + (rune(lo) - 0xDC00) + 0x10000
						l.i += 6
					}
				}
				if utf8.ValidRune(r) {
					b.WriteRune(r)
				}
			}
		}
	default:
		b.WriteByte(c)
	}
}

// template reads a template literal; what it interpolates reads as "${…}".
func (l *lexJS) template() string {
	src := l.src
	l.i++
	var b strings.Builder
	for l.i < len(src) {
		c := src[l.i]
		switch {
		case c == '`':
			l.i++
			return b.String()
		case c == '\\' && l.i+1 < len(src):
			l.i++
			l.escape(&b)
		case c == '$' && l.i+1 < len(src) && src[l.i+1] == '{':
			l.i += 2
			b.WriteString("${…}")
			// Skip the interpolation, its nested strings and braces with it.
			sub := &lexJS{src: src, i: l.i}
			depth := 0
			for {
				t := sub.next()
				if t == nil {
					break
				}
				if t.is('p', "{") {
					depth++
				} else if t.is('p', "}") {
					if depth == 0 {
						break
					}
					depth--
				}
				sub.prev = t
			}
			l.i = sub.i
		default:
			b.WriteByte(c)
			l.i++
		}
	}
	return b.String()
}

// jsDocText is a JSDoc comment's text: its leading stars gone, its lines joined, its tags
// after the first left out but @deprecated.
func jsDocText(body string) string {
	var parts []string
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(line, "*"))
		if strings.HasPrefix(line, "@") && !strings.HasPrefix(line, "@deprecated") {
			break
		}
		if line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, " ")
}
