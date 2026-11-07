package refdiff

import (
	"strings"
	"unicode/utf8"
)

// BoundedNote is a version's changelog as the reference publishes it: a line longer than
// MaxChangelogLine cut there and marked, the whole cut at the last line that fits
// MaxChangelogText, and Truncated set where either cut anything, so a page that reads it never
// meets more. Every note is bounded where it is made: fetched, loaded back from published
// files, or joined for a range of versions (CompareRange).
func BoundedNote(url, text string) *ChangelogNote {
	note := &ChangelogNote{URL: url}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if len(line) > MaxChangelogLine {
			lines[i] = truncateUTF8(line, MaxChangelogLine-len("…")) + "…"
			note.Truncated = true
		}
	}
	var sb strings.Builder
	for i, line := range lines {
		if sb.Len()+len(line)+1 > MaxChangelogText {
			note.Truncated = true
			break
		}
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(line)
	}
	note.Text = sb.String()
	return note
}

// Bounded is n within the bounds: a note loaded back from what was published, or made before
// the bounds, cut as BoundedNote cuts, Truncated where it was already or where this cut it. A
// note within them is returned as it is.
func (n ChangelogNote) Bounded() ChangelogNote {
	b := BoundedNote(n.URL, n.Text)
	b.Truncated = b.Truncated || n.Truncated
	return *b
}

// truncateUTF8 is s cut to at most n bytes, never inside a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
