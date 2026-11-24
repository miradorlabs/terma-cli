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
	secretOptions = []string{"-u", "--user", "-U", "--proxy-user", "-H", "--header", "--proxy-header", "-p", "--password", "-b", "--cookie", "-d", "--data", "--data-raw", "--data-binary", "--data-urlencode", "--oauth2-bearer"}
	secretNames   = []string{"pass", "token", "secret", "key", "auth"}
	userinfo      = regexp.MustCompile(`://[^/\s]+@`)
)

// firstWords returns command's first four words (<=60 runes) with secret-like values masked.
func firstWords(command string) string { return maskWords(command, 0) }
func maskWords(command string, depth int) string {
	cut := len(command) > 4096
	for n := min(len(command), 4096); cut; n-- {
		if n == 0 || utf8.RuneStart(command[n]) {
			command = command[:n]
			break
		}
	}
	words, bare := shellFields(command, 5)
	more := cut || len(words) > 4
	words, bare = words[:min(len(words), 4)], bare[:min(len(bare), 4)]
	secret, hasRedis := false, false
	for i, b := range bare {
		hasRedis = hasRedis || strings.HasSuffix(b, "redis-cli")
		masked := userinfo.ReplaceAllString(b, "://…@")
		name, _, eq := strings.Cut(masked, "=")
		switch {
		case secret:
			words[i] = "…"
		case len(b) > 2 && b[0] == '-' && b[1] != '-':
			words[i] = b[:2] + "…"
		case eq && name != "" && !strings.ContainsAny(name, " \t\n"):
			words[i] = name + "=…"
		case strings.ContainsAny(b, " \t\n"):
			if depth >= 2 {
				words[i] = "…"
			} else if inner := maskWords(b, depth+1); inner != b {
				words[i] = inner
			}
		case masked != b:
			words[i] = masked
		}
		lower, short := strings.ToLower(b), len(b) > 2 && b[0] == '-' && b[1] != '-'
		secret = slices.Contains(secretOptions, b) || b == "-a" && hasRedis ||
			short && (slices.Contains(secretOptions, "-"+b[len(b)-1:]) || b[len(b)-1] == 'a' && hasRedis) ||
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
	start, quote, ansiC, ansiNul, dollar, escaped := -1, rune(0), false, false, false, false
	flush := func() {
		if dollar {
			buf.WriteByte('$')
			dollar = false
		}
	}
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
				var adv int
				r, adv = ansiEscape(command[i+1:])
				w = 1 + adv
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
		default:
			flush()
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
			case !unicode.IsSpace(r):
				buf.WriteRune(r)
			case start >= 0:
				raw, bare = append(raw, command[start:i]), append(bare, buf.String())
				if buf.Reset(); len(raw) == n {
					return raw, bare
				}
				start = -1
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
	if flush(); start >= 0 {
		raw, bare = append(raw, command[start:]), append(bare, buf.String())
	}
	return raw, bare
}
func ansiEscape(s string) (rune, int) {
	if s == "" {
		return '\\', 0
	}
	if j := strings.IndexByte("abeEfnrtv\\'\"?", s[0]); j >= 0 {
		return rune("\a\b\x1b\x1b\f\n\r\t\v\\'\"?"[j]), 1
	}
	if s[0] == 'c' && len(s) > 1 {
		return rune(s[1] & 0x1f), 2
	}
	base, start, limit, digits := 8, 0, 3, "01234567"
	if j := strings.IndexByte("xuU", s[0]); j >= 0 {
		base, start, limit, digits = 16, 1, 2<<j, "0123456789abcdefABCDEF"
	}
	k := 0
	for k < limit && start+k < len(s) && strings.IndexByte(digits, s[start+k]) >= 0 {
		k++
	}
	if v, err := strconv.ParseUint(s[start:start+k], base, 32); k > 0 && err == nil {
		if base == 8 {
			v &= 0xff
		}
		return rune(v), start + k
	}
	return '\\', 0
}
