package configschema

import (
	"regexp"
	"strings"
)

// Reading options a program registers one call at a time (omp's register({ id, type,
// values, default, ui: { description } })): each call's object literal is an option, its id its
// path. Values and defaults that name a constant are what the constant holds, where the
// modules say; an id built at run time (a template, a loop's) is no option here.

// FromRegistry is the options files register by calls of call, each module by its path.
func FromRegistry(files map[string][]byte, call string) Options {
	m := readModules(files)
	in := &interp{lookup: m.lookup}
	out := Options{}
	for i, p := range m.paths {
		g := parseFile(files[p], 0, 0, i)
		walkCalls(g, call, func(obj *tok, doc string) {
			if path, o, ok := in.registered(obj, doc); ok {
				out[path] = o
			}
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// walkCalls calls each with the object literal every call of name is given, and the JSDoc of
// the statement it is in.
func walkCalls(g *tok, name string, each func(obj *tok, doc string)) {
	for _, item := range g.items {
		doc := ""
		for i, t := range item {
			if t.doc != "" && (t.is('i', "export") || t.is('i', "const") || i == 0) {
				doc = t.doc
			}
			if t.is('i', name) && i+1 < len(item) && item[i+1].kind == '(' {
				if obj := item[i+1].arg(0); len(obj) == 1 && obj[0].kind == '{' {
					each(obj[0], doc)
					doc = ""
					continue
				}
			}
			if t.kind == '(' || t.kind == '[' || t.kind == '{' {
				walkCalls(t, name, each)
			}
		}
	}
}

// registryKinds are a registration's types as an option's.
var registryKinds = map[string]string{"enum": "string", "record": "object", "list": "array", "int": "integer"}

// registered is the option a registration's object declares.
func (in *interp) registered(obj *tok, doc string) (string, Option, bool) {
	fields := map[string][]*tok{}
	for _, it := range obj.items {
		if k, v := keyValue(it); k != "" && v != nil {
			fields[k] = v
		}
	}
	id, ok := literal(fields["id"])
	path, isStr := id.(string)
	if !ok || !isStr || path == "" || strings.Contains(path, "${") {
		return "", Option{}, false
	}
	o := Option{Description: doc}
	if t, ok := literal(fields["type"]); ok {
		if s, ok := t.(string); ok {
			o.Type = s
			if k, ok := registryKinds[s]; ok {
				o.Type = k
			}
		}
	}
	if v, ok := in.constant(fields["default"]); ok && v != nil {
		o.Default = v
	}
	if vs := fields["values"]; len(vs) > 0 {
		if len(vs) == 1 && vs[0].kind == '[' {
			o.Enum = in.values(vs[0], vs[0])
		} else if len(vs) == 1 && vs[0].kind == 'i' {
			if seq := in.lookupSeq(vs[0].text, vs[0]); len(seq) >= 1 && (seq[0].kind == '[' || seq[0].kind == '{') {
				o.Enum = in.values(seq[0], seq[0])
			}
		}
	}
	if ui := fields["ui"]; len(ui) == 1 && ui[0].kind == '{' {
		for _, it := range ui[0].items {
			if k, v := keyValue(it); k == "description" {
				if s, ok := literal(v); ok {
					if str, ok := s.(string); ok {
						o.Description = str
					}
				}
			}
		}
	}
	if d, ok := literal(fields["deprecated"]); ok && d != false && d != nil {
		o.Deprecated = true
	}
	o.Deprecated = o.Deprecated || deprecatedText(o.Description)
	return path, o, true
}

// constant is the literal an expression holds, or the constant it names holds.
func (in *interp) constant(seq []*tok) (any, bool) {
	if v, ok := literal(seq); ok {
		return v, true
	}
	if len(seq) == 1 && seq[0].kind == 'i' {
		if def := in.lookupSeq(seq[0].text, seq[0]); def != nil {
			return literal(def)
		}
	}
	return nil, false
}

// rustStructRE is a struct's declaration.
var rustFieldRE = regexp.MustCompile(`^\s*pub(?:\([^)]*\))?\s+([a-z_][a-z0-9_]*)\s*:\s*(.+?),?\s*$`)

// FromRustStruct is the options the fields of struct name in src declare: an old Codex's
// ConfigToml, each serde field its key, its doc comment its description, its type what its
// Rust type plainly is. Nested tables are not followed.
func FromRustStruct(src []byte, name string) Options {
	text := string(src)
	start := regexp.MustCompile(`(?m)^pub struct ` + regexp.QuoteMeta(name) + `\s*\{`).FindStringIndex(text)
	if start == nil {
		return nil
	}
	out := Options{}
	var doc []string
	rename := ""
	skip := false
	depth := 1
	for line := range strings.Lines(text[start[1]:]) {
		t := strings.TrimSpace(line)
		depth += strings.Count(t, "{") - strings.Count(t, "}")
		if depth <= 0 {
			break
		}
		switch {
		case strings.HasPrefix(t, "///"):
			doc = append(doc, strings.TrimSpace(strings.TrimPrefix(t, "///")))
		case strings.HasPrefix(t, "#[serde("):
			if m := regexp.MustCompile(`rename\s*=\s*"([^"]+)"`).FindStringSubmatch(t); m != nil {
				rename = m[1]
			}
			skip = skip || strings.Contains(t, "skip") && !strings.Contains(t, "skip_serializing_if")
		case strings.HasPrefix(t, "#["):
		default:
			if m := rustFieldRE.FindStringSubmatch(line); m != nil && depth == 1 && !skip {
				key := m[1]
				if rename != "" {
					key = rename
				}
				desc := strings.Join(doc, " ")
				out[key] = Option{Type: rustKind(m[2]), Description: desc, Deprecated: deprecatedText(desc)}
			}
			doc, rename, skip = nil, "", false
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// rustKind is the kind of value a Rust type plainly holds, "" for a type of its own.
func rustKind(t string) string {
	t = strings.TrimSpace(t)
	for {
		inner, ok := strings.CutPrefix(t, "Option<")
		if !ok {
			break
		}
		t = strings.TrimSuffix(inner, ">")
	}
	switch {
	case t == "bool":
		return "boolean"
	case t == "String" || t == "PathBuf" || t == "AbsolutePathBuf" || t == "&str":
		return "string"
	case t == "f32" || t == "f64":
		return "number"
	case regexp.MustCompile(`^[iu](8|16|32|64|128|size)$`).MatchString(t) || strings.HasPrefix(t, "NonZero"):
		return "integer"
	case strings.HasPrefix(t, "Vec<"):
		return "array"
	case strings.HasPrefix(t, "HashMap<") || strings.HasPrefix(t, "BTreeMap<") || strings.HasPrefix(t, "IndexMap<"):
		return "object"
	}
	return ""
}
