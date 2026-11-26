package style

import (
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// Columns is w's width when a person watches it as a terminal, else 0: output that is not
// watched, or that an agent or a dumb terminal reads, is never wrapped, so a copied line
// stays whole.
func Columns(w io.Writer) int {
	if !Terminal(w) {
		return 0
	}
	if h, ok := w.(highlighter); ok {
		w = h.w
	}
	cols, _, err := term.GetSize(int(w.(*os.File).Fd()))
	if err != nil {
		return 0
	}
	return cols
}

// Wrap breaks s at spaces into lines of at most width columns, never inside a `quoted
// command`; a word longer than width gets a line of its own. Width 0 or less is no limit.
func Wrap(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	var out []string
	line := ""
	for _, word := range words(s) {
		switch {
		case line == "":
			line = word
		case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= width:
			line += " " + word
		default:
			out = append(out, line)
			line = word
		}
	}
	return append(out, line)
}

// words splits s at spaces outside backquotes.
func words(s string) []string {
	var out []string
	var b strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '`':
			quoted = !quoted
			b.WriteRune(r)
		case r == ' ' && !quoted:
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}
