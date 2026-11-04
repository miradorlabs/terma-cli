package configschema

import (
	"regexp"
	"strings"
)

// Reading options declared as TypeScript types: DeepSeek Harness's config catalog (its
// generated docs/config-catalog.md) pastes, for each plugin a configuration may load, the
// interface its config: block is, each field's JSDoc its description. A field's type is what
// its TypeScript type says plainly (string, number, boolean, a list, an object, a union of
// literals); a named type of the package's own says nothing here.

var (
	catalogBlockRE = regexp.MustCompile("(?s)<!-- BEGIN GENERATED config-catalog:([^ ]+) -->(.*?)<!-- END GENERATED")
	catalogFenceRE = regexp.MustCompile("(?s)```ts config-catalog\n(.*?)```")
)

// FromConfigCatalog is the options a config catalog declares: each plugin's by its package's
// name without its scope ("dsh-agent-loop.maxParallelToolCalls").
func FromConfigCatalog(md []byte) Options {
	root := &node{types: []string{"object"}}
	for _, m := range catalogBlockRE.FindAllSubmatch(md, -1) {
		pkg := string(m[1])
		if i := strings.LastIndex(pkg, "/"); i >= 0 {
			pkg = pkg[i+1:]
		}
		fence := catalogFenceRE.FindSubmatch(m[2])
		if fence == nil {
			continue
		}
		g := parseJS(fence[1], 0, 0)
		var s []*tok
		for _, it := range g.items {
			s = append(s, it...)
		}
		// The first interface is the config; the types after it are what it refers to.
		for i := 0; i+2 < len(s); i++ {
			if s[i].is('i', "interface") && s[i+1].kind == 'i' {
				j := i + 2
				for j < len(s) && s[j].kind != '{' {
					j++
				}
				if j < len(s) {
					n := tsMembers(s[j])
					n.desc = s[i].doc
					if n.desc == "" && i > 0 {
						n.desc = s[i-1].doc
					}
					root.setProp(pkg, n)
				}
				break
			}
		}
	}
	if len(root.props) == 0 {
		return nil
	}
	return flatten(root)
}

// tsMembers reads a type literal's members: `/** doc */ name?: Type`, one a line.
func tsMembers(g *tok) *node {
	n := &node{types: []string{"object"}}
	var s []*tok
	for k, it := range g.items {
		if k > 0 {
			s = append(s, &tok{kind: 'p', text: ",", nl: true})
		}
		s = append(s, it...)
	}
	starts := func(i int) bool {
		if s[i].kind != 'i' && s[i].kind != 's' || !(i == 0 || s[i].nl || s[i-1].is('p', ";") || s[i-1].is('p', ",")) {
			return false
		}
		j := i + 1
		if j < len(s) && s[j].is('p', "?") {
			j++
		}
		return j < len(s) && s[j].is('p', ":")
	}
	for i := 0; i < len(s); i++ {
		if !starts(i) {
			continue
		}
		name, doc := s[i].text, s[i].doc
		j := i + 1
		if s[j].is('p', "?") {
			j++
		}
		j++ // :
		k := j
		for k < len(s) && !starts(k) {
			k++
		}
		typ := s[j:k]
		for len(typ) > 0 && (typ[len(typ)-1].is('p', ";") || typ[len(typ)-1].is('p', ",")) {
			typ = typ[:len(typ)-1]
		}
		c := tsType(typ)
		c.desc = doc
		n.setProp(name, c)
		i = k - 1
	}
	return n
}

// tsType is what a TypeScript type says of a value.
func tsType(s []*tok) *node {
	n := &node{}
	// A union: each of its parts.
	var part []*tok
	parts := [][]*tok{}
	depth := 0
	for _, t := range s {
		switch {
		case t.is('p', "<"):
			depth++
		case t.is('p', ">"):
			depth--
		}
		if depth == 0 && t.is('p', "|") {
			parts = append(parts, part)
			part = nil
			continue
		}
		part = append(part, t)
	}
	parts = append(parts, part)
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		last := p[len(p)-1]
		switch {
		case last.kind == '[' && len(last.items) <= 1 && len(last.items[0]) == 0:
			n.addType("array")
		case p[0].kind == 's':
			n.addEnum(p[0].text)
		case p[0].kind == 'n':
			if v, ok := literal(p[:1]); ok {
				n.addEnum(v)
			}
		case p[0].kind == '{':
			n.merge(tsMembers(p[0]))
		case p[0].kind == '(' && len(p) == 1:
			n.merge(tsType(p[0].items[0]))
		case p[0].kind == 'i':
			switch p[0].text {
			case "string", "number", "boolean", "null":
				n.addType(p[0].text)
			case "true", "false":
				n.addType("boolean")
			case "Array", "ReadonlyArray", "Set":
				n.addType("array")
			case "Record", "Map", "Partial", "Dict":
				n.addType("object")
			case "Volatile", "Readonly":
				// A wrapper: what it wraps.
				var inner []*tok
				for k := 2; k < len(p)-1; k++ {
					inner = append(inner, p[k])
				}
				n.merge(tsType(inner))
			}
		}
	}
	return n
}
