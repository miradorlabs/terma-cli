package hookrun

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	secretOptions          = []string{"-c", "-u", "--user", "-U", "--proxy-user", "-H", "--header", "--proxy-header", "-p", "--password", "-b", "--cookie", "-d", "--data", "--data-ascii", "--data-raw", "--data-binary", "--data-urlencode", "--json", "--oauth2-bearer"}
	secretNames, compounds = []string{"pass", "token", "secret", "key", "auth"}, []string{"if", "for", "while", "until", "case", "select"}
	userinfo               = regexp.MustCompile(`(://)[^/\s]+@|[^/\s:@]*:[^/\s@]+@`)
)

// Pre-parse bounds preventing deep parser recursion or huge expression backtracking.
const maxShellNesting, maxShellExpr, separators = 4096, 4096, " \t\n;&|()<>"

// firstWords returns command's first four words (<=60 runes) with secret-like values masked.
func firstWords(command string) string { return maskWords(command, 0) }
func maskWords(command string, depth int) string {
	more := len(command) > 4096
	for n := min(len(command), 4096); more; n-- {
		if n == 0 || utf8.RuneStart(command[n]) {
			command = command[:n]
			break
		}
	}
	words, bare := shellFields(command, 5)
	more, words, bare = more || len(words) > 4, words[:min(len(words), 4)], bare[:min(len(bare), 4)]
	secret, redir, hasRedis := false, 0, false
	for i, b := range bare {
		hasRedis = hasRedis || strings.HasSuffix(b, "redis-cli")
		hasSubst := strings.Contains(b, "$(") || strings.Contains(b, "`") || strings.Contains(b, "${") || strings.Contains(b, "$IFS")
		if redir > 0 {
			if redir == 2 || hasSubst || userinfo.MatchString(b) {
				words[i] = "…"
			}
			redir = 0
			continue
		}
		if r := strings.TrimLeft(words[i], "0123456789"); strings.HasPrefix(r, "<") || strings.HasPrefix(r, ">") {
			if strings.HasPrefix(r, "<<<") && len(r) > 3 {
				words[i] = "<<<…"
			} else if hasSubst || strings.HasPrefix(r, "<(") || strings.HasPrefix(r, ">(") || userinfo.MatchString(b) {
				words[i] = "…"
			} else if r == "<<<" {
				redir = 2
			} else if strings.TrimLeft(r, "<>&-") == "" {
				redir = 1
			}
			continue
		}
		masked := userinfo.ReplaceAllString(b, "${1}…@")
		name, _, eq := strings.Cut(masked, "=")
		if secret || hasSubst || depth >= 2 && strings.ContainsAny(b, " \t\n") {
			words[i] = "…"
		} else if len(b) > 2 && b[0] == '-' && b[1] != '-' {
			words[i] = b[:2] + "…"
		} else if eq && name != "" && !strings.ContainsAny(name, " \t\n") {
			words[i] = name + "=…"
		} else if strings.ContainsAny(b, " \t\n") {
			if inner := maskWords(b, depth+1); inner != b {
				words[i] = inner
			}
		} else if masked != b {
			words[i] = masked
		}
		lower, short := strings.ToLower(b), len(b) > 2 && b[0] == '-' && b[1] != '-'
		secret = slices.Contains(secretOptions, b) || b == "eval" || b == "-a" && hasRedis ||
			short && (strings.ContainsRune(b[1:], 'c') || slices.Contains(secretOptions, "-"+b[len(b)-1:]) || b[len(b)-1] == 'a' && hasRedis) ||
			strings.HasPrefix(b, "-") && slices.ContainsFunc(secretNames, func(n string) bool { return strings.Contains(lower, n) })
	}
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.Join(words, " "))
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + " …"
	} else if more {
		return s + " …"
	}
	return s
}

// shellFields returns command's first n words as written alongside their unquoted forms.
func shellFields(command string, n int) (raw, bare []string) {
	var buf strings.Builder
	start, quote, cmdDepth, backtick, ansiC, ansiNul, dollar, escaped := -1, rune(0), 0, false, false, false, false, false
	for i := 0; i < len(command); {
		r, w := utf8.DecodeRuneInString(command[i:])
		switch {
		case escaped:
			escaped = false
			buf.WriteRune(r)
		case ansiC && r == '\'':
			quote, ansiC, ansiNul = 0, false, false
		case ansiC:
			if r == '\\' {
				r, w = ansiEscape(command[i+1:])
				w++
			}
			if ansiNul = ansiNul || r == 0; !ansiNul {
				buf.WriteRune(r)
			}
		case r == '\\' && quote != '\'' && i+1 < len(command) && (command[i+1] == '\n' || command[i+1] == '\r' && i+2 < len(command) && command[i+2] == '\n'):
			if i += 2; command[i-1] == '\r' {
				i++
			}
			continue
		case dollar && (r == '\'' || r == '"'):
			dollar, quote, ansiC, ansiNul = false, r, r == '\'', false
		case dollar && r == '(':
			dollar, cmdDepth = false, cmdDepth+1
			buf.WriteString("$(")
		default:
			if dollar {
				buf.WriteByte('$')
				dollar = false
			}
			switch {
			case r == '\\' && quote != '\'':
				escaped = true
			case quote != 0 && r == quote:
				quote = 0
			case quote != 0:
				buf.WriteRune(r)
			case r == '\'' || r == '"':
				quote = r
			case r == '$':
				dollar = true
			case r == '`':
				backtick = !backtick
				buf.WriteRune(r)
			case r == '(' && (cmdDepth > 0 || buf.String() == "<" || buf.String() == ">"):
				cmdDepth++
				buf.WriteRune(r)
			case r == ')' && cmdDepth > 0:
				cmdDepth--
				buf.WriteRune(r)
			case cmdDepth > 0:
				buf.WriteRune(r)
			case backtick || !unicode.IsSpace(r) && (r != '<' && r != '>' || start < 0 || strings.Trim(buf.String(), "0123456789<>&") == ""):
				buf.WriteRune(r)
			case start >= 0:
				raw, bare = append(raw, command[start:i]), append(bare, buf.String())
				if buf.Reset(); len(raw) == n {
					return raw, bare
				}
				if start = -1; r == '<' || r == '>' {
					start = i
					buf.WriteRune(r)
					break
				}
				fallthrough
			default:
				i += w
				continue
			}
		}
		if start < 0 {
			start = i
		}
		i += w
	}
	if start >= 0 {
		if dollar {
			buf.WriteByte('$')
		}
		raw, bare = append(raw, command[start:]), append(bare, buf.String())
	}
	return raw, bare
}
func ansiEscape(s string) (rune, int) {
	if s == "" {
		return '\\', 0
	} else if j := strings.IndexByte("abeEfnrtv\\'\"?", s[0]); j >= 0 {
		return rune("\a\b\x1b\x1b\f\n\r\t\v\\'\"?"[j]), 1
	} else if s[0] == 'c' && len(s) > 1 {
		return rune(s[1] & 0x1f), 2
	}
	base, start, limit, digits := 8, 0, 3, "01234567"
	if j := strings.IndexByte("xuU", s[0]); j >= 0 {
		base, start, limit, digits = 16, 1, 2<<j, "0123456789abcdefABCDEF"
	}
	k := 0
	for ; k < limit && start+k < len(s) && strings.IndexByte(digits, s[start+k]) >= 0; k++ {
	}
	if v, err := strconv.ParseUint(s[start:start+k], base, 32); k > 0 && err == nil {
		if base == 8 {
			v &= 0xff
		}
		return rune(v), start + k
	}
	return '\\', 0
}

type heredoc struct {
	delim             string
	quoted, stripTabs bool
}

// parsable reports whether src is within maxShellBytes, maxShellNesting, maxShellExpr, and maxShellWords.
func parsable(src string) bool {
	if len(src) > maxShellBytes {
		return false
	}
	var open []byte // what closes each context open: ) } ] " `; h and q a body's (nothing)
	var docs, bodies []heredoc
	body, expr, exprDepth, letUntil, links, words, dollars := 0, -1, 0, -1, 0, 0, 0
	push := func(c byte, n, i int, isExpr bool) {
		if isExpr && expr < 0 {
			expr, exprDepth = i, len(open)
		}
		if k := strings.IndexByte("({[", c); k >= 0 {
			c = ")}]"[k]
		}
		for range n {
			open = append(open, c)
		}
	}
	read := func() {
		marker := byte('h')
		if body = len(open); bodies[0].quoted {
			marker = 'q'
		}
		open = append(open, marker)
	}
	byteAt := func(k int, def byte) byte {
		if k >= 0 && k < len(src) {
			return src[k]
		}
		return def
	}
	for i := 0; i < len(src); i++ {
		c, top, prev, next := src[i], byte(0), byteAt(i-1, '\n'), byteAt(i+1, 0)
		if len(open) > 0 {
			top = open[len(open)-1]
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
			for len(bodies) > 0 && i < len(src) {
				line := src[i+1 : lineEnd(src, i+1)]
				if bodies[0].stripTabs {
					line = strings.TrimLeft(line, "\t")
				}
				if strings.TrimRight(line, "\r") != bodies[0].delim {
					break
				}
				i, open = lineEnd(src, i+1), slices.Delete(open, body, body+1)
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
			for j := i + 1; j < len(src); j++ {
				if src[j] == '\'' {
					i = j
					break
				} else if src[j] == '\\' && prev == '$' { // $'\''
					j++
				}
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
			if d, end := heredocDelimiter(src, i+2); end < 0 {
				return false
			} else if i = end; d.delim != "" {
				docs = append(docs, d)
			}
		case 'a' <= c && c <= 'z' && strings.IndexByte(" \t\n;&|(){}`", prev) >= 0:
			j := i
			for j < len(src) && 'a' <= src[j] && src[j] <= 'z' {
				j++
			}
			if word := src[i:j]; slices.Contains(compounds, word) {
				links++
			} else if word == "let" { // its words are expressions
				end := strings.IndexAny(src[j:], "\n;&|")
				if end < 0 {
					end = len(src) - j
				}
				if letUntil = j + end; end > maxShellExpr {
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
	return len(bodies) == 0 && len(docs) == 0
}
func heredocDelimiter(src string, i int) (heredoc, int) {
	stripTabs := i < len(src) && src[i] == '-'
	if stripTabs {
		i++
	}
	i += len(src[i:]) - len(strings.TrimLeft(src[i:], " \t"))
	var d strings.Builder
	quoted := false
	for quote := byte(0); i < len(src); i++ {
		if c := src[i]; quote != 0 && c == '\n' {
			return heredoc{}, -1
		} else if quote == '"' && c == '\\' && i+1 < len(src) && strings.IndexByte("\"\\`$", src[i+1]) >= 0 {
			i++
			d.WriteByte(src[i])
		} else if quote != 0 && c == quote {
			quote = 0
		} else if quote != 0 {
			d.WriteByte(c)
		} else if c == '$' && i+1 < len(src) && src[i+1] == '\'' {
			return heredoc{}, -1
		} else if c == '\'' || c == '"' {
			quote, quoted = c, true
		} else if c == '\\' && i+1 < len(src) {
			i, quoted = i+1, true
			d.WriteByte(src[i])
		} else if strings.IndexByte(" \t\n;&|<>()", c) >= 0 {
			return heredoc{d.String(), quoted, stripTabs}, i - 1
		} else {
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
