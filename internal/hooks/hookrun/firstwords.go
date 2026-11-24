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
func firstWords(command string) string {
	cut := len(command) > 4096
	if cut {
		n := 4096
		for n > 0 && !utf8.RuneStart(command[n]) {
			n--
		}
		command = command[:n]
	}
	words, bare := shellFields(command, 5)
	more := cut || len(words) > 4
	words, bare = words[:min(len(words), 4)], bare[:min(len(bare), 4)]
	secret, hasRedis := false, false
	for i, b := range bare {
		hasRedis = hasRedis || strings.HasSuffix(b, "redis-cli")
		name, _, eq := strings.Cut(b, "=")
		switch {
		case secret:
			words[i] = "…"
		case len(b) > 2 && b[0] == '-' && b[1] != '-':
			words[i] = b[:2] + "…"
		case eq && name != "":
			words[i] = name + "=…"
		case userinfo.MatchString(b):
			words[i] = b
		}
		words[i] = userinfo.ReplaceAllString(words[i], "://…@")
		lower := strings.ToLower(b)
		tail := len(b) > 2 && b[0] == '-' && b[1] != '-' &&
			(slices.Contains(secretOptions, "-"+b[len(b)-1:]) || b[len(b)-1] == 'a' && hasRedis)
		secret = slices.Contains(secretOptions, b) || b == "-a" && hasRedis || tail ||
			strings.HasPrefix(b, "-") && slices.ContainsFunc(secretNames, func(n string) bool { return strings.Contains(lower, n) })
	}
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.Join(words, " "))
	if r := []rune(s); len(r) > 60 {
		s, more = string(r[:60]), true
	}
	if more {
		return s + " …"
	}
	return s
}

// shellFields returns command's first n words as written alongside their unquoted forms.
func shellFields(command string, n int) (raw, bare []string) {
	var buf strings.Builder
	start, quote, ansiC, dollar, escaped := -1, rune(0), false, false, false
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
			quote, ansiC = 0, false
		case ansiC && r == '\\':
			ch, adv := ansiEscape(command[i+1:])
			buf.WriteRune(ch)
			i += 1 + adv
			continue
		case ansiC:
			buf.WriteRune(r)
		case dollar && (r == '\'' || r == '"'):
			dollar, quote, ansiC = false, r, r == '\''
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
				start, i = -1, i+w
				continue
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
	if j := strings.IndexByte("xuU", s[0]); j >= 0 {
		k := 0
		for k < 2<<j && 1+k < len(s) && strings.ContainsRune("0123456789abcdefABCDEF", rune(s[1+k])) {
			k++
		}
		if v, err := strconv.ParseUint(s[1:1+k], 16, 32); k > 0 && err == nil {
			return rune(v), 1 + k
		}
	} else if s[0] >= '0' && s[0] <= '7' {
		k := 1
		for k < 3 && k < len(s) && s[k] >= '0' && s[k] <= '7' {
			k++
		}
		v, _ := strconv.ParseUint(s[:k], 8, 32)
		return rune(v & 0xff), k
	}
	return '\\', 0
}
