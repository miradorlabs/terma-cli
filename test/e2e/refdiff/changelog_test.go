package refdiff

import (
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// What the reference publishes of a changelog is bounded: a long line is cut and marked, the
// whole is cut at the last line that fits, never inside a character, and text within the
// bounds is published as it is.
func TestBoundedNote(t *testing.T) {
	plain := "- Added a thing\n- Fixed another"
	if n := BoundedNote("u", plain); n.Text != plain || n.Truncated {
		t.Errorf("text within bounds = %+v", n)
	}
	long := "- " + strings.Repeat("界", MaxChangelogLine)
	n := BoundedNote("u", long+"\n- next")
	first := strings.SplitN(n.Text, "\n", 2)[0]
	if !n.Truncated || len(first) > MaxChangelogLine || !utf8.ValidString(first) || !strings.HasSuffix(first, "…") {
		t.Errorf("long line: truncated=%v len=%d valid=%v", n.Truncated, len(first), utf8.ValidString(first))
	}
	if !strings.HasSuffix(n.Text, "\n- next") {
		t.Errorf("the lines after a long one are kept")
	}
	var sb strings.Builder
	for sb.Len() < MaxChangelogText+5000 {
		sb.WriteString("- an item of the release notes\n")
	}
	n = BoundedNote("u", sb.String())
	if !n.Truncated || len(n.Text) > MaxChangelogText || !strings.HasSuffix(n.Text, "notes") {
		t.Errorf("long text: truncated=%v len=%d tail=%q", n.Truncated, len(n.Text), n.Text[len(n.Text)-10:])
	}
}

// A note loaded back is bounded as a fetched one is, keeping a cut it had: a note within the
// bounds is as it was.
func TestBoundedLoadedNote(t *testing.T) {
	if n := (ChangelogNote{URL: "u", Text: "- a", Truncated: true}).Bounded(); n.Text != "- a" || !n.Truncated {
		t.Errorf("a note cut before = %+v", n)
	}
	long := ChangelogNote{URL: "u", Text: strings.Repeat("x", MaxChangelogLine+10)}
	if n := long.Bounded(); !n.Truncated || len(n.Text) > MaxChangelogLine {
		t.Errorf("a long loaded note = %d bytes, truncated %v", len(n.Text), n.Truncated)
	}
	once := BoundedNote("u", strings.Repeat("- an item\n", MaxChangelogText/5))
	if twice := once.Bounded(); twice != *once {
		t.Error("bounding a bounded note changed it")
	}
}

// A range's notes joined are bounded as one note is: the whole within MaxChangelogText,
// truncated where the join was cut or any part had been.
func TestCompareRangeBoundsChangelog(t *testing.T) {
	from, to := &Snapshot{Harness: "codex", Version: "1.0.0"}, &Snapshot{Harness: "codex", Version: "1.0.40"}
	notes := map[string]ChangelogNote{}
	var sb strings.Builder
	for sb.Len() < 40<<10 {
		sb.WriteString("- an item of the release notes\n")
	}
	for i := 1; i <= 40; i++ {
		notes["1.0."+strconv.Itoa(i)] = ChangelogNote{URL: "https://x/" + strconv.Itoa(i), Text: sb.String()}
	}
	d := CompareRange(from, to, notes, time.Now())
	if d.Changelog == nil || len(d.Changelog.Text) > MaxChangelogText || !d.Changelog.Truncated {
		t.Fatalf("joined changelog: %d bytes, truncated %v", len(d.Changelog.Text), d.Changelog.Truncated)
	}
	if !strings.HasPrefix(d.Changelog.Text, "## 1.0.40\n") || d.Changelog.URL != "https://x/40" {
		t.Errorf("newest first: %q, %s", d.Changelog.Text[:20], d.Changelog.URL)
	}
	// Short notes, one of them cut when it was fetched: the join is whole, and truncated.
	small := map[string]ChangelogNote{"1.0.1": {URL: "u1", Text: "- a"}, "1.0.2": {URL: "u2", Text: "- b…", Truncated: true}}
	d = CompareRange(from, &Snapshot{Harness: "codex", Version: "1.0.2"}, small, time.Now())
	if d.Changelog.Text != "## 1.0.2\n\n- b…\n\n## 1.0.1\n\n- a" || !d.Changelog.Truncated {
		t.Errorf("joined = %+v", d.Changelog)
	}
	small["1.0.2"] = ChangelogNote{URL: "u2", Text: "- b"}
	if d = CompareRange(from, &Snapshot{Harness: "codex", Version: "1.0.2"}, small, time.Now()); d.Changelog.Truncated {
		t.Errorf("nothing was cut: %+v", d.Changelog)
	}
}
