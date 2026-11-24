package hookrun

import (
	"slices"
	"strings"
)

// Pre-parse bounds preventing deep parser recursion or huge expression backtracking.
const (
	maxShellNesting = 4096
	maxShellExpr    = 4096
	separators      = " \t\n;&|()<>"
)

var compounds = []string{"if", "for", "while", "until", "case", "select"}

type heredoc struct {
	delim             string
	quoted, stripTabs bool
}

// parsable reports whether src is within maxShellNesting, maxShellExpr, and maxShellWords.
func parsable(src string) bool {
	var open []byte // what closes each context open: ) } ] " `; h and q a body's (nothing)
	var docs, bodies []heredoc
	body, expr, exprDepth, letUntil, links, words, dollars := 0, -1, 0, -1, 0, 0, 0
	push := func(c byte, n, i int, isExpr bool) {
		if isExpr && expr < 0 {
			expr, exprDepth = i, len(open)
		}
		for range n {
			open = append(open, closing(c))
		}
	}
	read := func() {
		marker := byte('h')
		if body = len(open); bodies[0].quoted {
			marker = 'q'
		}
		open = append(open, marker)
	}
	for i := 0; i < len(src); i++ {
		c, top, prev, next := src[i], byte(0), byte('\n'), byte(0)
		if len(open) > 0 {
			top = open[len(open)-1]
		}
		if i > 0 {
			prev = src[i-1]
		}
		if i+1 < len(src) {
			next = src[i+1]
		}
		double := i+2 < len(src) && src[i+2] == next
		if top != '"' && top != 'h' && top != 'q' && strings.IndexByte(separators, c) < 0 && strings.IndexByte(separators, prev) >= 0 {
			words++
		}
		if c == '$' && top != 'q' {
			dollars++
		}
		if c == '\n' || c == ';' || c == '&' || c == '|' {
			letUntil = -1
		}
		switch {
		case c == '\n' && (len(bodies) > 0 || len(docs) > 0 && top != '"' && top != '`'):
			if len(bodies) == 0 {
				bodies, docs = docs, nil
				read()
			}
			for len(bodies) > 0 && i < len(src) && delimiterLine(src[i+1:lineEnd(src, i+1)], bodies[0]) {
				i = lineEnd(src, i+1)
				open = slices.Delete(open, body, body+1)
				if bodies = bodies[1:]; len(bodies) > 0 {
					read()
				}
			}
		case top == 'q':
		case c == '\\':
			i++
		case c == '$' && (next == '(' || next == '{' || next == '['): // $( $(( ${ $[
			n := 1
			if next == '(' && double {
				n = 2
			}
			i += n
			push(next, n, i, n == 2 || next != '(')
		case c == '`' && top == '`', c == '"' && top == '"':
			open = open[:len(open)-1]
		case c == '`', c == '"' && top != 'h':
			push(c, 1, i, false)
		case top == '"' || top == 'h':
		case c == '\'':
			j := i + 1
			for ; j < len(src) && src[j] != '\''; j++ {
				if src[j] == '\\' && prev == '$' { // $'\''
					j++
				}
			}
			if j < len(src) {
				i = j
			}
		case c == '#' && strings.IndexByte(" \t\n;&|()", prev) >= 0:
			i = lineEnd(src, i) - 1
		case c == '(' || c == '[' || c == '{': // (( [[ [ open expressions
			n := 1
			if c != '{' && next == c {
				n = 2
			}
			i += n - 1
			push(c, n, i, n == 2 || c == '[')
		case c == top && len(open) > 0: // a NUL is no context's close
			open = open[:len(open)-1]
		case c == '|' || c == '&' && next == '&':
			links++
		case c == '<' && next == '<' && prev != '<' && expr < 0 && i >= letUntil && !double:
			var d heredoc
			if d, i = heredocDelimiter(src, i+2); i < 0 {
				return false
			} else if d.delim != "" {
				docs = append(docs, d)
			}
		case 'a' <= c && c <= 'z' && strings.IndexByte(" \t\n;&|(){}`", prev) >= 0:
			j := i
			for j < len(src) && 'a' <= src[j] && src[j] <= 'z' {
				j++
			}
			switch word := src[i:j]; {
			case slices.Contains(compounds, word):
				links++
			case word == "let": // its words are expressions
				end := strings.IndexAny(src[j:], "\n;&|")
				if end > maxShellExpr || end < 0 && len(src)-j > maxShellExpr {
					return false
				}
				letUntil = j + end
				if end < 0 {
					letUntil = len(src)
				}
			}
			i = j - 1
		}
		if expr >= 0 && len(open) <= exprDepth {
			expr = -1
		}
		if len(open) > maxShellNesting || links > maxShellNesting || expr >= 0 && i-expr > maxShellExpr ||
			words > maxShellWords || dollars > maxShellWords {
			return false
		}
	}
	return len(bodies) == 0 && len(docs) == 0
}
func closing(c byte) byte {
	if i := strings.IndexByte("({[", c); i >= 0 {
		return ")}]"[i]
	}
	return c
}
func heredocDelimiter(src string, i int) (heredoc, int) {
	stripTabs := i < len(src) && src[i] == '-'
	if stripTabs {
		i++
	}
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	var d strings.Builder
	quoted := false
	for quote := byte(0); i < len(src); i++ {
		switch c := src[i]; {
		case quote != 0 && c == '\n':
			return heredoc{}, -1
		case quote == '"' && c == '\\' && i+1 < len(src) && strings.IndexByte("\"\\`$", src[i+1]) >= 0:
			i++
			d.WriteByte(src[i])
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			d.WriteByte(c)
		case c == '$' && i+1 < len(src) && src[i+1] == '\'':
			return heredoc{}, -1
		case c == '\'' || c == '"':
			quote, quoted = c, true
		case c == '\\' && i+1 < len(src):
			i, quoted = i+1, true
			d.WriteByte(src[i])
		case strings.IndexByte(" \t\n;&|<>()", c) >= 0:
			return heredoc{d.String(), quoted, stripTabs}, i - 1
		default:
			d.WriteByte(c)
		}
	}
	return heredoc{d.String(), quoted, stripTabs}, i - 1
}
func lineEnd(src string, i int) int {
	if j := strings.IndexByte(src[i:], '\n'); j >= 0 {
		return i + j
	}
	return len(src)
}
func delimiterLine(line string, h heredoc) bool {
	if h.stripTabs {
		line = strings.TrimLeft(line, "\t")
	}
	return strings.TrimRight(line, "\r") == h.delim
}
