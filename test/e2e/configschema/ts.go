package configschema

import (
	"path"
	"slices"
	"strings"
)

// Reading a schema written across TypeScript modules (OpenCode's Effect Schema config): each
// module's top-level constants, what it imports and under what name, and the namespaces it
// exports (`export * as ConfigAgentV1 from "./agent"`), so that a name the schema uses is
// what the module that uses it means by it.

// FromTSModules is the options of the schema the constant root holds in the module rootFile,
// files every module it may refer to, by path.
func FromTSModules(files map[string][]byte, rootFile, root string) Options {
	m := readModules(files)
	i := slices.Index(m.paths, rootFile)
	if i < 0 {
		return nil
	}
	expr := m.mods[i].consts[root]
	if expr == nil {
		return nil
	}
	in := &interp{lookup: m.lookup}
	n := in.schema(expr)
	if n == nil {
		return nil
	}
	return flatten(n)
}

type module struct {
	consts  map[string][]*tok
	imports map[string][2]string // a local name → its module's specifier, and the name it has there ("*" the module)
	exports map[string]string    // a namespace it exports → the specifier of the module it is
}

type modules struct {
	paths []string
	mods  []*module
}

func readModules(files map[string][]byte) *modules {
	m := &modules{}
	for _, p := range slices.Sorted(func(yield func(string) bool) {
		for p := range files {
			if !yield(p) {
				return
			}
		}
	}) {
		m.paths = append(m.paths, p)
	}
	for i, p := range m.paths {
		m.mods = append(m.mods, readModule(parseFile(files[p], 0, 0, i)))
	}
	return m
}

// readModule reads a module's statements: what it defines, imports and exports.
func readModule(g *tok) *module {
	mod := &module{consts: map[string][]*tok{}, imports: map[string][2]string{}, exports: map[string]string{}}
	var s []*tok
	for _, item := range g.items {
		s = append(s, item...)
	}
	str := func(i int) string {
		if i < len(s) && s[i].kind == 's' {
			return s[i].text
		}
		return ""
	}
	for i := 0; i < len(s); i++ {
		t := s[i]
		switch {
		case t.is('i', "import") && i+1 < len(s):
			j := i + 1
			if s[j].is('i', "type") {
				continue
			}
			from := func(k int) string {
				for ; k < len(s) && k < i+8; k++ {
					if s[k].is('i', "from") {
						return str(k + 1)
					}
				}
				return ""
			}
			switch {
			case s[j].kind == '{':
				spec := from(j + 1)
				for _, it := range s[j].items {
					if local, name := importName(it); local != "" {
						mod.imports[local] = [2]string{spec, name}
					}
				}
			case s[j].is('p', "*") && j+2 < len(s) && s[j+1].is('i', "as"):
				mod.imports[s[j+2].text] = [2]string{from(j + 3), "*"}
			case s[j].kind == 'i':
				mod.imports[s[j].text] = [2]string{from(j + 1), "default"}
			}
		case t.is('i', "export") && i+4 < len(s) && s[i+1].is('p', "*") && s[i+2].is('i', "as") && s[i+4].is('i', "from"):
			mod.exports[s[i+3].text] = str(i + 5)
		case t.is('i', "export") && i+1 < len(s) && s[i+1].kind == '{' && i+3 < len(s) && s[i+2].is('i', "from"):
			spec := str(i + 3)
			for _, it := range s[i+1].items {
				if local, name := importName(it); local != "" {
					mod.imports[local] = [2]string{spec, name}
				}
			}
		case (t.is('i', "const") || t.is('i', "let") || t.is('i', "var")) && i+1 < len(s) && s[i+1].kind == 'i':
			name := s[i+1].text
			j := i + 2
			// Past a type annotation, to the "=".
			for depth := 0; j < len(s) && !(depth == 0 && s[j].is('p', "=")); j++ {
				if s[j].is('p', "<") {
					depth++
				} else if s[j].is('p', ">") {
					depth--
				} else if s[j].is('i', "const") || s[j].is('i', "export") {
					break
				}
			}
			if j < len(s) && s[j].is('p', "=") {
				if _, seen := mod.consts[name]; !seen {
					mod.consts[name] = chainAt(s, j+1)
				}
			}
		}
	}
	return mod
}

// importName is an import list's item as the local name and the name it has in its module:
// `A`, `A as B`, `type A`.
func importName(it []*tok) (local, name string) {
	if len(it) > 0 && it[0].is('i', "type") {
		it = it[1:]
	}
	switch {
	case len(it) == 1 && it[0].kind == 'i':
		return it[0].text, it[0].text
	case len(it) == 3 && it[0].kind == 'i' && it[1].is('i', "as") && it[2].kind == 'i':
		return it[2].text, it[0].text
	}
	return "", ""
}

// chainAt is the expression at s[i]: a value and what is chained on it (.x, (…), […], a type's
// arguments), or an arrow function.
func chainAt(s []*tok, i int) []*tok {
	start := i
	if i >= len(s) {
		return nil
	}
	if s[i].is('i', "new") || s[i].is('i', "await") {
		i++
	}
	if i < len(s) && (s[i].kind == '(' || s[i].kind == 'i') && i+1 < len(s) && s[i+1].is('p', "=>") {
		// An arrow function: its body is the next value.
		return append(s[start:i+2:i+2], chainAt(s, i+2)...)
	}
	i++
	for i < len(s) {
		t := s[i]
		switch {
		case (t.is('p', ".") || t.is('p', "?.")) && i+1 < len(s) && s[i+1].kind == 'i':
			i += 2
		case t.kind == '(' || t.kind == '[':
			i++
		case t.is('p', "<"):
			// Type arguments, then a call: Schema.Class<Info>("Info")(…).
			depth, j := 0, i
			for ; j < len(s); j++ {
				if s[j].is('p', "<") {
					depth++
				} else if s[j].is('p', ">") {
					if depth--; depth == 0 {
						break
					}
				} else if s[j].is('p', ">>") {
					if depth -= 2; depth <= 0 {
						break
					}
				}
			}
			if j+1 < len(s) && s[j+1].kind == '(' {
				i = j + 1
				continue
			}
			return s[start:i]
		default:
			return s[start:i]
		}
	}
	return s[start:i]
}

// lookup is what name means in the module from is in: its constant, an import's constant in
// its module, a namespace's member.
func (m *modules) lookup(name string, from *tok, _ bool) []*tok {
	file := 0
	if from != nil {
		file = from.file
	}
	if file < 0 || file >= len(m.mods) {
		return nil
	}
	first, member, dotted := strings.Cut(name, ".")
	if !dotted {
		return m.constOf(file, name, 0)
	}
	if strings.Contains(member, ".") {
		return nil
	}
	if ns := m.namespace(file, first, 0); ns >= 0 {
		return m.constOf(ns, member, 0)
	}
	return nil
}

// constOf is the expression file's name holds, following imports and re-exports.
func (m *modules) constOf(file int, name string, hops int) []*tok {
	mod := m.mods[file]
	if e, ok := mod.consts[name]; ok {
		return e
	}
	if hops > 4 {
		return nil
	}
	if imp, ok := mod.imports[name]; ok && imp[1] != "*" {
		if f := m.resolve(file, imp[0]); f >= 0 {
			return m.constOf(f, imp[1], hops+1)
		}
	}
	return nil
}

// namespace is the module a name in file stands for: an `import * as`, an imported namespace
// another module exports, or one file itself exports.
func (m *modules) namespace(file int, name string, hops int) int {
	if hops > 4 {
		return -1
	}
	mod := m.mods[file]
	if spec, ok := mod.exports[name]; ok {
		return m.resolve(file, spec)
	}
	imp, ok := mod.imports[name]
	if !ok {
		return -1
	}
	f := m.resolve(file, imp[0])
	if f < 0 || imp[1] == "*" {
		return f
	}
	return m.namespace(f, imp[1], hops+1)
}

// resolve is the module a specifier names from file: a relative path, or a workspace
// package's (@scope/pkg/sub/path, as packages/pkg/src/sub/path.ts).
func (m *modules) resolve(file int, spec string) int {
	if spec == "" {
		return -1
	}
	var cands []string
	trimmed := strings.TrimSuffix(strings.TrimSuffix(spec, ".js"), ".ts")
	if strings.HasPrefix(spec, ".") {
		base := path.Join(path.Dir(m.paths[file]), trimmed)
		cands = []string{base + ".ts", base + ".tsx", base + "/index.ts", base}
	} else {
		parts := strings.Split(trimmed, "/")
		if strings.HasPrefix(spec, "@") && len(parts) >= 2 {
			parts = parts[1:]
		}
		pkg, rest := parts[0], strings.Join(parts[1:], "/")
		if rest == "" {
			rest = "index"
		}
		for _, p := range m.paths {
			for _, suffix := range []string{"/" + pkg + "/src/" + rest + ".ts", "/" + pkg + "/src/" + rest + "/index.ts", "/" + pkg + "/" + rest + ".ts"} {
				if strings.HasSuffix("/"+p, suffix) {
					cands = append(cands, p)
				}
			}
		}
	}
	for _, c := range cands {
		if i := slices.Index(m.paths, c); i >= 0 {
			return i
		}
	}
	return -1
}
