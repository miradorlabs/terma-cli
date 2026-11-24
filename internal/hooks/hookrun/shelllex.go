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

var secretOptions, secretNames, compounds = []string{"-c", "-e", "--eval", "-u", "--user", "-U", "--proxy-user", "-H", "--header", "--proxy-header", "-p", "--password", "-b", "--cookie", "-d", "--data", "--data-ascii", "--data-raw", "--data-binary", "--data-urlencode", "--url-query", "--json", "--oauth2-bearer", "-E", "--cert", "--proxy-cert", "--post-data", "--body-data", "--post-file", "--body-file", "-F", "--form", "--form-string"}, []string{"pass", "token", "secret", "key", "auth"}, []string{"if", "for", "while", "until", "case", "select"}
var userinfo, shellMeta = regexp.MustCompile(`(://)[^/\s]+@|[^/\s:@]*:[^/\s@]+@`), [256]bool{' ': true, '\t': true, '\n': true, '\\': true, '$': true, '`': true, '"': true, '\'': true, '(': true, ')': true, '[': true, ']': true, '{': true, '}': true, '|': true, '&': true, '<': true, '>': true, ';': true}

const maxShellNesting, maxShellExpr, separators = 256, 4096, " \t\n;&|()<>"

// firstWords returns command's first four words (<=60 runes) with secret-like values masked.
func firstWords(command string) string { return maskWords(command, 0) }
func maskWords(command string, depth int) string {
	more, n := len(command) > 4096, min(len(command), 4096)
	for ; more && n > 0 && !utf8.RuneStart(command[n]); n-- {
	}
	words, bare := shellFields(command[:n], 5)
	more, words, bare = more || len(words) > 4, words[:min(len(words), 4)], bare[:min(len(bare), 4)]
	secret, redir, heredoc, hasRedis, hasSubst := 0, 0, false, false, false
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
			masked, short := userinfo.ReplaceAllString(b, "${1}…@"), len(b) > 2 && b[0] == '-' && b[1] != '-'
			name, _, eq := strings.Cut(masked, "=")
			if secret > 0 || hasSubst || depth >= 2 && strings.ContainsAny(b, " \t\n") {
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
			one := hasSubst || slices.Contains(secretOptions, b) || len(b) >= 4 && strings.HasPrefix(b, "--") && slices.ContainsFunc(secretOptions, func(o string) bool { return strings.HasPrefix(o, b) }) || b == "eval" || b == "-a" && hasRedis || short && (strings.ContainsRune(b[1:], 'c') || slices.Contains(secretOptions, "-"+b[len(b)-1:]) || b[len(b)-1] == 'a' && hasRedis) || strings.HasPrefix(b, "-") && slices.ContainsFunc(secretNames, func(n string) bool { return strings.Contains(lower, n) })
			secret = max(secret-1, pick(hasRedis && lower == "auth", 2, pick(one, 1, 0)))
		}
	}
	r := []rune(strings.Map(func(r rune) rune { return pick(r < 0x20 || r == 0x7f, ' ', r) }, strings.Join(words, " ")))
	return pick(len(r) > 60, string(r[:min(len(r), 60)])+" …", string(r)+pick(more, " …", ""))
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
				if start = pick(r == '<' || r == '>', i, -1); start < 0 {
					i += w
					continue
				}
				buf.WriteRune(r)
			default:
				i += w
				continue
			}
		}
		start, i = pick(start < 0, i, start), i+w
	}
	if start >= 0 {
		raw, bare = append(raw, command[start:]), append(bare, buf.String()+pick(dollar, "$", ""))
	}
	return raw, bare
}
func ansiEscape(s string) (rune, int) {
	if j := pick(s != "", strings.IndexByte("abeEfnrtv\\'\"?", byteAt(s, 0)), -1); j >= 0 {
		return rune("\a\b\x1b\x1b\f\n\r\t\v\\'\"?"[j]), 1
	} else if len(s) > 1 && s[0] == 'c' {
		return rune(s[1] & 0x1f), 2
	}
	j := pick(s != "", strings.IndexByte("xuU", byteAt(s, 0)), -1)
	base, start, limit, digits, k := pick(j >= 0, 16, 8), pick(j >= 0, 1, 0), pick(j >= 0, 2<<max(j, 0), 3), pick(j >= 0, "0123456789abcdefABCDEF", "01234567"), 0
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
	open, docs, bodies, body, expr, exprDepth, letUntil, links, words, dollars := []byte{0}, []heredoc(nil), []heredoc(nil), 0, -1, 0, -1, 0, 0, 0
	push := func(c byte, n, i int, isExpr bool) {
		expr, exprDepth = pick(isExpr && expr < 0, i, expr), pick(isExpr && expr < 0, len(open), exprDepth)
		k := strings.IndexByte("({[", c)
		open = append(open, slices.Repeat([]byte{pick(k >= 0, ")}]"[max(k, 0)], c)}, n)...)
	}
	read := func() { body, open = len(open), append(open, pick(bodies[0].quoted, byte('q'), byte('h'))) }
	for i := 0; i < len(src); i++ {
		c, top := src[i], open[len(open)-1]
		if top == 0 && (c == ' ' || c == '\t' || c == '\n' && len(bodies) == 0 && len(docs) == 0) {
			for letUntil = pick(c == '\n', -1, letUntil); i+1 < len(src) && (src[i+1] == ' ' || src[i+1] == '\t' || src[i+1] == '\n' && len(bodies) == 0 && len(docs) == 0); i, letUntil = i+1, pick(src[i+1] == '\n', -1, letUntil) {
			}
			continue
		}
		prev, ni := pick(i > 0, byteAt(src, i-1), byte('\n')), pick(top == 'q', i+1, skipCont(src, i+1))
		next, di := byteAt(src, ni), pick(top == 'q', ni+1, skipCont(src, ni+1))
		double := di < len(src) && src[di] == next
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
				if strings.TrimRight(pick(bodies[0].stripTabs, strings.TrimLeft(line, "\t"), line), "\r") != bodies[0].delim {
					break
				}
				i, open = lineEnd(src, i+1), slices.Delete(open, body, body+1)
				if bodies = bodies[1:]; len(bodies) > 0 {
					read()
				}
			}
		case top == 'q':
			i = lineEnd(src, i) - 1
		case c == '\\':
			for i = max(skipCont(src, i)-1, i+1); i+2 < len(src) && src[i] != '\n' && src[i+1] == '\\' && src[i+2] != '\n' && src[i+2] != '\r'; i += 2 {
			}
		case c == '$' && (next == '(' || next == '{' || next == '['): // $( $(( ${ $[
			i = pick(next == '(' && double, di, ni)
			push(next, pick(next == '(' && double, 2, 1), i, next == '(' && double || next != '(')
		case c == '`' && top == '`', c == '"' && top == '"':
			open = open[:len(open)-1]
		case c == '`', c == '"' && top != 'h':
			push(c, 1, i, false)
		case top == '"' || top == 'h':
			j := strings.IndexAny(src[i+1:], pick(top == 'h', "\n\\$`", "\\$`\""))
			i = pick(j >= 0, i+j, len(src)-1)
		case c == '\'':
			for i++; i < len(src) && src[i] != '\''; i += 1 + pick(src[i] == '\\' && prev == '$', 1, 0) {
			}
		case c == '#' && strings.IndexByte(" \t\n;&|()", prev) >= 0:
			words, i = words+1, lineEnd(src, i)-1
		case c == '(' || c == '[' || c == '{': // (( [[ [ open expressions
			i = pick(c != '{' && next == c, ni, i)
			push(c, pick(c != '{' && next == c, 2, 1), i, c != '{' && next == c || c == '[')
		case c == top && len(open) > 1: // a NUL is no context's close
			open = open[:len(open)-1]
		case c == '|' || c == '&' && next == '&':
			links, i = links+1, pick(c == '&', ni, i)
		case c == '<' && next == '<' && prev != '<' && expr < 0 && i >= letUntil && !double:
			if d, end := heredocDelimiter(src, ni+1); end < 0 {
				return false
			} else if i = end; d.delim != "" {
				docs = append(docs, d)
			}
		case strings.IndexByte(";&<>)]}", c) >= 0:
			words += 16
		case 'a' <= c && c <= 'z' && strings.IndexByte(" \t\n;&|(){}`", prev) >= 0:
			n, j, buf := 0, i, [8]byte{}
			for ; j < len(src) && 'a' <= src[j] && src[j] <= 'z'; j = skipCont(src, j+1) {
				buf[min(n, 7)], n = src[j], n+1
			}
			if word := string(buf[:min(n, 8)]); n <= 7 && slices.Contains(compounds, word) {
				links++
			} else if n == 3 && word == "let" { // its words are expressions
				for letUntil = j; letUntil < len(src) && strings.IndexByte("\n;&|", src[letUntil]) < 0; letUntil = max(skipCont(src, letUntil), letUntil+1) {
				}
				if letUntil-j > maxShellExpr {
					return false
				}
			}
			i = j - 1
		case !shellMeta[c]:
			for ; i+1 < len(src) && !shellMeta[src[i+1]]; i++ {
			}
		}
		expr = pick(expr >= 0 && len(open) <= exprDepth, -1, expr)
		if len(open) > maxShellNesting+1 || links > maxShellNesting || expr >= 0 && i-expr > maxShellExpr || words > maxShellWords || dollars > maxShellWords {
			return false
		}
	}
	return len(bodies) == 0 && len(docs) == 0
}
func byteAt(s string, i int) byte {
	return pick(i >= 0 && i < len(s), s, "\x00")[pick(i >= 0 && i < len(s), i, 0)]
}
func skipCont(src string, i int) int {
	for i+1 < len(src) && src[i] == '\\' && (src[i+1] == '\n' || src[i+1] == '\r' && i+2 < len(src) && src[i+2] == '\n') {
		i += pick(src[i+1] == '\r', 3, 2)
	}
	return i
}
func heredocDelimiter(src string, i int) (heredoc, int) {
	i = skipCont(src, i)
	stripTabs, quoted := i < len(src) && src[i] == '-', false
	var d strings.Builder
	for quote, i := byte(0), len(src)-len(strings.TrimLeft(src[i+pick(stripTabs, 1, 0):], " \t")); i < len(src); i++ {
		switch c := src[i]; {
		case quote != '\'' && skipCont(src, i) > i:
			i = skipCont(src, i) - 1
		case quote != 0 && c == '\n', quote == 0 && c == '$' && i+1 < len(src) && src[i+1] == '\'':
			return heredoc{}, -1
		case c == '\\' && i+1 < len(src) && (quote == 0 || quote == '"' && strings.IndexByte("\"\\`$", src[i+1]) >= 0):
			i, quoted = i+1, quoted || quote == 0
			d.WriteByte(src[i])
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '\'' || c == '"'):
			quote, quoted = c, true
		case quote == 0 && strings.IndexByte(" \t\n;&|<>()", c) >= 0:
			return heredoc{d.String(), quoted, stripTabs}, i - 1
		default:
			d.WriteByte(c)
		}
	}
	return heredoc{d.String(), quoted, stripTabs}, len(src) - 1
}
func lineEnd(src string, i int) int {
	j := strings.IndexByte(src[i:], '\n')
	return pick(j >= 0, i+j, len(src))
}
