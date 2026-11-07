package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/e2e/refdiff"
)

// What the reference publishes of a changelog is bounded: a long line is cut and marked, the
// whole is cut at the last line that fits, never inside a character, and text within the
// bounds is published as it is.
func TestBoundedNote(t *testing.T) {
	plain := "- Added a thing\n- Fixed another"
	if n := boundedNote("u", plain); n.Text != plain || n.Truncated {
		t.Errorf("text within bounds = %+v", n)
	}
	long := "- " + strings.Repeat("界", refdiff.MaxChangelogLine)
	n := boundedNote("u", long+"\n- next")
	first := strings.SplitN(n.Text, "\n", 2)[0]
	if !n.Truncated || len(first) > refdiff.MaxChangelogLine || !utf8.ValidString(first) || !strings.HasSuffix(first, "…") {
		t.Errorf("long line: truncated=%v len=%d valid=%v", n.Truncated, len(first), utf8.ValidString(first))
	}
	if !strings.HasSuffix(n.Text, "\n- next") {
		t.Errorf("the lines after a long one are kept")
	}
	var sb strings.Builder
	for sb.Len() < refdiff.MaxChangelogText+5000 {
		sb.WriteString("- an item of the release notes\n")
	}
	n = boundedNote("u", sb.String())
	if !n.Truncated || len(n.Text) > refdiff.MaxChangelogText || !strings.HasSuffix(n.Text, "notes") {
		t.Errorf("long text: truncated=%v len=%d tail=%q", n.Truncated, len(n.Text), n.Text[len(n.Text)-10:])
	}
}
