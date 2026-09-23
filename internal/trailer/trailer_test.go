package trailer

import (
	"strings"
	"testing"
)

const template = `

# Please enter the commit message for your changes. Lines starting
# with '#' will be ignored, and an empty message aborts the commit.
#
# On branch main
# Changes to be committed:
#	modified:   src/a.go
#
`

func TestStampOntoPlainMessage(t *testing.T) {
	msg := "Fix the flaky test\n\nIt raced the ticker.\n" + template
	out, changed := Stamp(msg, []Trailer{{SessionID: "s1", Tool: "claude-code/2.1"}}, "#")
	if !changed {
		t.Fatal("expected a change")
	}
	want := "Fix the flaky test\n\nIt raced the ticker.\n\nAgent-Session-Id: s1\nAgent-Tool: claude-code/2.1\n"
	if !strings.HasPrefix(out, want) {
		t.Fatalf("unexpected message:\n%s", out)
	}
	if !strings.Contains(out, "# On branch main") {
		t.Fatal("comment block was dropped")
	}
	if idx := strings.Index(out, "Agent-Session-Id"); idx > strings.Index(out, "# Please enter") {
		t.Fatal("trailers landed below the comments")
	}
}

func TestStampJoinsExistingTrailerBlock(t *testing.T) {
	msg := "Add thing\n\nCo-authored-by: A <a@x.io>\nSigned-off-by: B <b@x.io>\n"
	out, _ := Stamp(msg, []Trailer{{SessionID: "s1"}}, "#")
	want := "Add thing\n\nCo-authored-by: A <a@x.io>\nSigned-off-by: B <b@x.io>\nAgent-Session-Id: s1\n"
	if out != want {
		t.Fatalf("got:\n%q\nwant:\n%q", out, want)
	}
}

func TestStampIsIdempotent(t *testing.T) {
	msg := "Add thing\n\nAgent-Session-Id: s1\nAgent-Tool: codex/1.0\n"
	out, changed := Stamp(msg, []Trailer{{SessionID: "s1", Tool: "codex/1.0"}}, "#")
	if changed || out != msg {
		t.Fatalf("expected no change, got changed=%v:\n%s", changed, out)
	}
	out, changed = Stamp(msg, []Trailer{{SessionID: "s1"}, {SessionID: "s2"}}, "#")
	if !changed || !strings.HasSuffix(out, "Agent-Session-Id: s2\n") {
		t.Fatalf("second session not appended:\n%s", out)
	}
	if strings.Count(out, "Agent-Session-Id: s1") != 1 {
		t.Fatal("first session duplicated")
	}
}

func TestStampEmptyTemplateKeepsCommentsBelow(t *testing.T) {
	out, changed := Stamp(template, []Trailer{{SessionID: "s1"}}, "#")
	if !changed {
		t.Fatal("expected a change")
	}
	if !strings.HasPrefix(out, "Agent-Session-Id: s1\n") {
		t.Fatalf("trailer should open the message:\n%s", out)
	}
	if !strings.Contains(out, "# Please enter the commit message") {
		t.Fatal("template comments lost")
	}
}

func TestStampRespectsScissors(t *testing.T) {
	msg := "Subject\n\n# ------------------------ >8 ------------------------\n# Do not modify or remove the line above.\ndiff --git a/x b/x\n"
	out, _ := Stamp(msg, []Trailer{{SessionID: "s1"}}, "#")
	sIdx := strings.Index(out, "Agent-Session-Id")
	cIdx := strings.Index(out, ">8")
	if sIdx == -1 || cIdx == -1 || sIdx > cIdx {
		t.Fatalf("trailer must precede the scissors line:\n%s", out)
	}
}

func TestStampOneLineConventionalSubjectIsNotATrailerBlock(t *testing.T) {
	out, _ := Stamp("fix: handle nil\n", []Trailer{{SessionID: "s1"}}, "#")
	if out != "fix: handle nil\n\nAgent-Session-Id: s1\n" {
		t.Fatalf("subject treated as trailer block:\n%q", out)
	}
}

func TestStampCustomCommentChar(t *testing.T) {
	msg := "Subject\n\n; comment line\n"
	out, _ := Stamp(msg, []Trailer{{SessionID: "s1"}}, ";")
	if !strings.HasPrefix(out, "Subject\n\nAgent-Session-Id: s1\n") || !strings.Contains(out, "; comment line") {
		t.Fatalf("unexpected:\n%s", out)
	}
}

func TestParsePairsToolWithSession(t *testing.T) {
	msg := "Subject\n\nAgent-Session-Id: s1\nAgent-Tool: claude-code/2\nAgent-Session-Id: s2\n# Agent-Session-Id: ignored\n"
	got := Parse(msg, "#")
	if len(got) != 2 || got[0] != (Trailer{SessionID: "s1", Tool: "claude-code/2"}) || got[1] != (Trailer{SessionID: "s2"}) {
		t.Fatalf("unexpected parse: %+v", got)
	}
}
