package configschema

import (
	"bytes"
	"fmt"
	"regexp"
)

// Reading a schema out of a minified JavaScript bundle (Claude Code's): the bundle names zod's
// builders as it likes ("o()" a string, "d({…})" an object), so they are learned from the
// bundled zod itself, each builder function by the schema class it constructs; and a name the
// schema uses is the nearest definition of it before the use, as a minifier's scopes mostly
// have it. Both are heuristics: a name reused in a nearer scope reads as that one.

// FromBundle is the options the object literal of zod schemas at src[at] declares (the "{").
func FromBundle(src []byte, at int) Options {
	if at < 0 || at >= len(src) || src[at] != '{' {
		return nil
	}
	b := &bundle{src: src, cache: map[string][]*tok{}}
	in := &interp{builders: zodBuilders(src, at), lookup: b.lookup}
	g := parseJS(src, at, 4<<20)
	if g == nil || g.kind != '{' {
		return nil
	}
	return flatten(in.object(g))
}

// zodCtorRE is a zod schema class's definition: `I2t=Xn("ZodString",`.
var zodCtorRE = regexp.MustCompile(`([A-Za-z_$][\w$]*)=[A-Za-z_$][\w$]*\("Zod(\w+)",`)

// zodKinds are what each zod class's builder makes, as builderKinds says it.
var zodKinds = map[string]string{
	"String": "string", "StringFormat": "string", "Email": "string", "URL": "string", "UUID": "string", "GUID": "string",
	"ISODateTime": "string", "ISODate": "string", "ISOTime": "string", "ISODuration": "string", "Base64": "string", "JWT": "string",
	"Number": "number", "NumberFormat": "integer", "BigInt": "integer", "Boolean": "boolean", "Null": "null",
	"Any": "any", "Unknown": "any", "Custom": "any", "Transform": "any", "Date": "string",
	"Object": "object", "Array": "array", "Tuple": "tuple", "Record": "record", "Map": "record", "Set": "array",
	"Union": "union", "DiscriminatedUnion": "union", "Intersection": "intersection",
	"Enum": "enum", "Literal": "enum", "Optional": "wrap", "ExactOptional": "wrap", "Nullable": "nullable",
	"Default": "default", "Prefault": "default", "Catch": "wrap", "Readonly": "wrap", "Lazy": "wrap",
	"Pipe": "pipe", "Preprocess": "last",
}

// zodBuilders are the builder functions of the zod copy bundled nearest before src[at], by
// the bundle's names for them: each short function among its schema classes that constructs
// one ("function o(e){return sn(I2t,e)}", I2t the ZodString class: o builds strings), by the
// first class its body names.
func zodBuilders(src []byte, at int) map[string]string {
	lo := max(0, at-16<<20)
	window := src[lo:at]
	locs := zodCtorRE.FindAllSubmatchIndex(window, -1)
	// The copy: the classes around the last ZodString class before the schema.
	anchor := -1
	for i, l := range locs {
		if string(window[l[4]:l[5]]) == "String" {
			anchor = i
		}
	}
	if anchor < 0 {
		return nil
	}
	base := locs[anchor][0]
	classes := map[string]string{} // the class's name → what its builders make
	first, last := len(window), 0
	for _, l := range locs {
		if l[0] < base-400_000 || l[0] > base+400_000 {
			continue
		}
		if kind := zodKinds[string(window[l[4]:l[5]])]; kind != "" {
			classes[string(window[l[2]:l[3]])] = kind
			first, last = min(first, l[0]), max(last, l[1])
		}
	}
	if first >= last {
		return nil
	}
	region := window[first:min(len(window), last+8000)]
	out := map[string]string{}
	for _, f := range jsFuncRE.FindAllSubmatchIndex(region, -1) {
		body := funcBody(region[f[1]:])
		if body == nil {
			continue
		}
		bestAt, kind := len(body), ""
		for class, k := range classes {
			if i := mentionAt(body, []byte(class)); i >= 0 && i < bestAt {
				bestAt, kind = i, k
			}
		}
		if kind != "" {
			name := string(region[f[2]:f[3]])
			if _, taken := out[name]; !taken {
				out[name] = kind
			}
		}
	}
	return out
}

// funcBody is the body of a short function whose parameters code starts after, nil for one
// longer than a builder is.
func funcBody(code []byte) []byte {
	open := bytes.IndexByte(code[:min(len(code), 80)], '{')
	if open < 0 {
		return nil
	}
	depth := 0
	for i := open; i < min(len(code), open+600); i++ {
		switch code[i] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return code[open : i+1]
			}
		}
	}
	return nil
}

// mentionAt is where code first names name as a whole identifier, -1 for nowhere.
func mentionAt(code, name []byte) int {
	for i := 0; ; {
		j := bytes.Index(code[i:], name)
		if j < 0 {
			return -1
		}
		at := i + j
		end := at + len(name)
		if (at == 0 || !isIdentPart(code[at-1])) && (end == len(code) || !isIdentPart(code[end])) {
			return at
		}
		i = end
	}
}

var jsFuncRE = regexp.MustCompile(`function ([A-Za-z_$][\w$]*)\(`)

type bundle struct {
	src   []byte
	cache map[string][]*tok
}

// lookupWindow bounds how far from a use its definition is looked for.
const lookupWindow = 3 << 20

// lookup is the expression a bare name is defined as nearest before from: `function N(…){…}`,
// `N=…` (a var, a let, an assignment), else the nearest after. A name from calls is defined as
// something a call can be made of: a function, an arrow, a memo of one (`N=p(()=>…)`).
func (b *bundle) lookup(name string, from *tok, call bool) []*tok {
	if !isIdentStart(name[0]) || bytes.ContainsAny([]byte(name), ".") {
		return nil
	}
	at := len(b.src) / 2
	if from != nil {
		at = from.at
	}
	window, key := lookupWindow, fmt.Sprintf("%s/%t", name, call)
	if len(name) == 1 {
		// A minifier gives its shortest names to a function's locals: one far off is another's.
		window, key = 200_000, fmt.Sprintf("%s/%t@%d", name, call, at)
	}
	if seq, ok := b.cache[key]; ok {
		return seq
	}
	def, isFn := -1, false
	lo := max(0, at-window)
	if i := lastDef(b.src[lo:at], name, call); i >= 0 {
		def, isFn = lo+i, bytes.HasPrefix(b.src[lo+i:], []byte("function"))
	} else if i := firstDef(b.src[at:min(len(b.src), at+window)], name, call); i >= 0 {
		def, isFn = at+i, bytes.HasPrefix(b.src[at+i:], []byte("function"))
	}
	var seq []*tok
	if def >= 0 {
		seq = parseExpr(b.src, def, isFn)
	}
	b.cache[key] = seq
	return seq
}

// callableRE is the start of a definition a call can be made of: an arrow's parameters, a
// function, or a call that makes one (a memo of an arrow).
var callableRE = regexp.MustCompile(`^(?:\(|async\b|function\b|[A-Za-z_$][\w$]*=>|[A-Za-z_$][\w$]*\(\(\)=>)`)

// defAt reports whether data[i:] defines name (as a callable, for call), and where its
// expression starts.
func defAt(data []byte, i int, name string, call bool) (int, bool) {
	if bytes.HasPrefix(data[i:], []byte("function "+name+"(")) {
		if i == 0 || !isIdentPart(data[i-1]) {
			return i, true
		}
		return 0, false
	}
	end := i + len(name)
	if !bytes.HasPrefix(data[i:], []byte(name)) || end+1 >= len(data) || data[end] != '=' || data[end+1] == '=' || data[end+1] == '>' {
		return 0, false
	}
	if i > 0 && (isIdentPart(data[i-1]) || data[i-1] == '.') {
		return 0, false
	}
	if call && !callableRE.Match(data[end+1:min(len(data), end+80)]) {
		return 0, false
	}
	return end + 1, true
}

func lastDef(data []byte, name string, call bool) int {
	best := -1
	for _, pat := range [][]byte{[]byte("function " + name + "("), []byte(name + "=")} {
		hi := len(data)
		for hi > 0 {
			j := bytes.LastIndex(data[:hi], pat)
			if j < 0 {
				break
			}
			if e, ok := defAt(data, j, name, call); ok {
				if bytes.HasPrefix(pat, []byte("function")) {
					e = j
				}
				if e > best {
					best = e
				}
				break
			}
			hi = j
		}
	}
	return best
}

func firstDef(data []byte, name string, call bool) int {
	best := -1
	for _, pat := range [][]byte{[]byte("function " + name + "("), []byte(name + "=")} {
		for lo := 0; lo < len(data); {
			j := bytes.Index(data[lo:], pat)
			if j < 0 {
				break
			}
			if e, ok := defAt(data, lo+j, name, call); ok {
				if bytes.HasPrefix(pat, []byte("function")) {
					e = lo + j
				}
				if best < 0 || e < best {
					best = e
				}
				break
			}
			lo += j + 1
		}
	}
	return best
}

// parseExpr reads the expression at src[at], to the "," or ";" that ends it; a function's, to
// its body's end.
func parseExpr(src []byte, at int, isFn bool) []*tok {
	l := &lexJS{src: src[:min(len(src), at+1<<20)], i: at}
	var seq []*tok
	for {
		t := l.next()
		if t == nil {
			return seq
		}
		if t.kind == 'p' {
			switch t.text {
			case "(", "[", "{":
				g := l.group(t)
				seq = append(seq, g)
				l.prev = g
				if isFn && g.kind == '{' {
					return seq
				}
				continue
			case ",", ";", ")", "]", "}":
				return seq
			}
		}
		seq = append(seq, t)
		l.prev = t
	}
}
