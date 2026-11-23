package hookrun

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	secretOptions = []string{"-u", "--user", "-H", "--header", "-p", "--password", "-b", "--cookie", "-d", "--data", "--data-raw", "--data-binary", "--data-urlencode", "--oauth2-bearer"}
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
	words := shellFields(command, 5)
	more := cut || len(words) > 4
	words = words[:min(len(words), 4)]
	secret, hasRedis := false, false
	for i, w := range words {
		bare := strings.Trim(w, `"'`)
		hasRedis = hasRedis || strings.HasSuffix(bare, "redis-cli")
		name, _, eq := strings.Cut(w, "=")
		switch {
		case secret:
			words[i] = "…"
		case eq && name != "":
			words[i] = name + "=…"
		case len(bare) > 2 && bare[0] == '-' && bare[1] != '-':
			words[i] = bare[:2] + "…"
		}
		words[i] = userinfo.ReplaceAllString(words[i], "://…@")
		lower := strings.ToLower(bare)
		tail := len(bare) > 2 && bare[0] == '-' && bare[1] != '-' &&
			(slices.Contains(secretOptions, "-"+bare[len(bare)-1:]) || bare[len(bare)-1] == 'a' && hasRedis)
		secret = slices.Contains(secretOptions, bare) || bare == "-a" && hasRedis || tail ||
			strings.HasPrefix(bare, "-") && slices.ContainsFunc(secretNames, func(n string) bool { return strings.Contains(lower, n) })
	}
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.Join(words, " "))
	runes := 0
	for i := range s {
		if runes++; runes > 60 {
			s, more = s[:i], true
			break
		}
	}
	if more {
		s += " …"
	}
	return s
}

// shellFields are command's first n words as written, quotes kept, split at whitespace
// outside them, honoring backslash escapes outside single quotes.
func shellFields(command string, n int) []string {
	var words []string
	start, quote, escaped := -1, rune(0), false
	for i, r := range command + " " {
		if escaped {
			escaped = false
			if start < 0 {
				start = i
			}
			continue
		}
		switch {
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case !unicode.IsSpace(r):
		case start >= 0:
			if words = append(words, command[start:i]); len(words) == n {
				return words
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
		words = append(words, command[start:]) // an open quote's
	}
	return words
}
