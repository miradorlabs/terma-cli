package configschema

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// FromJSONSchema is the options a JSON schema of a configuration file describes (draft 7 or
// 2020-12): each property, its $ref and allOf resolved, its oneOf and anyOf a union of the
// kinds and values of their branches, a table of user-named keys (additionalProperties,
// patternProperties) as "<name>".
func FromJSONSchema(data []byte) (Options, error) {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	r := &jsonReader{root: root, stack: map[string]bool{}}
	return flatten(r.read(root)), nil
}

type jsonReader struct {
	root  map[string]any
	stack map[string]bool // the definitions being read, so a recursive one ends
	depth int             // how deep in properties the schema being read is
}

// def is the schema a local $ref names: #/definitions/X, #/$defs/X, #/components/schemas/X.
func (r *jsonReader) def(ref string) map[string]any {
	path, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return nil
	}
	var cur any = r.root
	for seg := range strings.SplitSeq(path, "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[strings.NewReplacer("~1", "/", "~0", "~").Replace(seg)]
	}
	m, _ := cur.(map[string]any)
	return m
}

func (r *jsonReader) read(s map[string]any) *node {
	n := &node{}
	if s == nil {
		return n
	}
	if ref, _ := s["$ref"].(string); ref != "" {
		if !r.stack[ref] {
			if d := r.def(ref); d != nil {
				r.stack[ref] = true
				n = r.read(d).clone()
				delete(r.stack, ref)
			}
		} else {
			n.addType("object") // a definition inside itself: say what it is, not again what it holds
		}
	}
	for _, sub := range list(s["allOf"]) {
		// The schema's own keywords say more than what it is made of: read them over it.
		base := r.read(sub)
		base.merge(n)
		n = base
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		for _, sub := range list(s[key]) {
			b := r.read(sub)
			if len(list(s[key])) > 1 {
				b.desc = "" // a branch's description is the branch's, not the option's
			}
			b.hasDef = false
			n.merge(b)
		}
	}
	switch t := s["type"].(type) {
	case string:
		n.addType(t)
	case []any:
		for _, v := range t {
			if str, ok := v.(string); ok {
				n.addType(str)
			}
		}
	}
	if e, ok := s["enum"].([]any); ok {
		n.addEnum(e...)
	}
	if c, ok := s["const"]; ok {
		n.addEnum(c)
	}
	if d, ok := s["description"].(string); ok && d != "" {
		n.desc = d
	}
	if d, ok := s["default"]; ok {
		n.def, n.hasDef = d, true
	}
	if b, _ := s["deprecated"].(bool); b {
		n.deprecated = true
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for _, k := range sortedKeys(props) {
			if p, ok := props[k].(map[string]any); ok {
				n.setProp(k, r.child(p))
			}
		}
		n.addType("object")
	}
	if ap, ok := s["additionalProperties"].(map[string]any); ok {
		n.rest = r.child(ap)
		n.addType("object")
	}
	if pp, ok := s["patternProperties"].(map[string]any); ok && n.rest == nil {
		for _, k := range sortedKeys(pp) {
			if p, ok := pp[k].(map[string]any); ok {
				n.rest = r.child(p)
				break
			}
		}
		n.addType("object")
	}
	switch items := s["items"].(type) {
	case map[string]any:
		n.items = r.child(items)
		n.addType("array")
	case []any:
		if len(items) > 0 {
			if m, ok := items[0].(map[string]any); ok {
				n.items = r.child(m)
			}
		}
		n.addType("array")
	}
	return n
}

// child reads a schema a level down. What is deeper than any path goes is read for what it is,
// not for what it holds: a large schema's every branch need not be.
func (r *jsonReader) child(s map[string]any) *node {
	r.depth++
	defer func() { r.depth-- }()
	if r.depth > MaxDepth+1 {
		shallow := map[string]any{}
		for _, k := range []string{"type", "description", "default", "enum", "const", "deprecated"} {
			if v, ok := s[k]; ok {
				shallow[k] = v
			}
		}
		return r.read(shallow)
	}
	return r.read(s)
}

func list(v any) []map[string]any {
	vs, _ := v.([]any)
	var out []map[string]any
	for _, x := range vs {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func sortedKeys(m map[string]any) []string { return slices.Sorted(maps.Keys(m)) }
