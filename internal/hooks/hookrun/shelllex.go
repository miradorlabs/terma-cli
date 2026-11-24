package hookrun

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var secretOptions = []string{"-c", "-e", "--eval", "-u", "--user", "-U", "--proxy-user", "-H", "--header", "--proxy-header", "-p", "--password", "-b", "--cookie", "-d", "--data", "--data-ascii", "--data-raw", "--data-binary", "--data-urlencode", "--json", "--oauth2-bearer", "-E", "--cert", "--proxy-cert"}
var secretNames, compounds = []string{"pass", "token", "secret", "key", "auth"}, []string{"if", "for", "while", "until", "case", "select"}
var userinfo = regexp.MustCompile(`(://)[^/\s]+@|[^/\s:@]*:[^/\s@]+@`)

// Pre-parse bounds preventing deep parser recursion or huge expression backtracking.
const maxShellNesting, maxShellExpr, separators = 256, 4096, " \t\n;&|()<>"

// firstWords returns command's first four words (<=60 runes) with secret-like values masked.
func firstWords(command string) string { return maskWords(command, 0) }
func maskWords(command string, depth int) string {
	more, n := len(command) > 4096, min(len(command), 4096)
	for more && n > 0 && !utf8.RuneStart(command[n]) {
		n--
	}
	command = command[:n]
	words, bare := shellFields(command, 5)
	more, words, bare = more || len(words) > 4, words[:min(len(words), 4)], bare[:min(len(bare), 4)]
	secret, redir, heredoc, hasRedis, hasSubst := false, 0, false, false, false
	for i, b := range bare {
		hasRedis, hasSubst = hasRedis || strings.HasSuffix(b, "redis-cli"), strings.ContainsAny(b, "`$") || strings.ContainsRune(b, '{') && (strings.ContainsRune(b, ',') || strings.Contains(b, ".."))
		if heredoc {
			words[i] = "…"
		} else if redir > 0 {
			words[i] = pick(redir == 2 || hasSubst || userinfo.MatchString(b), "…", words[i])
			heredoc, redir = redir == 3, 0
		} else if r := strings.TrimLeft(words[i], "0123456789"); strings.HasPrefix(r, "<") || strings.HasPrefix(r, ">") {
			if strings.HasPrefix(r, "<<<") && len(r) > 3 {
				words[i] = "<<<…"
			} else if strings.HasPrefix(r, "<<") && !strings.HasPrefix(r, "<<<") && r != "<<" && r != "<<-" {
				heredoc, words[i] = true, pick(hasSubst || userinfo.MatchString(b), "…", words[i])
			} else if hasSubst || strings.HasPrefix(r, "<(") || strings.HasPrefix(r, ">(") || userinfo.MatchString(b) {
				words[i] = "…"
			} else if strings.TrimLeft(r, "<>&-") == "" {
				redir = cmp.Or(map[string]int{"<<": 3, "<<-": 3, "<<<": 2}[r], 1)
			}
		} else {
			masked := userinfo.ReplaceAllString(b, "${1}…@")
			name, _, eq := strings.Cut(masked, "=")
			short := len(b) > 2 && b[0] == '-' && b[1] != '-'
			if secret || hasSubst || depth >= 2 && strings.ContainsAny(b, " \t\n") {
				words[i] = "…"
			} else if short {
				words[i] = b[:2] + "…"
			} else if eq && name != "" && !strings.ContainsAny(name, " \t\n") {
				words[i] = name + "=…"
			} else if strings.ContainsAny(b, " \t\n") {
				inner := maskWords(b, depth+1)
				words[i] = pick(inner != b, inner, words[i])
			} else if masked != b {
				words[i] = masked
			}
			lower := strings.ToLower(b)
			secret = hasSubst || slices.Contains(secretOptions, b) || len(b) >= 4 && strings.HasPrefix(b, "--") && slices.ContainsFunc(secretOptions, func(o string) bool { return strings.HasPrefix(o, b) }) || b == "eval" || b == "-a" && hasRedis ||
				short && (strings.ContainsRune(b[1:], 'c') || slices.Contains(secretOptions, "-"+b[len(b)-1:]) || b[len(b)-1] == 'a' && hasRedis) ||
				strings.HasPrefix(b, "-") && slices.ContainsFunc(secretNames, func(n string) bool { return strings.Contains(lower, n) })
		}
	}
	s := strings.Map(func(r rune) rune { return pick(r < 0x20 || r == 0x7f, ' ', r) }, strings.Join(words, " "))
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + " …"
	}
	return s + pick(more, " …", "")
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
			i += pick(command[i+1] == '\r', 3, 2)
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
			case quote != 0, cmdDepth > 0 && r != '(' && r != ')':
				buf.WriteRune(r)
			case r == '\'' || r == '"':
				quote = r
			case r == '$':
				dollar = true
			case r == '`':
				backtick = !backtick
				buf.WriteRune(r)
			case r == '(' && (cmdDepth > 0 || buf.String() == "<" || buf.String() == ">"), r == ')' && cmdDepth > 0:
				cmdDepth += pick(r == '(', 1, -1)
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
		start = pick(start < 0, i, start)
		i += w
	}
	if start >= 0 {
		raw, bare = append(raw, command[start:]), append(bare, buf.String()+pick(dollar, "$", ""))
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
		return rune(v & pick(base == 8, uint64(0xff), ^uint64(0))), start + k
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
	open, docs, bodies := []byte(nil), []heredoc(nil), []heredoc(nil)
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
	read := func() { body, open = len(open), append(open, pick(bodies[0].quoted, byte('q'), byte('h'))) }
	for i := 0; i < len(src); i++ {
		c, top := src[i], byte(0)
		if len(open) > 0 {
			top = open[len(open)-1]
		}
		if top == 0 && (c == ' ' || c == '\t' || c == '\n' && len(bodies) == 0 && len(docs) == 0) {
			for ; i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' && len(bodies) == 0 && len(docs) == 0); i++ {
				letUntil = pick(src[i] == '\n', -1, letUntil)
			}
			i--
			continue
		}
		prev, next := byte('\n'), byte(0)
		if i > 0 {
			prev = src[i-1]
		}
		if i+1 < len(src) {
			next = src[i+1]
		}
		double := i+2 < len(src) && src[i+2] == next
		words += pick(top != '"' && top != 'h' && top != 'q' && (strings.IndexByte(separators, c) < 0 && strings.IndexByte(separators, prev) >= 0 || strings.IndexByte("\"'`([{", c) >= 0), 1, 0)
		dollars += pick((c == '$' || c == '`') && top != 'q', 1, 0)
		letUntil = pick(strings.IndexByte("\n;&|", c) >= 0, -1, letUntil)
		switch {
		case c == '\n' && (len(bodies) > 0 || len(docs) > 0 && top != '"' && top != '`'):
			if len(bodies) == 0 {
				bodies, docs = docs, nil
				read()
			}
			for len(bodies) > 0 && i < len(src) {
				line := src[i+1 : lineEnd(src, i+1)]
				line = pick(bodies[0].stripTabs, strings.TrimLeft(line, "\t"), line)
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
			n := pick(next == '(' && double, 2, 1)
			i += n
			push(next, n, i, n == 2 || next != '(')
		case c == '`' && top == '`', c == '"' && top == '"':
			open = open[:len(open)-1]
		case c == '`', c == '"' && top != 'h':
			push(c, 1, i, false)
		case top == '"' || top == 'h':
		case c == '\'':
			for i++; i < len(src) && src[i] != '\''; i++ {
				if src[i] == '\\' && prev == '$' { // $'\''
					i++
				}
			}
		case c == '#' && strings.IndexByte(" \t\n;&|()", prev) >= 0:
			words, i = words+1, lineEnd(src, i)-1
		case c == '(' || c == '[' || c == '{': // (( [[ [ open expressions
			n := pick(c != '{' && next == c, 2, 1)
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
			for ; j < len(src) && 'a' <= src[j] && src[j] <= 'z'; j++ {
			}
			if word := src[i:j]; slices.Contains(compounds, word) {
				links++
			} else if word == "let" { // its words are expressions
				end := lineEnd(src, j) - j
				if k := strings.IndexAny(src[j:j+end], ";&|"); k >= 0 {
					end = k
				}
				if letUntil = j + end; end > maxShellExpr {
					return false
				}
			}
			i = j - 1
		}
		expr = pick(expr >= 0 && len(open) <= exprDepth, -1, expr)
		if len(open) > maxShellNesting || links > maxShellNesting || expr >= 0 && i-expr > maxShellExpr || words > maxShellWords || dollars > maxShellWords {
			return false
		}
	}
	return len(bodies) == 0 && len(docs) == 0
}
func heredocDelimiter(src string, i int) (heredoc, int) {
	stripTabs, quoted := i < len(src) && src[i] == '-', false
	i += pick(stripTabs, 1, 0)
	i += len(src[i:]) - len(strings.TrimLeft(src[i:], " \t"))
	var d strings.Builder
	for quote := byte(0); i < len(src); i++ {
		switch c := src[i]; {
		case quote != 0 && c == '\n', quote == 0 && c == '$' && i+1 < len(src) && src[i+1] == '\'':
			return heredoc{}, -1
		case quote == '"' && c == '\\' && i+1 < len(src) && strings.IndexByte("\"\\`$", src[i+1]) >= 0:
			i++
			d.WriteByte(src[i])
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '\'' || c == '"'):
			quote, quoted = c, true
		case quote == 0 && c == '\\' && i+1 < len(src):
			i, quoted = i+1, true
			d.WriteByte(src[i])
		case quote == 0 && strings.IndexByte(" \t\n;&|<>()", c) >= 0:
			return heredoc{d.String(), quoted, stripTabs}, i - 1
		default:
			d.WriteByte(c)
		}
	}
	return heredoc{d.String(), quoted, stripTabs}, i - 1
}
func lineEnd(src string, i int) int {
	j := strings.IndexByte(src[i:], '\n')
	return pick(j >= 0, i+j, len(src))
}
