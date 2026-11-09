package configschema

import (
	"strconv"
	"strings"
)

// Reading a schema written in code: zod (z.object({…}), and a minified bundle's own names for
// its builders), Effect Schema (Schema.Struct, Schema.optional(…).annotate({…})), TypeBox
// (Type.Object) and plain objects of options. A value's schema is its builder's call and the
// methods chained on it; a builder is known by its name, whatever names its namespace, and a
// minified one by what builders says of it. A name the schema refers to is looked up where it
// is used (a module's imports, a bundle's nearest definition); one that cannot be is a value
// of no known type, its description still read.

// interp reads schemas written in code.
type interp struct {
	// builders are a minified bundle's names for zod's builders: "o" → "string".
	builders map[string]string
	// lookup is the expression a name holds where from uses it ("ConfigAgentV1.Info", "Xk"),
	// nil for none; call says it is called, so it is a function.
	lookup func(name string, from *tok, call bool) []*tok

	resolving map[string]bool
	depth     int
}

// builderKinds are what each builder name makes: a kind, or how to read its arguments.
var builderKinds = map[string]string{
	"string": "string", "String": "string", "email": "string", "url": "string", "uuid": "string", "NonEmptyString": "string",
	"Trimmed": "string", "NonEmptyTrimmedString": "string", "TemplateLiteral": "string", "templateLiteral": "string", "Char": "string",
	"number": "number", "Number": "number", "Finite": "number", "NumberFromString": "number", "float": "number", "Float": "number",
	"int": "integer", "Int": "integer", "Integer": "integer", "NonNegativeInt": "integer", "PositiveInt": "integer", "natural": "integer",
	"NonNegative": "number", "Positive": "number", "bigint": "integer", "BigInt": "integer", "int32": "integer", "uint32": "integer",
	"NumberFormat": "integer",
	"boolean":      "boolean", "Boolean": "boolean", "BooleanFromString": "boolean", "stringbool": "boolean",
	"null": "null", "Null": "null", "undefined": "", "Undefined": "", "void": "", "Void": "",
	"any": "any", "Any": "any", "unknown": "any", "Unknown": "any", "Json": "any", "custom": "any", "Custom": "any", "never": "",
	"date": "string", "Date": "string", "DateFromString": "string", "Uint8Array": "string", "instanceof": "any",
	"object": "object", "strictObject": "object", "looseObject": "object", "Struct": "object", "Object": "object", "Class": "object",
	"TaggedStruct": "object", "Interface": "object", "ReadonlyStruct": "object", "StructWithRest": "rest-object",
	"array": "array", "Array": "array", "NonEmptyArray": "array", "ReadonlyArray": "array", "set": "array", "Set": "array", "tuple": "tuple", "Tuple": "tuple",
	"record": "record", "Record": "record", "dict": "record", "map": "record", "Map": "record", "partialRecord": "record", "ReadonlyRecord": "record",
	"enum": "enum", "Enum": "enum", "Enums": "enum", "nativeEnum": "enum", "Literal": "enum", "literal": "enum", "Literals": "enum", "const": "enum", "Const": "enum",
	"union": "union", "Union": "union", "discriminatedUnion": "union", "xor": "union", "or": "union", "Or": "union",
	"intersection": "intersection", "Intersect": "intersection", "and": "intersection", "extend": "intersection",
	"optional": "wrap", "Optional": "wrap", "exactOptional": "wrap", "UndefinedOr": "wrap", "partial": "wrap", "Partial": "wrap",
	"mutable": "wrap", "readonly": "wrap", "Readonly": "wrap", "Lazy": "wrap", "lazy": "wrap", "suspend": "wrap", "brand": "wrap",
	"optionalWith": "wrap", "withDecodingDefault": "wrap", "withConstructorDefault": "wrap", "Redacted": "wrap", "Volatile": "wrap",
	"NullOr": "nullable", "nullable": "nullable", "Nullable": "nullable", "NullishOr": "nullable", "nullish": "nullable",
	"preprocess": "last", "pipe": "pipe", "transform": "last", "Transform": "last", "Preprocess": "last", "Pipe": "pipe", "decodeTo": "first",
	"default": "default", "Default": "default", "Prefault": "default", "prefault": "default", "Catch": "wrap",
}

// schema is the node an expression's schema reads as, nil where it is no schema it can read.
func (in *interp) schema(seq []*tok) *node {
	if len(seq) == 0 || in.depth > 64 {
		return nil
	}
	in.depth++
	defer func() { in.depth-- }()
	// () => schema, as a thunk or a lazy schema's body.
	if len(seq) >= 2 && seq[0].kind == '(' && seq[1].is('p', "=>") {
		seq = seq[2:]
		if len(seq) == 1 && seq[0].kind == '{' {
			return in.returned(seq[0])
		}
	}
	if len(seq) >= 2 && seq[0].kind == 'i' && seq[1].is('p', "=>") {
		seq = seq[2:]
	}
	// a && {…}, a ? b : c: the last operand is what a spread or a choice yields.
	for i := len(seq) - 1; i > 0; i-- {
		if seq[i].is('p', "&&") || seq[i].is('p', "||") || seq[i].is('p', "??") || seq[i].is('p', ":") {
			return in.schema(seq[i+1:])
		}
	}
	head := seq[0]
	var n *node
	i := 1
	switch head.kind {
	case '{':
		n = in.object(head)
	case '(':
		if len(head.items) == 1 {
			n = in.schema(head.items[0]) // a parenthesized expression
		}
	case 's':
		n = &node{}
		n.addEnum(head.text)
	case 'n':
		n = &node{}
		if v, ok := literal(seq[:1]); ok {
			n.addEnum(v)
		}
	case 'i':
		if head.text == "function" || head.text == "async" {
			for _, t := range seq {
				if t.kind == '{' {
					return in.returned(t)
				}
			}
			return nil
		}
		name := head.text
		for i+1 < len(seq) && (seq[i].is('p', ".") || seq[i].is('p', "?.")) && seq[i+1].kind == 'i' {
			if _, builder := builderKinds[name[strings.LastIndex(name, ".")+1:]]; builder && strings.Contains(name, ".") {
				break // Schema.String.pipe(…): a method of the builder's value
			}
			name += "." + seq[i+1].text
			i += 2
		}
		var args *tok
		if i < len(seq) && seq[i].kind == '(' {
			args = seq[i]
			i++
		}
		n = in.call(name, args, head)
	default:
		return nil
	}
	if n == nil {
		return nil
	}
	// Its methods: .optional(), .describe("…"), .default(v), .annotate({…})…
	for i+1 < len(seq) && (seq[i].is('p', ".") || seq[i].is('p', "?.")) && seq[i+1].kind == 'i' {
		method := seq[i+1].text
		i += 2
		var args *tok
		if i < len(seq) && seq[i].kind == '(' {
			args = seq[i]
			i++
		}
		n = in.method(n, method, args)
	}
	return n
}

// returned is the schema a function body returns.
func (in *interp) returned(body *tok) *node {
	for _, item := range body.items {
		for j, t := range item {
			if t.is('i', "return") {
				return in.schema(item[j+1:])
			}
		}
	}
	return nil
}

func (g *tok) arg(i int) []*tok {
	if g == nil || i >= len(g.items) {
		return nil
	}
	return g.items[i]
}

func (g *tok) nargs() int {
	if g == nil {
		return 0
	}
	n := 0
	for _, it := range g.items {
		if len(it) > 0 {
			n++
		}
	}
	return n
}

// call reads a builder's call (args nil: a name used as a schema, Schema.String).
func (in *interp) call(name string, args, from *tok) *node {
	last := name
	if i := strings.LastIndex(name, "."); i >= 0 {
		last = name[i+1:]
	}
	kind, known := builderKinds[last]
	if b, ok := in.builders[name]; ok && !strings.Contains(name, ".") {
		kind, known = b, true
	} else if !strings.Contains(name, ".") && in.builders != nil {
		known = false // a bare minified name is a builder only where the bundle says it is
	}
	if !known {
		// A memo or a lazy schema (p(() => schema)): what its arrow returns, whatever p is.
		if args.nargs() == 1 && len(args.arg(0)) >= 2 && args.arg(0)[0].kind == '(' && args.arg(0)[1].is('p', "=>") {
			if n := in.schema(args.arg(0)); n != nil && (len(n.types) > 0 || len(n.props) > 0) {
				return n
			}
		}
		if n := in.resolve(name, from, args != nil); n != nil {
			if args == nil || len(n.types) > 0 || len(n.props) > 0 {
				return n.clone()
			}
		}
		// An unknown call around a schema (a helper, a memo, a lazy()) is that schema.
		if args != nil {
			for _, a := range args.items {
				if n := in.schema(a); n != nil && (len(n.types) > 0 || len(n.props) > 0 || len(n.enum) > 0) {
					return n
				}
			}
		}
		return &node{}
	}
	n := &node{}
	switch kind {
	case "object":
		if obj := firstGroup(args, '{'); obj != nil {
			n = in.object(obj)
		} else if args != nil {
			// Schema.Class<…>("Name")({…}): the fields are in the next call, which a method reads.
			if s := in.schema(args.arg(0)); s != nil && len(s.props) > 0 {
				n = s
			}
		}
		n.addType("object")
	case "rest-object":
		if s := in.schema(args.arg(0)); s != nil {
			n = s
		}
		if rest := args.arg(1); len(rest) == 1 && rest[0].kind == '[' {
			if r := in.schema(rest[0].arg(0)); r != nil {
				n.rest = r.rest
				if n.rest == nil {
					n.rest = r
				}
			}
		}
		n.addType("object")
	case "array":
		if args.nargs() > 0 {
			n.items = in.schema(args.arg(0))
		}
		n.addType("array")
	case "tuple":
		n.addType("array")
	case "record":
		v := args.arg(0)
		if args.nargs() >= 2 {
			v = args.arg(1)
		}
		n.rest = in.schema(v)
		n.addType("object")
	case "enum":
		for _, a := range args.items {
			if len(a) == 1 && (a[0].kind == '[' || a[0].kind == '{') {
				n.addEnum(in.values(a[0], from)...)
			} else if v, ok := literal(a); ok {
				n.addEnum(v)
			} else if len(a) == 1 && a[0].kind == 'i' {
				// z.enum(MODES): the values a constant holds.
				if seq := in.lookupSeq(a[0].text, a[0]); len(seq) == 1 && (seq[0].kind == '[' || seq[0].kind == '{') {
					n.addEnum(in.values(seq[0], seq[0])...)
				}
			}
		}
	case "union":
		for _, a := range args.items {
			if len(a) == 1 && a[0].kind == '[' {
				for _, it := range a[0].items {
					n.unionWith(in.schema(it))
				}
			} else if len(a) > 0 && a[0].kind != 's' {
				n.unionWith(in.schema(a))
			}
		}
	case "intersection":
		for _, a := range args.items {
			if s := in.schema(a); s != nil {
				n.merge(s)
			}
		}
	case "wrap", "first":
		if s := in.schema(args.arg(0)); s != nil {
			n = s
		}
	case "nullable":
		if s := in.schema(args.arg(0)); s != nil {
			n = s
		}
		n.addType("null")
	case "last":
		// preprocess(fn, schema): the schema is the last argument.
		if args != nil {
			if s := in.schema(args.arg(len(args.items) - 1)); s != nil {
				n = s
			}
		}
	case "pipe":
		if s := in.schema(args.arg(0)); s != nil {
			n = s
		}
	case "default":
		if s := in.schema(args.arg(0)); s != nil {
			n = s
		}
		if v, ok := literal(args.arg(1)); ok {
			n.def, n.hasDef = v, true
		}
	case "any", "string", "number", "integer", "boolean", "null":
		n.addType(kind)
		if kind == "any" {
			n.types = nil
		}
	}
	return n
}

// unionWith joins a branch of a union into n.
func (n *node) unionWith(b *node) {
	if b == nil {
		return
	}
	b = b.clone()
	b.desc, b.hasDef = "", false
	n.merge(b)
}

// method reads a method chained on schema n.
func (in *interp) method(n *node, name string, args *tok) *node {
	switch name {
	case "describe", "description":
		if v, ok := literal(args.arg(0)); ok {
			if s, ok := v.(string); ok {
				n = n.clone()
				n.desc = s
			}
		}
	case "meta", "annotate", "annotations", "openapi":
		if obj := firstGroup(args, '{'); obj != nil {
			n = n.clone()
			for _, it := range obj.items {
				key, val := keyValue(it)
				switch key {
				case "description", "title":
					if v, ok := literal(val); ok && (key == "description" || n.desc == "") {
						if s, ok := v.(string); ok {
							n.desc = s
						}
					}
				case "default":
					if v, ok := literal(val); ok {
						n.def, n.hasDef = v, true
					}
				case "deprecated":
					if v, ok := literal(val); ok && v != false {
						n.deprecated = true
					}
				}
			}
		}
	case "default", "prefault", "catch":
		if v, ok := literal(args.arg(0)); ok && name != "catch" {
			n = n.clone()
			n.def, n.hasDef = v, true
		}
	case "deprecated":
		n = n.clone()
		n.deprecated = true
	case "nullable", "nullish":
		n = n.clone()
		n.addType("null")
	case "int", "safe", "step":
		n = n.clone()
		if len(n.types) == 0 || len(n.types) == 1 && n.types[0] == "number" {
			n.types = []string{"integer"}
		}
	case "array":
		n = &node{items: n, types: []string{"array"}}
	case "or":
		n = n.clone()
		n.unionWith(in.schema(args.arg(0)))
	case "and", "merge", "extend", "safeExtend":
		n = n.clone()
		if obj := firstGroup(args, '{'); obj != nil {
			n.merge(in.object(obj))
		} else {
			n.merge(in.schema(args.arg(0)))
		}
	case "pipe":
		// zod's .pipe(z.transform…) keeps the schema; Effect's .pipe(Schema.optional) too.
	case "pick", "omit", "partial", "required", "strict", "passthrough", "strip", "catchall", "optional", "readonly", "brand":
	}
	return n
}

// object reads an object literal: each key's schema, a spread's keys with them.
func (in *interp) object(obj *tok) *node {
	n := &node{types: []string{"object"}}
	for _, it := range obj.items {
		if len(it) == 0 {
			continue
		}
		if it[0].is('p', "...") {
			s := in.schema(it[1:])
			if s == nil && len(it) > 1 {
				continue
			}
			if s != nil {
				for _, p := range s.props {
					n.setProp(p.name, p.n)
				}
			}
			continue
		}
		key, val := keyValue(it)
		if key == "" {
			continue
		}
		// A feature's gated settings (Claude Code's `voice: {buildGate: …, shape: () => ({…})}`):
		// the keys its shape declares are the object's own, there when the feature is.
		if len(val) == 1 && val[0].kind == '{' {
			if shape := gatedShape(val[0]); shape != nil {
				if s := in.schema(shape); s != nil {
					for _, p := range s.props {
						n.setProp(p.name, p.n)
					}
				}
				continue
			}
		}
		var s *node
		if val == nil {
			s = in.schema(it[:1]) // shorthand: { name }
		} else {
			s = in.schema(val)
		}
		if s == nil {
			s = &node{}
		}
		if s.desc == "" && it[0].doc != "" {
			s = s.clone()
			s.desc = it[0].doc
		}
		n.setProp(key, s)
	}
	return n
}

// gatedShape is the shape a gated group of settings declares, nil for an object that is no
// such group: one with a buildGate and a shape.
func gatedShape(obj *tok) []*tok {
	var shape []*tok
	gate := false
	for _, it := range obj.items {
		switch k, v := keyValue(it); k {
		case "shape":
			shape = v
		case "buildGate":
			gate = true
		}
	}
	if !gate {
		return nil
	}
	// () => ({…}): the parenthesized object.
	if len(shape) == 3 && shape[0].kind == '(' && shape[1].is('p', "=>") && shape[2].kind == '(' && len(shape[2].items) == 1 {
		return shape[2].items[0]
	}
	return shape
}

// keyValue are an object literal item's key and value: `key: v`, `"key": v`, `key?: T`.
func keyValue(it []*tok) (string, []*tok) {
	if len(it) == 0 {
		return "", nil
	}
	k := it[0]
	if k.kind != 'i' && k.kind != 's' && k.kind != 'n' {
		return "", nil
	}
	rest := it[1:]
	if len(rest) > 0 && rest[0].is('p', "?") {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		if k.kind == 'i' {
			return k.text, nil
		}
		return "", nil
	}
	if !rest[0].is('p', ":") {
		return "", nil // a method, a getter
	}
	return k.text, rest[1:]
}

func valueOf(it []*tok) []*tok {
	_, v := keyValue(it)
	return v
}

func firstGroup(args *tok, kind byte) *tok {
	if args == nil {
		return nil
	}
	for _, a := range args.items {
		if len(a) >= 1 && a[0].kind == kind {
			return a[0]
		}
	}
	return nil
}

// values are the literals an array (or an enum object's values) lists, a spread of a constant
// array (...MODES) with them.
func (in *interp) values(g *tok, from *tok) []any {
	var out []any
	for _, it := range g.items {
		if len(it) == 0 {
			continue
		}
		if g.kind == '{' {
			it = valueOf(it)
		}
		if len(it) == 2 && it[0].is('p', "...") && it[1].kind == 'i' {
			if seq := in.lookupSeq(it[1].text, it[1]); len(seq) >= 1 && seq[0].kind == '[' {
				out = append(out, in.values(seq[0], seq[0])...)
			}
			continue
		}
		if v, ok := literal(it); ok {
			out = append(out, v)
		}
	}
	return out
}

// lookupSeq is the expression a name holds, without reading it as a schema.
func (in *interp) lookupSeq(name string, from *tok) []*tok {
	if in.lookup == nil || in.resolving[name] {
		return nil
	}
	return in.lookup(name, from, false)
}

// resolve reads the schema name holds where from uses it, once at a time: a schema that
// refers to itself (a recursive one) reads as nothing the second time.
func (in *interp) resolve(name string, from *tok, call bool) *node {
	if in.lookup == nil || in.resolving[name] {
		return nil
	}
	seq := in.lookup(name, from, call)
	if seq == nil {
		return nil
	}
	if in.resolving == nil {
		in.resolving = map[string]bool{}
	}
	in.resolving[name] = true
	defer delete(in.resolving, name)
	return in.schema(seq)
}

// literal is the value an expression holds where it is one written out: a string, a number,
// true and false (or a bundle's !0 and !1), null, and arrays and objects of them. `as const`
// and the like after it are no part of it.
func literal(seq []*tok) (any, bool) {
	for i, t := range seq {
		if t.is('i', "as") || t.is('i', "satisfies") {
			seq = seq[:i]
			break
		}
	}
	if len(seq) == 0 {
		return nil, false
	}
	t := seq[0]
	switch {
	case len(seq) == 2 && t.is('p', "!") && seq[1].kind == 'n':
		return seq[1].text == "0", true
	case len(seq) == 2 && t.is('p', "-") && seq[1].kind == 'n':
		if v, ok := number(seq[1].text); ok {
			return -v.(float64), true
		}
	case len(seq) == 2 && t.is('i', "void"):
		return nil, false
	case len(seq) != 1:
		return nil, false
	}
	switch t.kind {
	case 's':
		return t.text, true
	case 'n':
		return number(t.text)
	case 'i':
		switch t.text {
		case "true", "True":
			return true, true
		case "false", "False":
			return false, true
		case "null", "None":
			return nil, true
		}
	case '[':
		out := []any{}
		for _, it := range t.items {
			if len(it) == 0 {
				continue
			}
			v, ok := literal(it)
			if !ok {
				return nil, false
			}
			out = append(out, v)
		}
		return out, true
	case '{':
		out := map[string]any{}
		for _, it := range t.items {
			if len(it) == 0 {
				continue
			}
			k, val := keyValue(it)
			if k == "" || val == nil {
				return nil, false
			}
			v, ok := literal(val)
			if !ok {
				return nil, false
			}
			out[k] = v
		}
		return out, true
	}
	return nil, false
}

func number(s string) (any, bool) {
	s = strings.ReplaceAll(strings.TrimSuffix(s, "n"), "_", "")
	if v, err := strconv.ParseInt(s, 0, 64); err == nil {
		return float64(v), true
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v, true
	}
	return nil, false
}
