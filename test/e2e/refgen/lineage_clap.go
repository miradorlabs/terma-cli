package main

import (
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Reading a Rust command line from its source: the commands and options clap's derive makes of
// a tree's structs and enums, keyed as a snapshot keys them (cli.commands.<command>, and
// cli.commands.<command>.options.<--flag>), each with the line that defines it. A struct that
// derives Parser or Args gives its fields' options, those of the structs it flattens, and the
// subcommands of the enum its subcommand field holds; an enum that derives Subcommand gives a
// command for each variant. What a help text shows is what is kept: a hidden argument or
// command is not, --help is on every command, --version where the command says version, and an
// option marked global is on every command beneath its own. Types are found as rustc would:
// through the file's use declarations, then the crate (by its Cargo.toml), then anywhere.

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
func isRustIdent(c byte) bool      { return isRustIdentStart(c) || c >= '0' && c <= '9' }

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

// rustTree is a tree's Rust: its files' items by crate.
type rustTree struct {
	files  map[string]*rustFile
	byName map[string][]*rustItem
	crates map[string]string // crate name (as code names it) → its directory
}

var (
	cargoPackageRE = regexp.MustCompile(`(?m)^\[package\][^\[]*?^name\s*=\s*"([^"]+)"`)
	cargoLibRE     = regexp.MustCompile(`(?m)^\[lib\][^\[]*?^name\s*=\s*"([^"]+)"`)
)

// newRustTree reads files (path → source; Cargo.toml files for the crates) into a tree.
func newRustTree(files map[string][]byte) *rustTree {
	t := &rustTree{files: map[string]*rustFile{}, byName: map[string][]*rustItem{}, crates: map[string]string{}}
	for p, data := range files {
		if path.Base(p) != "Cargo.toml" {
			continue
		}
		name := ""
		if m := cargoLibRE.FindSubmatch(data); m != nil {
			name = string(m[1])
		} else if m := cargoPackageRE.FindSubmatch(data); m != nil {
			name = string(m[1])
		}
		if name != "" {
			t.crates[strings.ReplaceAll(name, "-", "_")] = path.Dir(p)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		if !strings.HasSuffix(p, ".rs") {
			continue
		}
		f := parseRustFile(p, t.crateOf(p), string(files[p]))
		t.files[p] = f
		for _, it := range f.items {
			t.byName[it.name] = append(t.byName[it.name], it)
		}
	}
	return t
}

// crateOf is the crate a file is in: the one whose directory is the nearest above it.
func (t *rustTree) crateOf(p string) string {
	best, bestDir := "", ""
	for name, dir := range t.crates {
		if strings.HasPrefix(p, dir+"/") && len(dir) > len(bestDir) {
			best, bestDir = name, dir
		}
	}
	return best
}

// resolve finds the item a type names, from a file.
func (t *rustTree) resolve(from *rustFile, typ []rustTok) *rustItem {
	it := t.resolvePath(from, typePath(typ), 0)
	for hops := 0; it != nil && it.alias != nil && hops < 4; hops++ {
		it = t.resolvePath(t.files[it.file], typePath(it.alias), 0)
	}
	return it
}

// platformAlias is, of a file's aliases named name, the one a macOS build takes (the reference
// reads its command line on macOS), or else one no cfg limits.
func platformAlias(f *rustFile, name string) *rustItem {
	var plain, any *rustItem
	for _, it := range f.items {
		if it.name != name || it.alias == nil {
			continue
		}
		if any == nil {
			any = it
		}
		cfg := ""
		for _, a := range it.attrs {
			if a.name == "cfg" {
				cfg = joinAttr(a)
			}
		}
		switch {
		case strings.Contains(cfg, `target_os="macos"`) && !strings.HasPrefix(cfg, "not"):
			return it
		case cfg == "" && plain == nil:
			plain = it
		}
	}
	if plain != nil {
		return plain
	}
	return any
}

// joinAttr is an attribute's items as one text: target_os="macos", or not(...) for a
// predicate's call.
func joinAttr(a rustAttr) string {
	var parts []string
	for _, it := range a.items {
		switch {
		case it.str:
			parts = append(parts, it.key+`="`+it.value+`"`)
		case it.value != "":
			parts = append(parts, it.key+"("+it.value+")")
		default:
			parts = append(parts, it.key)
		}
	}
	return strings.Join(parts, ",")
}

func (t *rustTree) resolvePath(from *rustFile, p []string, hops int) *rustItem {
	if len(p) == 0 || hops > 6 {
		return nil
	}
	if len(p) == 1 {
		if use, ok := from.uses[p[0]]; ok && !(len(use) == 1 && use[0] == p[0]) {
			return t.resolvePath(from, use, hops+1)
		}
		for _, it := range from.items {
			if it.name == p[0] {
				if it.alias != nil {
					return platformAlias(from, p[0])
				}
				return it
			}
		}
		return t.inCrate(from.crate, p[0], nil, hops)
	}
	name := p[len(p)-1]
	mods := p[:len(p)-1]
	switch mods[0] {
	case "crate", "self", "super":
		return t.inCrate(from.crate, name, mods[1:], hops)
	}
	if _, ok := t.crates[mods[0]]; ok {
		return t.inCrate(mods[0], name, mods[1:], hops)
	}
	if use, ok := from.uses[mods[0]]; ok {
		return t.resolvePath(from, append(slices.Clone(use), p[1:]...), hops+1)
	}
	return t.inCrate(from.crate, name, mods, hops)
}

// inCrate finds name in a crate: an item of that name (in the module mods names, where more
// than one has it), or a name its files re-export under it.
func (t *rustTree) inCrate(crate, name string, mods []string, hops int) *rustItem {
	var found []*rustItem
	for _, it := range t.byName[name] {
		if it.crate == crate {
			found = append(found, it)
		}
	}
	if len(found) > 1 && len(mods) > 0 {
		for _, it := range found {
			if strings.Contains(it.file, "/"+mods[len(mods)-1]) {
				return it
			}
		}
	}
	if len(found) > 0 {
		return found[0]
	}
	for _, p := range slices.Sorted(maps.Keys(t.files)) {
		f := t.files[p]
		if f.crate != crate {
			continue
		}
		if use, ok := f.uses[name]; ok && use[len(use)-1] != name || ok && len(use) > 1 && use[0] != "crate" {
			if it := t.resolvePath(f, use, hops+1); it != nil {
				return it
			}
		}
	}
	if all := t.byName[name]; len(all) == 1 {
		return all[0]
	}
	return nil
}

// typePath is the path a type names, its wrappers (Option, Box) taken off.
func typePath(typ []rustTok) []string {
	for {
		var p []string
		i := 0
		for i < len(typ) && (typ[i].kind == 'i' || typ[i].text == "::" || typ[i].text == "&") {
			if typ[i].kind == 'i' && typ[i].text != "mut" && typ[i].text != "dyn" {
				p = append(p, typ[i].text)
			}
			i++
		}
		if len(p) == 0 {
			return nil
		}
		last := p[len(p)-1]
		if (last == "Option" || last == "Box") && i < len(typ) && typ[i].text == "<" {
			typ = typ[i+1:]
			if n := len(typ); n > 0 && typ[n-1].text == ">" {
				typ = typ[:n-1]
			}
			continue
		}
		return p
	}
}

// clapLocation is where a command or an option is defined.
type clapLocation struct {
	file string
	line int
}

// clapOption is an option a struct gives, with whether it is global.
type clapOption struct {
	key    string
	global bool
	loc    clapLocation
}

// clapIndex is every command and option of the command line the tree's root struct is, by
// id; nil where the tree has no such root.
func (t *rustTree) clapIndex(root, bin string) map[string]clapLocation {
	var it *rustItem
	for _, cand := range t.byName[root] {
		if cand.derives("Parser") {
			it = cand
			break
		}
	}
	if it == nil {
		return nil
	}
	out := map[string]clapLocation{}
	t.command(out, bin, it, nil, nil, 0)
	return out
}

// command adds a command's options, with those inherited, and its subcommands.
func (t *rustTree) command(out map[string]clapLocation, name string, it *rustItem, inline []rustField, inherited []clapOption, depth int) {
	if depth > 8 {
		return
	}
	var opts []clapOption
	var subs []*rustItem
	version := false
	var fields []rustField
	var from *rustFile
	if it != nil {
		fields = it.fields
		from = t.files[it.file]
		version = commandSays(it.attrs, "version")
	} else {
		fields = inline
	}
	t.args(&opts, &subs, &version, fields, from, 0)
	help := clapOption{key: "--help"}
	if it != nil {
		help.loc = clapLocation{file: it.file, line: it.line}
	}
	if it == nil || !commandSays(it.attrs, "disable_help_flag") {
		opts = append(opts, help)
	}
	if version {
		opts = append(opts, clapOption{key: "--version", loc: help.loc})
	}
	all := append(slices.Clone(inherited), opts...)
	var globals []clapOption
	for _, o := range all {
		id := "cli.commands." + name + ".options." + o.key
		if _, seen := out[id]; !seen {
			out[id] = o.loc
		}
		if o.global {
			globals = append(globals, o)
		}
	}
	for _, sub := range subs {
		t.subcommands(out, name, sub, globals, depth)
	}
}

// commandSays reports whether a struct's command attributes set key (version, or a flag such
// as disable_help_flag = true).
func commandSays(attrs []rustAttr, key string) bool {
	for _, a := range attrs {
		if !a.clap() {
			continue
		}
		if v, ok := a.get(key); ok && (v == "" || v == "true" || key == "version") {
			return true
		}
	}
	return false
}

// args collects the options of fields, through the structs they flatten, and the enums their
// subcommand fields hold.
func (t *rustTree) args(opts *[]clapOption, subs *[]*rustItem, version *bool, fields []rustField, from *rustFile, depth int) {
	if depth > 6 {
		return
	}
	for _, f := range fields {
		var clapAttrs []rustAttr
		for _, a := range f.attrs {
			if a.clap() {
				clapAttrs = append(clapAttrs, a)
			}
		}
		has := func(k string) bool { return slices.ContainsFunc(clapAttrs, func(a rustAttr) bool { return a.has(k) }) }
		get := func(k string) (string, bool) {
			for _, a := range clapAttrs {
				if v, ok := a.get(k); ok {
					return v, true
				}
			}
			return "", false
		}
		if v, ok := get("hide"); ok && (v == "" || v == "true") {
			continue
		}
		switch {
		case has("flatten"):
			if from == nil {
				continue
			}
			sub := t.resolve(from, f.typ)
			// A newtype that implements Args by hand around another (struct
			// TuiSharedCliOptions(SharedCliOptions)) flattens what it wraps.
			for hops := 0; sub != nil && !sub.enum && len(sub.fields) == 0 && len(sub.tuple) == 1 && hops < 3; hops++ {
				sub = t.resolve(t.files[sub.file], sub.tuple[0])
			}
			if sub != nil && !sub.enum {
				*version = *version || commandSays(sub.attrs, "version")
				t.args(opts, subs, version, sub.fields, t.files[sub.file], depth+1)
			}
		case has("subcommand"):
			if from == nil {
				continue
			}
			if sub := t.resolve(from, f.typ); sub != nil && sub.enum {
				*subs = append(*subs, sub)
			}
		case has("long") || has("short"):
			key, line := "", f.line
			if v, ok := get("long"); ok {
				if v == "" {
					v = strings.ReplaceAll(f.name, "_", "-")
					v = strings.TrimPrefix(v, "r#")
				} else {
					// Named in its attribute: that line defines it.
					for _, a := range clapAttrs {
						for _, it := range a.items {
							if it.key == "long" {
								line = it.line
							}
						}
					}
				}
				key = "--" + v
			} else {
				v, _ := get("short")
				if v == "" && f.name != "" {
					v = f.name[:1]
				}
				key = "-" + v
			}
			g, _ := get("global")
			p := ""
			if from != nil {
				p = from.path
			}
			*opts = append(*opts, clapOption{key: key, global: has("global") && (g == "" || g == "true"), loc: clapLocation{file: p, line: line}})
		}
	}
}

// subcommands adds each command an enum's variants make under parent.
func (t *rustTree) subcommands(out map[string]clapLocation, parent string, en *rustItem, globals []clapOption, depth int) {
	from := t.files[en.file]
	for _, v := range en.variants {
		var get func(string) (string, bool)
		has := func(k string) bool {
			return slices.ContainsFunc(v.attrs, func(a rustAttr) bool { return a.clap() && a.has(k) })
		}
		get = func(k string) (string, bool) {
			for _, a := range v.attrs {
				if a.clap() {
					if x, ok := a.get(k); ok {
						return x, true
					}
				}
			}
			return "", false
		}
		if x, ok := get("hide"); ok && (x == "" || x == "true") {
			continue
		}
		if has("external_subcommand") {
			continue
		}
		if has("flatten") && len(v.tuple) == 1 {
			if sub := t.resolve(from, v.tuple[0]); sub != nil && sub.enum {
				t.subcommands(out, parent, sub, globals, depth+1)
			}
			continue
		}
		name, explicit := get("name")
		if !explicit || name == "" {
			name = kebab(v.name)
		}
		cmd := parent + " " + name
		id := "cli.commands." + cmd
		if _, seen := out[id]; !seen {
			out[id] = clapLocation{file: en.file, line: v.line}
		}
		switch {
		case len(v.tuple) == 1:
			sub := t.resolve(from, v.tuple[0])
			switch {
			case sub == nil:
				t.command(out, cmd, nil, nil, globals, depth+1)
			case sub.enum && sub.derives("Subcommand"):
				t.command(out, cmd, nil, nil, globals, depth+1)
				t.subcommands(out, cmd, sub, globals, depth+1)
			default:
				t.command(out, cmd, sub, nil, globals, depth+1)
			}
		case len(v.fields) > 0:
			// A struct variant: its fields are the command's, in the enum's file.
			holder := &rustItem{file: en.file, crate: en.crate, name: v.name, fields: v.fields, attrs: v.attrs, line: v.line}
			t.command(out, cmd, holder, nil, globals, depth+1)
		default:
			t.command(out, cmd, nil, nil, globals, depth+1)
		}
	}
}

// kebab is a variant's name as clap names its command: AppServer is app-server.
func kebab(s string) string {
	var sb strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		if unicode.IsUpper(r) {
			if i > 0 && (unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1]) || i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1])) {
				sb.WriteByte('-')
			}
			sb.WriteRune(unicode.ToLower(r))
			continue
		}
		if r == '_' {
			sb.WriteByte('-')
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
