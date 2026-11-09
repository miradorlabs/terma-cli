// Package configschema itemizes a coding-agent harness's configuration options from whatever
// form its build or source keeps them in: a JSON schema (Codex's config.schema.json, Gemini
// CLI's and Pi's settings.schema.json), a schema built in code (zod, Effect Schema, a plain
// object literal: Claude Code's bundle, OpenCode's source), a Python dict of defaults
// (Hermes's), a Rust struct (an old Codex's ConfigToml). Every form becomes the same thing:
// each option by its dotted path, with the type, description, default, values and
// deprecation the form says, so that the options of two builds compare key by key.
//
// A path is the option's keys from the top of the file, joined with dots
// ("sandbox_workspace_write.network_access"); a table of keys the user names is "<name>"
// ("mcp_servers.<name>.command"), and a list's items are "[]" on the list's key
// ("hooks.PreToolUse[].matcher"). Paths go MaxDepth keys deep.
package configschema

import (
	"slices"
	"sort"
	"strings"
)

// MaxDepth is how many keys deep a path goes.
const MaxDepth = 4

// Option is one configuration option. Type is the kinds of value it takes, joined with "|"
// in order ("boolean", "integer|string"), "" where the form does not say.
type Option struct {
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	Default     any    `json:"default,omitempty"`
	Enum        []any  `json:"enum,omitempty"`
	Deprecated  bool   `json:"deprecated,omitempty"`
}

// Options are a build's options by path.
type Options map[string]Option

// node is a schema as every form reads into: what a value is, and what is beneath it.
type node struct {
	types      []string
	desc       string
	def        any
	hasDef     bool
	enum       []any
	deprecated bool
	props      []prop
	rest       *node // the values of a table of keys the user names
	items      *node // a list's items
}

type prop struct {
	name string
	n    *node
}

func (n *node) addType(ts ...string) {
	for _, t := range ts {
		if t != "" && !slices.Contains(n.types, t) {
			n.types = append(n.types, t)
		}
	}
}

func (n *node) addEnum(vs ...any) {
	for _, v := range vs {
		if !slices.ContainsFunc(n.enum, func(e any) bool { return e == v }) {
			n.enum = append(n.enum, v)
		}
	}
}

// setProp sets a property, replacing one of the same name (a spread's key overridden).
func (n *node) setProp(name string, c *node) {
	for i, p := range n.props {
		if p.name == name {
			n.props[i].n = c
			return
		}
	}
	n.props = append(n.props, prop{name, c})
}

// merge takes what o says that n does not: its kinds and values joined, its properties added.
func (n *node) merge(o *node) {
	if o == nil {
		return
	}
	n.addType(o.types...)
	n.addEnum(o.enum...)
	if n.desc == "" {
		n.desc = o.desc
	}
	if !n.hasDef && o.hasDef {
		n.def, n.hasDef = o.def, true
	}
	n.deprecated = n.deprecated || o.deprecated
	for _, p := range o.props {
		if !slices.ContainsFunc(n.props, func(q prop) bool { return q.name == p.name }) {
			n.props = append(n.props, p)
		}
	}
	if n.rest == nil {
		n.rest = o.rest
	}
	if n.items == nil {
		n.items = o.items
	}
}

// clone is a copy of n to change without changing what else shares it (a resolved name).
func (n *node) clone() *node {
	c := *n
	c.types = slices.Clone(n.types)
	c.enum = slices.Clone(n.enum)
	c.props = slices.Clone(n.props)
	return &c
}

// kindOf is the JSON kind of a literal value.
func kindOf(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, int, int64:
		return "number"
	case nil:
		return "null"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return ""
}

// typeOf is n's kinds as an Option says them: "integer" where "number" is also said is the
// more exact.
func (n *node) typeOf() string {
	ts := slices.Clone(n.types)
	for _, v := range n.enum {
		if k := kindOf(v); k != "" && !slices.Contains(ts, k) && !(k == "number" && slices.Contains(ts, "integer")) {
			ts = append(ts, k)
		}
	}
	if slices.Contains(ts, "integer") {
		ts = slices.DeleteFunc(ts, func(t string) bool { return t == "number" })
	}
	sort.Strings(ts)
	return strings.Join(ts, "|")
}

// deprecatedText reports a description that says its option is deprecated, as TypeScript's
// @deprecated and most schemas' prose do.
func deprecatedText(desc string) bool {
	d := strings.ToLower(strings.TrimSpace(desc))
	return strings.HasPrefix(d, "@deprecated") || strings.HasPrefix(d, "[deprecated]") ||
		strings.HasPrefix(d, "deprecated") || strings.HasPrefix(d, "(deprecated)")
}

// flatten is every option beneath root, by path.
func flatten(root *node) Options {
	out := Options{}
	var walk func(n *node, path string, depth int, seen map[*node]bool)
	walk = func(n *node, path string, depth int, seen map[*node]bool) {
		if n == nil || seen[n] {
			return
		}
		seen[n] = true
		defer delete(seen, n)
		emit := func(key string, c *node) {
			if c == nil {
				return
			}
			p := key
			if path != "" {
				p = path + "." + key
			}
			out[p] = c.option()
			if depth+1 < MaxDepth {
				walk(c, p, depth+1, seen)
			}
		}
		for _, p := range n.props {
			emit(p.name, p.n)
		}
		if n.rest != nil {
			emit("<name>", n.rest)
		}
		if n.items != nil && path != "" && (len(n.items.props) > 0 || n.items.rest != nil) {
			// A list of tables: its items' keys are under the list's.
			items := path + "[]"
			for _, p := range n.items.props {
				out[items+"."+p.name] = p.n.option()
				if depth+1 < MaxDepth {
					walk(p.n, items+"."+p.name, depth+1, seen)
				}
			}
		}
	}
	walk(root, "", 0, map[*node]bool{})
	return out
}

func (n *node) option() Option {
	o := Option{Type: n.typeOf(), Description: strings.TrimSpace(n.desc), Deprecated: n.deprecated || deprecatedText(n.desc)}
	if n.hasDef && n.def != nil {
		o.Default = n.def
	}
	if len(n.enum) > 0 {
		o.Enum = slices.Clone(n.enum)
	}
	return o
}
