package hookrun

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// secretOptions are the options whose value, the next word, firstWords leaves out, as it does
// that of any option whose name holds one of secretNames, and of redis-cli -a; userinfo is a
// URL's user and password.
var (
	secretOptions = []string{"-u", "--user", "-H", "--header", "-p", "--password"}
	secretNames   = []string{"pass", "token", "secret", "key", "auth"}
	userinfo      = regexp.MustCompile(`://[^/@\s]+@`)
)

// firstWords are command's first four words, split at whitespace outside quotes, at most 60
// characters, with what may be secret left out: an assignment's or an option's value
// (NAME=…, --password=…, -p…), the value of a secretOptions option or of one whose name
// holds a secretNames word (-H …, --api-key …), and a URL's user and password
// (https://…@host). Enough for doctor to name the command, and no more of it on disk.
func firstWords(command string) string {
	cut := len(command) > 4096 // past the four words' 60 characters, however masked
	if cut {
		n := 4096
		for !utf8.RuneStart(command[n]) {
			n--
		}
		command = command[:n]
	}
	words := shellFields(command, 5)
	more := cut || len(words) > 4
	words = words[:min(len(words), 4)]
	secret := false
	for i, w := range words {
		name, _, eq := strings.Cut(w, "=")
		switch {
		case secret:
			words[i] = "…"
		case eq && name != "":
			words[i] = name + "=…"
		case len(w) > 2 && w[0] == '-' && w[1] != '-':
			words[i] = w[:2] + "…"
		}
		words[i] = userinfo.ReplaceAllString(words[i], "://…@")
		lower := strings.ToLower(w)
		secret = slices.Contains(secretOptions, w) || w == "-a" && strings.HasSuffix(words[0], "redis-cli") ||
			strings.HasPrefix(w, "-") && slices.ContainsFunc(secretNames, func(n string) bool { return strings.Contains(lower, n) })
	}
	s, runes := strings.Join(words, " "), 0
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
// outside them.
func shellFields(command string, n int) []string {
	var words []string
	start, quote := -1, rune(0)
	for i, r := range command + " " {
		switch {
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
