package configschema

import (
	"regexp"
	"strconv"
	"strings"
)

// Reading a Python dict of defaults (Hermes's DEFAULT_CONFIG): each key its default, its type
// the kind of that default, a nested dict its keys beneath it, and its description the comments
// that lead up to it or follow it on its line, as such a file documents its keys. A value the
// file computes (a call, a name) has no default here.

// FromPythonDict is the options of the dict literal assigned to name at the top level of src.
func FromPythonDict(src []byte, name string) Options {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*(?::[^=\n]+)?=\s*\{`)
	loc := re.FindIndex(src)
	if loc == nil {
		return nil
	}
	p := &pyReader{src: src, i: loc[1] - 1}
	n := p.value()
	if n == nil {
		return nil
	}
	return flatten(n)
}

type pyReader struct {
	src      []byte
	i        int
	comments []string // the comments read since the last item ended
}

// skip moves past spaces, newlines and comments, keeping the comments' text.
func (p *pyReader) skip() {
	for p.i < len(p.src) {
		c := p.src[p.i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\\':
			p.i++
		case c == '#':
			end := p.i
			for end < len(p.src) && p.src[end] != '\n' {
				end++
			}
			if text := strings.TrimSpace(strings.TrimLeft(string(p.src[p.i:end]), "#")); text != "" {
				p.comments = append(p.comments, text)
			}
			p.i = end
		default:
			return
		}
	}
}

// trailing is the comment after the item just read, on its line.
func (p *pyReader) trailing() string {
	j := p.i
	for j < len(p.src) && (p.src[j] == ' ' || p.src[j] == '\t' || p.src[j] == ',') {
		j++
	}
	if j < len(p.src) && p.src[j] == '#' {
		end := j
		for end < len(p.src) && p.src[end] != '\n' {
			end++
		}
		text := strings.TrimSpace(strings.TrimLeft(string(p.src[j:end]), "#"))
		if p.i < j && strings.ContainsRune(string(p.src[p.i:j]), ',') {
			p.i = end // the comma and the comment are this item's
		} else {
			p.i = end
		}
		return text
	}
	return ""
}

// value reads a value: a dict, a list, a literal, or an expression it reads only past.
func (p *pyReader) value() *node {
	p.skip()
	if p.i >= len(p.src) {
		return nil
	}
	switch c := p.src[p.i]; {
	case c == '{':
		return p.dict()
	case c == '[' || c == '(':
		start := p.i
		p.balanced()
		n := &node{types: []string{"array"}}
		if v, ok := pyLiteral(string(p.src[start:p.i])); ok {
			n.def, n.hasDef = v, true
		}
		return n
	}
	start := p.i
	p.expr()
	text := strings.TrimSpace(string(p.src[start:p.i]))
	n := &node{}
	if v, ok := pyLiteral(text); ok {
		if v != nil {
			n.addType(pyKind(v, text))
			n.def, n.hasDef = v, true
		}
	} else if v, ok := pyArith(text); ok {
		n.addType("integer")
		n.def, n.hasDef = v, true
	}
	return n
}

// dict reads a dict literal, its keys in order.
func (p *pyReader) dict() *node {
	p.i++ // {
	n := &node{types: []string{"object"}}
	p.comments = nil
	for {
		p.skip()
		if p.i >= len(p.src) {
			return n
		}
		if p.src[p.i] == '}' {
			p.i++
			p.comments = nil
			return n
		}
		if p.src[p.i] == ',' {
			p.i++
			continue
		}
		lead := strings.Join(p.comments, " ")
		p.comments = nil
		if p.src[p.i] == '*' { // **spread: its keys are computed
			p.expr()
			continue
		}
		key, ok := p.str()
		if !ok {
			p.expr()
			continue
		}
		p.skip()
		if p.i >= len(p.src) || p.src[p.i] != ':' {
			continue
		}
		p.i++
		v := p.value()
		if v == nil {
			return n
		}
		desc := lead
		if tail := p.trailing(); tail != "" {
			desc = strings.TrimSpace(desc + " " + tail)
		}
		if v.desc == "" {
			v.desc = desc
		}
		p.comments = nil
		n.setProp(key, v)
	}
}

// str reads a string literal at p.i.
func (p *pyReader) str() (string, bool) {
	j := p.i
	for j < len(p.src) && strings.ContainsRune("rRbBuUfF", rune(p.src[j])) && j-p.i < 2 {
		j++
	}
	if j >= len(p.src) || (p.src[j] != '"' && p.src[j] != '\'') {
		return "", false
	}
	q := p.src[j]
	var b strings.Builder
	for k := j + 1; k < len(p.src); k++ {
		c := p.src[k]
		if c == '\\' && k+1 < len(p.src) {
			b.WriteByte(p.src[k+1])
			k++
			continue
		}
		if c == q {
			p.i = k + 1
			return b.String(), true
		}
		if c == '\n' {
			break
		}
		b.WriteByte(c)
	}
	return "", false
}

// balanced moves past a bracketed run, its strings and comments with it.
func (p *pyReader) balanced() {
	depth := 0
	for p.i < len(p.src) {
		c := p.src[p.i]
		switch {
		case c == '"' || c == '\'':
			if _, ok := p.str(); !ok {
				p.i++
			}
			continue
		case c == '#':
			for p.i < len(p.src) && p.src[p.i] != '\n' {
				p.i++
			}
			continue
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
			if depth <= 0 {
				p.i++
				return
			}
		}
		p.i++
	}
}

// expr moves past an expression, to the "," or closing bracket that ends it.
func (p *pyReader) expr() {
	for p.i < len(p.src) {
		c := p.src[p.i]
		switch {
		case c == ',' || c == '}' || c == ']' || c == ')':
			return
		case c == '#' || c == '\n':
			return
		case c == '"' || c == '\'':
			if _, ok := p.str(); !ok {
				p.i++
			}
		case c == '(' || c == '[' || c == '{':
			p.balanced()
		default:
			p.i++
		}
	}
}

// pyLiteral is the value of a literal: a string, a number, True, False, None, and lists and
// tuples of them.
func pyLiteral(s string) (any, bool) {
	s = strings.TrimSpace(s)
	switch s {
	case "True":
		return true, true
	case "False":
		return false, true
	case "None":
		return nil, true
	}
	if s == "" {
		return nil, false
	}
	if (s[0] == '[' && s[len(s)-1] == ']') || (s[0] == '(' && s[len(s)-1] == ')') {
		inner := strings.TrimSpace(s[1 : len(s)-1])
		out := []any{}
		if inner == "" {
			return out, true
		}
		p := &pyReader{src: []byte(inner)}
		for {
			p.skip()
			if p.i >= len(p.src) {
				return out, true
			}
			start := p.i
			if p.src[p.i] == '[' || p.src[p.i] == '(' || p.src[p.i] == '{' {
				p.balanced()
			} else {
				p.expr()
			}
			v, ok := pyLiteral(string(p.src[start:p.i]))
			if !ok {
				return nil, false
			}
			out = append(out, v)
			p.skip()
			if p.i < len(p.src) && p.src[p.i] == ',' {
				p.i++
			}
		}
	}
	if s[0] == '"' || s[0] == '\'' || (len(s) > 1 && strings.ContainsRune("rRbBuU", rune(s[0])) && (s[1] == '"' || s[1] == '\'')) {
		p := &pyReader{src: []byte(s)}
		if v, ok := p.str(); ok && p.i == len(s) {
			return v, true
		}
		return nil, false
	}
	n := strings.ReplaceAll(s, "_", "")
	if v, err := strconv.ParseInt(n, 0, 64); err == nil {
		return float64(v), true
	}
	if v, err := strconv.ParseFloat(n, 64); err == nil {
		return v, true
	}
	return nil, false
}

// pyArith is the value of an integer expression of literals: 2 * 1024 * 1024.
func pyArith(s string) (float64, bool) {
	if !regexp.MustCompile(`^[\d\s*+_()-]+$`).MatchString(s) {
		return 0, false
	}
	total, term := 0.0, 1.0
	sign := 1.0
	for _, f := range regexp.MustCompile(`\d[\d_]*|[*+-]`).FindAllString(strings.NewReplacer("(", "", ")", "").Replace(s), -1) {
		switch f {
		case "*":
		case "+", "-":
			total += sign * term
			term = 1
			sign = map[string]float64{"+": 1, "-": -1}[f]
		default:
			v, _ := strconv.ParseFloat(strings.ReplaceAll(f, "_", ""), 64)
			term *= v
		}
	}
	return total + sign*term, true
}

func pyKind(v any, text string) string {
	if _, ok := v.(float64); ok && !strings.ContainsAny(text, ".eE") {
		return "integer"
	}
	return kindOf(v)
}
