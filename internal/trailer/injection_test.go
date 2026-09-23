package trailer

import (
	"strings"
	"testing"
)

// A trailer is one line. A value carrying a newline must not become a second line
// that git, `git interpret-trailers`, and the GitHub App would read as a real trailer.
func TestStampSkipsMultilineSessionID(t *testing.T) {
	evil := "abc\nCo-authored-by: Attacker <a@evil.test>"
	out, changed := Stamp("fix: a thing\n", []Trailer{{SessionID: evil, Tool: "claude-code"}}, "#")
	if changed {
		t.Fatalf("expected no stamp for a multiline id, got:\n%s", out)
	}
	if strings.Contains(out, "Co-authored-by") {
		t.Fatalf("forged trailer reached the message:\n%s", out)
	}
}

// A tool label is not an identifier, so it is collapsed rather than dropping the
// whole attribution: the session id is still worth recording.
func TestFormatCollapsesMultilineTool(t *testing.T) {
	lines := Format(Trailer{SessionID: "s1", Tool: "claude-code\nAgent-Session-Id: forged"})
	for _, l := range lines {
		if strings.ContainsAny(l, "\r\n") {
			t.Fatalf("a rendered trailer line contains a line break: %q", l)
		}
	}
	if len(lines) != 2 || lines[1] != KeyTool+": claude-codeAgent-Session-Id: forged" {
		t.Fatalf("unexpected lines: %q", lines)
	}
}

// Whatever goes in, no output line may ever break out of its trailer.
func TestStampOutputIsAlwaysWellFormed(t *testing.T) {
	for _, tc := range []Trailer{
		{SessionID: "ok", Tool: "a\r\nb"},
		{SessionID: "ok2", Tool: "a\rb"},
		{SessionID: "ok3", Tool: "\n\n"},
	} {
		out, _ := Stamp("subject\n", []Trailer{tc}, "#")
		for line := range strings.SplitSeq(out, "\n") {
			if strings.HasPrefix(line, KeySessionID+":") || strings.HasPrefix(line, KeyTool+":") {
				continue
			}
			if line != "" && line != "subject" {
				t.Fatalf("unexpected line %q in:\n%s", line, out)
			}
		}
	}
}
