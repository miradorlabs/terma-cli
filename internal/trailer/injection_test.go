package trailer

import (
	"strings"
	"testing"
)

// A value carrying a newline never becomes a second trailer line.
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

// A multi-line tool label is collapsed, keeping the attribution.
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
