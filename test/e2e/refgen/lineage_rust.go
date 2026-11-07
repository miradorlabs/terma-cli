package main

import (
	"slices"
	"strings"
)

// Rust source as lineage_clap.go reads it: tokens (lexRust), and of each file the items, their
// attributes, fields and variants, and its use declarations (parseRustFile). Enough of Rust
// to find what clap's derive makes a command line of, not a compiler's parse.

// rustTok is a token of Rust source.
type rustTok struct {
	kind byte // i ident, s string, c char, p punctuation, n number
	text string
	line int
}

// lexRust tokenizes src, without comments.
func lexRust(src string) []rustTok {
	var out []rustTok
	line := 1
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case strings.HasPrefix(src[i:], "/*"):
			depth := 0
			for i < len(src) {
				if strings.HasPrefix(src[i:], "/*") {
					depth++
					i += 2
					continue
				}
				if strings.HasPrefix(src[i:], "*/") {
					depth--
					i += 2
					if depth == 0 {
						break
					}
					continue
				}
				if src[i] == '\n' {
					line++
				}
				i++
			}
		case c == '"' || (c == 'b' && i+1 < len(src) && src[i+1] == '"'):
			start := line
			if c == 'b' {
				i++
			}
			i++
			var sb strings.Builder
			for i < len(src) && src[i] != '"' {
				if src[i] == '\\' && i+1 < len(src) {
					if src[i+1] == '\n' {
						line++
					}
					sb.WriteByte(src[i+1])
					i += 2
					continue
				}
				if src[i] == '\n' {
					line++
				}
				sb.WriteByte(src[i])
				i++
			}
			i++
			out = append(out, rustTok{'s', sb.String(), start})
		case (c == 'r' || c == 'b') && rawStringAt(src, i):
			start := line
			j := i + 1
			if src[i] == 'b' {
				j++
			}
			hashes := 0
			for j < len(src) && src[j] == '#' {
				hashes++
				j++
			}
			j++ // the quote
			end := strings.Index(src[j:], "\""+strings.Repeat("#", hashes))
			if end < 0 {
				end = len(src) - j
			}
			text := src[j : j+end]
			line += strings.Count(text, "\n")
			out = append(out, rustTok{'s', text, start})
			i = j + end + 1 + hashes
		case c == '\'':
			// A char literal ('x', '\n', '\u{1F600}') or a lifetime ('a).
			if i+2 < len(src) && src[i+1] == '\\' {
				end := strings.IndexByte(src[i+2:], '\'')
				if end >= 0 {
					out = append(out, rustTok{'c', src[i+1 : i+2+end], line})
					i += end + 3
					continue
				}
			}
			if i+2 < len(src) && src[i+2] == '\'' {
				out = append(out, rustTok{'c', src[i+1 : i+2], line})
				i += 3
				continue
			}
			j := i + 1
			for j < len(src) && isRustIdent(src[j]) {
				j++
			}
			out = append(out, rustTok{'p', "'", line})
			i = j
		case isRustIdentStart(c):
			j := i
			for j < len(src) && isRustIdent(src[j]) {
				j++
			}
			out = append(out, rustTok{'i', src[i:j], line})
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(src) && (isRustIdent(src[j]) || src[j] == '.' && j+1 < len(src) && src[j+1] >= '0' && src[j+1] <= '9') {
				j++
			}
			out = append(out, rustTok{'n', src[i:j], line})
			i = j
		default:
			if strings.HasPrefix(src[i:], "::") || strings.HasPrefix(src[i:], "->") || strings.HasPrefix(src[i:], "=>") {
				out = append(out, rustTok{'p', src[i : i+2], line})
				i += 2
				continue
			}
			if c >= 0x80 {
				i++ // a non-ASCII byte outside a string: nothing clap reads
				continue
			}
			out = append(out, rustTok{'p', string(c), line})
			i++
		}
	}
	return out
}

func rawStringAt(src string, i int) bool {
	j := i + 1
	if src[i] == 'b' {
		if j >= len(src) || src[j] != 'r' {
			return false
		}
		j++
	}
	if i > 0 && isRustIdent(src[i-1]) {
		return false
	}
	for j < len(src) && src[j] == '#' {
		j++
	}
	return j < len(src) && src[j] == '"'
}

func isRustIdentStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isRustIdent(c byte) bool { return isRustIdentStart(c) || c >= '0' && c <= '9' }

// rustAttr is an attribute: #[name(items)], each item a key or key = value.
type rustAttr struct {
	name  string
	items []rustAttrItem
	line  int
}

type rustAttrItem struct {
	key   string
	value string // a string literal's text, or the tokens joined; "" for a bare key
	str   bool
	line  int
}

func (a rustAttr) has(key string) bool {
	return slices.ContainsFunc(a.items, func(it rustAttrItem) bool { return it.key == key })
}

func (a rustAttr) get(key string) (string, bool) {
	for _, it := range a.items {
		if it.key == key {
			return it.value, true
		}
	}
	return "", false
}

// clapAttr reports whether a is one of clap's: arg, clap, command.
func (a rustAttr) clap() bool { return a.name == "arg" || a.name == "clap" || a.name == "command" }

type rustField struct {
	name  string
	typ   []rustTok
	attrs []rustAttr
	line  int
}

type rustVariant struct {
	name   string
	tuple  [][]rustTok // a tuple variant's types
	fields []rustField // a struct variant's fields
	attrs  []rustAttr
	line   int
}

// rustItem is a struct or enum, where it is defined.
type rustItem struct {
	file, crate string
	name        string
	enum        bool
	attrs       []rustAttr
	fields      []rustField
	tuple       [][]rustTok // a tuple struct's types
	alias       []rustTok   // a type alias's type
	variants    []rustVariant
	line        int
}

func (it *rustItem) derives(trait string) bool {
	for _, a := range it.attrs {
		if a.name != "derive" {
			continue
		}
		for _, d := range a.items {
			k := d.key
			if i := strings.LastIndex(k, "::"); i >= 0 {
				k = k[i+2:]
			}
			if k == trait {
				return true
			}
		}
	}
	return false
}

// rustFile is what a file declares: its items and the names its use declarations bring in.
type rustFile struct {
	path, crate string
	items       []*rustItem
	uses        map[string][]string // local name → path
}

// parseRustFile reads the structs, enums and use declarations of a file, at any depth of
// modules.
func parseRustFile(p, crate, src string) *rustFile {
	f := &rustFile{path: p, crate: crate, uses: map[string][]string{}}
	toks := lexRust(src)
	var attrs []rustAttr
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t.kind == 'p' && t.text == "#" && i+1 < len(toks) && toks[i+1].text == "[":
			end := matchGroup(toks, i+1)
			attrs = append(attrs, parseAttr(toks[i+2:end]))
			i = end
		case t.kind == 'p' && t.text == "#" && i+2 < len(toks) && toks[i+1].text == "!" && toks[i+2].text == "[":
			i = matchGroup(toks, i+2) // an inner attribute
		case t.kind == 'i' && t.text == "use":
			end := i + 1
			for end < len(toks) && toks[end].text != ";" {
				end++
			}
			parseUse(toks[i+1:end], nil, f.uses)
			i = end
			attrs = nil
		case t.kind == 'i' && (t.text == "struct" || t.text == "enum") && i+1 < len(toks) && toks[i+1].kind == 'i':
			it := &rustItem{file: p, crate: crate, name: toks[i+1].text, enum: t.text == "enum", attrs: attrs, line: toks[i+1].line}
			j := i + 2
			depth := 0
			for j < len(toks) {
				x := toks[j].text
				if x == "<" {
					depth++
				} else if x == ">" {
					depth--
				} else if depth <= 0 && (x == "{" || x == "(" || x == ";") {
					break
				}
				j++
			}
			if j >= len(toks) {
				return f
			}
			switch toks[j].text {
			case "{":
				end := matchGroup(toks, j)
				if it.enum {
					it.variants = parseVariants(toks[j+1 : end])
				} else {
					it.fields = parseFields(toks[j+1 : end])
				}
				i = end
			case "(":
				end := matchGroup(toks, j)
				for _, part := range splitTop(toks[j+1:end], true) {
					_, rest := leadingAttrs(part)
					if rest = skipVis(rest); len(rest) > 0 {
						it.tuple = append(it.tuple, rest)
					}
				}
				i = end
			default:
				i = j
			}
			f.items = append(f.items, it)
			attrs = nil
		case t.kind == 'i' && t.text == "type" && i+2 < len(toks) && toks[i+1].kind == 'i' && toks[i+2].text == "=":
			end := i + 3
			for end < len(toks) && toks[end].text != ";" {
				end++
			}
			f.items = append(f.items, &rustItem{file: p, crate: crate, name: toks[i+1].text, attrs: attrs, alias: toks[i+3 : end], line: toks[i+1].line})
			i = end
			attrs = nil
		case t.kind == 'i' && (t.text == "pub" || t.text == "crate" || t.text == "mod"):
			// What may come between an item's attributes and the item.
		case t.kind == 'p' && t.text == "(" && i > 0 && toks[i-1].text == "pub":
			i = matchGroup(toks, i)
		case t.kind == 'p' && (t.text == "{" || t.text == "}" || t.text == ";"):
			attrs = nil
		case t.kind == 'i':
			if t.text != "async" && t.text != "unsafe" && t.text != "const" && t.text != "extern" {
				attrs = nil
			}
		default:
			attrs = nil
		}
	}
	return f
}

// matchGroup is the index of the bracket closing the one at i.
func matchGroup(toks []rustTok, i int) int {
	depth := 0
	for j := i; j < len(toks); j++ {
		switch toks[j].text {
		case "(", "[", "{":
			if toks[j].kind == 'p' {
				depth++
			}
		case ")", "]", "}":
			if toks[j].kind == 'p' {
				depth--
				if depth == 0 {
					return j
				}
			}
		}
	}
	return len(toks) - 1
}

// splitTop splits toks at commas outside brackets (and angle brackets, where angles is set).
func splitTop(toks []rustTok, angles bool) [][]rustTok {
	var out [][]rustTok
	depth, start := 0, 0
	for j, t := range toks {
		if t.kind != 'p' {
			continue
		}
		switch t.text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
		case "<":
			if angles {
				depth++
			}
		case ">":
			if angles {
				depth--
			}
		case ",":
			if depth == 0 {
				out = append(out, toks[start:j])
				start = j + 1
			}
		}
	}
	if start < len(toks) {
		out = append(out, toks[start:])
	}
	return out
}

func parseAttr(toks []rustTok) rustAttr {
	a := rustAttr{}
	if len(toks) == 0 {
		return a
	}
	a.line = toks[0].line
	// The attribute's path: derive, arg, clap::arg.
	j := 0
	var name []string
	for j < len(toks) && (toks[j].kind == 'i' || toks[j].text == "::") {
		if toks[j].kind == 'i' {
			name = append(name, toks[j].text)
		}
		j++
	}
	if len(name) > 0 {
		a.name = name[len(name)-1]
	}
	if j >= len(toks) || toks[j].text != "(" {
		return a
	}
	end := matchGroup(toks, j)
	for _, part := range splitTop(toks[j+1:end], false) {
		if len(part) == 0 {
			continue
		}
		it := rustAttrItem{line: part[0].line}
		k := 0
		var key []string
		for k < len(part) && (part[k].kind == 'i' || part[k].text == "::") {
			key = append(key, part[k].text)
			k++
		}
		it.key = strings.Join(key, "")
		switch {
		case k < len(part) && part[k].text == "=":
			rest := part[k+1:]
			if len(rest) == 1 && (rest[0].kind == 's' || rest[0].kind == 'c') {
				it.value, it.str = rest[0].text, true
			} else {
				it.value = joinToks(rest)
			}
		case k < len(part) && part[k].text == "(":
			// long("x"), short('x'): a method call's argument.
			inner := part[k+1 : matchGroup(part, k)]
			if len(inner) == 1 && (inner[0].kind == 's' || inner[0].kind == 'c') {
				it.value, it.str = inner[0].text, true
			} else {
				it.value = joinToks(inner)
			}
		}
		a.items = append(a.items, it)
	}
	return a
}

func joinToks(toks []rustTok) string {
	var sb strings.Builder
	for _, t := range toks {
		sb.WriteString(t.text)
	}
	return sb.String()
}

// parseUse reads a use tree into names: each name it brings in, by the path it names.
func parseUse(toks []rustTok, prefix []string, names map[string][]string) {
	var p []string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t.kind == 'i' && t.text == "as" && i+1 < len(toks):
			if len(p) > 0 && toks[i+1].text != "_" {
				names[toks[i+1].text] = append(slices.Clone(prefix), p...)
			}
			return
		case t.kind == 'i' || t.text == "*":
			p = append(p, t.text)
		case t.text == "{":
			end := matchGroup(toks, i)
			for _, part := range splitTop(toks[i+1:end], false) {
				parseUse(part, append(slices.Clone(prefix), p...), names)
			}
			return
		}
	}
	if len(p) > 0 {
		last := p[len(p)-1]
		if last == "self" && len(p) > 1 {
			last = p[len(p)-2]
			p = p[:len(p)-1]
		}
		if last != "*" {
			names[last] = append(slices.Clone(prefix), p...)
		}
	}
}

// fieldStart skips a field's visibility.
func skipVis(toks []rustTok) []rustTok {
	if len(toks) > 0 && toks[0].text == "pub" {
		toks = toks[1:]
		if len(toks) > 0 && toks[0].text == "(" {
			toks = toks[matchGroup(toks, 0)+1:]
		}
	}
	return toks
}

// leadingAttrs splits the attributes off the front of toks.
func leadingAttrs(toks []rustTok) ([]rustAttr, []rustTok) {
	var attrs []rustAttr
	for len(toks) > 1 && toks[0].text == "#" && toks[1].text == "[" {
		end := matchGroup(toks, 1)
		attrs = append(attrs, parseAttr(toks[2:end]))
		toks = toks[end+1:]
	}
	return attrs, toks
}

func parseFields(toks []rustTok) []rustField {
	var out []rustField
	for _, part := range splitTop(toks, true) {
		attrs, rest := leadingAttrs(part)
		rest = skipVis(rest)
		if len(rest) < 2 || rest[0].kind != 'i' || rest[1].text != ":" {
			continue
		}
		out = append(out, rustField{name: rest[0].text, typ: rest[2:], attrs: attrs, line: rest[0].line})
	}
	return out
}

func parseVariants(toks []rustTok) []rustVariant {
	var out []rustVariant
	for _, part := range splitTop(toks, false) {
		attrs, rest := leadingAttrs(part)
		if len(rest) == 0 || rest[0].kind != 'i' {
			continue
		}
		v := rustVariant{name: rest[0].text, attrs: attrs, line: rest[0].line}
		if len(rest) > 1 {
			switch rest[1].text {
			case "(":
				end := matchGroup(rest, 1)
				v.tuple = splitTop(rest[2:end], true)
			case "{":
				end := matchGroup(rest, 1)
				v.fields = parseFields(rest[2:end])
			}
		}
		out = append(out, v)
	}
	return out
}
