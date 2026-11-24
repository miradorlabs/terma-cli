package hookrun

import (
	"regexp"
	"slices"
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
		s += " …"
	}
	return s
}

// shellFields returns command's first n words as written alongside their unquoted forms.
func shellFields(command string, n int) (raw, bare []string) {
	var buf strings.Builder
	start, quote, escaped := -1, rune(0), false
	for i, r := range command + " " {
		if escaped {
			escaped = false
			buf.WriteRune(r)
			continue
		}
		switch {
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				buf.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case !unicode.IsSpace(r):
			buf.WriteRune(r)
		case start >= 0:
			raw, bare = append(raw, command[start:i]), append(bare, buf.String())
			buf.Reset()
			if len(raw) == n {
				return raw, bare
			}
			start = -1
			continue
		default:
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		raw, bare = append(raw, command[start:]), append(bare, buf.String())
	}
	return raw, bare
}
