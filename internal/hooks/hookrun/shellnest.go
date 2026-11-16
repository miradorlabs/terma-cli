package hookrun

import (
	"slices"
	"strings"
)

// Bounds on a script, checked before it is parsed (parsable), besides maxShellBytes. The
// parser recurses at each level of a script's nesting and at each operator of an arithmetic
// or test expression: a megabyte of either overflows its stack, which no recover catches, or
// takes it far past the hook's budget. maxShellNesting bounds the brackets, quotes and
// substitutions open at once, and the compound commands (if, for, case...), pipes and lists
// (|, &&, ||) a script holds; maxShellExpr the bytes of an expression: (( )), $(( )), [[ ]],
// [ ], $[ ], ${ } and let's. A megabyte of short words or expansions takes the parser some
// 40ms, so a script holding more than maxShellWords of either, which the scan would not
// read to the end, is not parsed either.
const (
	maxShellNesting = 4096
	maxShellExpr    = 4096
)

// compounds are the words that open a compound command; separators the bytes that end a
// word.
var (
	compounds  = []string{"if", "for", "while", "until", "case", "select"}
	separators = " \t\n;&|()<>"
)

// heredoc is a here-document's delimiter, and whether it was quoted (its body holds nothing).
type heredoc struct {
	delim  string
	quoted bool
}

// parsable reports whether src is within maxShellNesting, maxShellExpr and maxShellWords,
// lexed as a shell does: a comment, a single-quoted string and a quoted here-document's body
// hold nothing, and a double-quoted string and any other here-document's body hold only
// expansions. Where it may read src otherwise than the parser (a quote never closed, a
// here-document's delimiter followed by a parenthesis), it reads on as commands: a script
// misread is only ever taken for one past the bounds, never the other way.
func parsable(src string) bool {
	var open []byte // what closes each context open: ) } ] " `; h and q a body's (nothing)
	var docs, bodies []heredoc
	body, expr, exprDepth, links, words, dollars := 0, -1, 0, 0, 0, 0
	// push opens n contexts c closes, at i, an expression's when isExpr.
	push := func(c byte, n, i int, isExpr bool) {
		if isExpr && expr < 0 {
			expr, exprDepth = i, len(open)
		}
		for range n {
			open = append(open, closing(c))
		}
	}
	// read starts reading bodies[0]'s body.
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
		code := top != '"' && top != '`' && top != 'h' && top != 'q'
		if code && strings.IndexByte(separators, c) < 0 && strings.IndexByte(separators, prev) >= 0 {
			words++
		}
		if c == '$' && top != 'q' {
			dollars++
		}
		switch {
		case c == '\n' && (len(bodies) > 0 || len(docs) > 0 && top != '"' && top != '`'):
			if len(bodies) == 0 {
				bodies, docs = docs, nil
				read()
			}
			// Each body that ends at the next line, its delimiter's, one after another; a
			// context its substitutions left open stays open.
			for len(bodies) > 0 && i < len(src) && delimiterLine(src[i+1:lineEnd(src, i+1)], bodies[0].delim) {
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
		case c == '`':
			push(c, 1, i, false)
		case top == '"' || top == 'h':
		case c == '"':
			push(c, 1, i, false)
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
			if j := strings.IndexByte(src[i:], '\n'); j > 0 {
				i += j - 1
			} else {
				i = len(src)
			}
		case c == '(' || c == '[' || c == '{': // (( [[ [ open expressions
			n := 1
			if c != '{' && next == c {
				n = 2
			}
			i += n - 1
			push(c, n, i, n == 2 || c == '[')
		case c == top:
			open = open[:len(open)-1]
		case c == '|' || c == '&' && next == '&':
			links++
		case c == '<' && next == '<' && expr < 0 && !double:
			var d heredoc
			if d, i = heredocDelimiter(src, i+2); d.delim != "" {
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
				if end := strings.IndexAny(src[j:], "\n;&|"); end > maxShellExpr || end < 0 && len(src)-j > maxShellExpr {
					return false
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
	return true
}

// closing is the byte that closes the context c opens.
func closing(c byte) byte {
	switch c {
	case '(':
		return ')'
	case '{':
		return '}'
	case '[':
		return ']'
	}
	return c // a quote
}

// heredocDelimiter is the here-document whose operator ends at i (past its - and blanks),
// its delimiter's quotes removed, and the index of its word's last byte.
func heredocDelimiter(src string, i int) (heredoc, int) {
	for i < len(src) && strings.IndexByte("- \t", src[i]) >= 0 {
		i++
	}
	var d strings.Builder
	quoted := false
	for quote := byte(0); i < len(src); i++ {
		switch c := src[i]; {
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			d.WriteByte(c)
		case c == '\'' || c == '"':
			quote, quoted = c, true
		case c == '\\' && i+1 < len(src):
			i, quoted = i+1, true
			d.WriteByte(src[i])
		case strings.IndexByte(" \t\n;&|<>()", c) >= 0:
			return heredoc{d.String(), quoted}, i - 1
		default:
			d.WriteByte(c)
		}
	}
	return heredoc{d.String(), quoted}, i - 1
}

// lineEnd is the index of the newline that ends the line at i, or len(src).
func lineEnd(src string, i int) int {
	if j := strings.IndexByte(src[i:], '\n'); j >= 0 {
		return i + j
	}
	return len(src)
}

// delimiterLine reports whether line ends a here-document delimited by delim, taken
// leniently (tabs before delim, blanks or a parenthesis after it): a body is never read as
// longer than the parser reads it.
func delimiterLine(line, delim string) bool {
	return strings.TrimRight(strings.TrimLeft(line, "\t"), " \t\r);") == delim
}
