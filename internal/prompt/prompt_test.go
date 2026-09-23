package prompt

import (
	"bytes"
	"github.com/miradorlabs/terma-cli/internal/style"
	"strings"
	"testing"
)

func form() *Form {
	return &Form{
		Title: "What should Claude Code send?",
		Items: []Item{
			{Label: "Traces", Selected: true, Heading: "Data"},
			{Label: "Events", Selected: true},
			{Label: "Metrics", Selected: true},
			{Label: "Global", Kind: Radio, Group: 1, Selected: true, Heading: "Where"},
			{Label: "Local", Kind: Radio, Group: 1},
		},
	}
}

func selected(f *Form) []bool {
	out := make([]bool, len(f.Items))
	for i, it := range f.Items {
		out[i] = it.Selected
	}
	return out
}

func drive(f *Form, keys ...key) (done, cancelled bool) {
	f.init()
	for _, k := range keys {
		if done, cancelled = f.handle(k); done || cancelled {
			return
		}
	}
	return
}

func TestSpaceTogglesACheckbox(t *testing.T) {
	f := form()
	drive(f, keyToggle, keyDown, keyToggle, keyToggle)
	if got := selected(f); got[0] || !got[1] {
		t.Fatalf("selected = %v, want traces off and events back on", got)
	}
}

func TestRadioIsExclusiveAndNeverEmpty(t *testing.T) {
	f := form()
	drive(f, keyDown, keyDown, keyDown, keyDown, keyToggle)
	if got := selected(f); got[3] || !got[4] {
		t.Fatalf("selected = %v, want local chosen and global cleared", got)
	}
	// Toggling the chosen radio again keeps it: one of the group is always selected.
	f.handle(keyToggle)
	if got := selected(f); got[3] || !got[4] {
		t.Fatalf("selected = %v after re-toggle, want unchanged", got)
	}
}

func TestAllAndNoneTouchOnlyCheckboxes(t *testing.T) {
	f := form()
	drive(f, keyClear)
	if got := selected(f); got[0] || got[1] || got[2] || !got[3] {
		t.Fatalf("after n: %v, want every checkbox off and the radio untouched", got)
	}
	f.handle(keyAll)
	if got := selected(f); !got[0] || !got[1] || !got[2] || !got[3] || got[4] {
		t.Fatalf("after a: %v, want every checkbox on and the radio untouched", got)
	}
}

func TestCursorSkipsDisabledItemsAndStopsAtTheEnds(t *testing.T) {
	f := form()
	f.Items[4].Disabled = true
	f.init()
	for range 10 {
		f.handle(keyDown)
	}
	if f.cursor != 3 {
		t.Fatalf("cursor = %d, want 3 — the disabled last item is not a landing spot", f.cursor)
	}
	for range 10 {
		f.handle(keyUp)
	}
	if f.cursor != 0 {
		t.Fatalf("cursor = %d, want 0", f.cursor)
	}
	// Space on a disabled item, if the cursor were ever there, changes nothing.
	f.toggle(4)
	if f.Items[4].Selected {
		t.Fatal("a disabled item must not toggle")
	}
}

func TestInitLandsOnTheFirstEnabledItem(t *testing.T) {
	f := form()
	f.Items[0].Disabled = true
	f.init()
	if f.cursor != 1 {
		t.Fatalf("cursor = %d, want 1", f.cursor)
	}
}

func TestEnterFinishesAndEscapeCancels(t *testing.T) {
	f := form()
	if done, cancelled := drive(f, keyEnter); !done || cancelled {
		t.Fatalf("enter: done=%v cancelled=%v", done, cancelled)
	}
	f = form()
	if done, cancelled := drive(f, keyCancel); done || !cancelled {
		t.Fatalf("cancel: done=%v cancelled=%v", done, cancelled)
	}
}

func TestDecodeKeys(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want key
	}{
		{"\x1b[A", keyUp}, {"\x1b[B", keyDown}, {"k", keyUp}, {"j", keyDown},
		{" ", keyToggle}, {"x", keyToggle}, {"\r", keyEnter}, {"\n", keyEnter},
		{"a", keyAll}, {"n", keyClear},
		{"\x1b", keyCancel}, {"q", keyCancel}, {"\x03", keyCancel}, {"\x04", keyCancel},
		{"z", keyNone}, {"\x1b[C", keyNone}, {"", keyNone},
	} {
		if got := decode([]byte(tc.in)); got != tc.want {
			t.Errorf("decode(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The frame is what the user reads; the line count is what the redraw backs up over.
// The two have to agree or every keypress smears the form down the screen.
func TestRenderCountsEveryLineItWrites(t *testing.T) {
	f := form()
	f.Items[4].Disabled, f.Items[4].Reason = true, "Codex has no repository settings"
	f.init()
	var buf bytes.Buffer
	lines := f.render(&buf, style.Plain())
	got := strings.Count(buf.String(), "\r\n")
	if got != lines {
		t.Fatalf("render reported %d lines, wrote %d", lines, got)
	}
	out := buf.String()
	for _, want := range []string{
		"What should Claude Code send?",
		"❯ [x] Traces",
		"  [x] Events",
		"  (•) Global",
		"  ( ) Local     Codex has no repository settings",
		"  Data", "  Where",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[2m") {
		t.Error("colour disabled, yet the frame carries SGR sequences")
	}
	if !strings.Contains(out, clearLine) {
		t.Error("every line must be cleared before it is redrawn")
	}
}

// A line wider than the terminal wraps onto several physical rows. The redraw backs up
// over physical rows, so render must count them — otherwise every up/down keypress
// smears a fresh copy of the form down the screen. Guards that reported bug.
func TestRenderCountsWrappedPhysicalRows(t *testing.T) {
	f := &Form{
		Title: strings.Repeat("x", 45),
		Items: []Item{{Label: strings.Repeat("y", 45), Kind: Check}},
	}
	f.init()

	f.termWidth = 20
	var buf bytes.Buffer
	lines := f.render(&buf, style.Plain())
	if logical := strings.Count(buf.String(), "\r\n"); lines <= logical {
		t.Fatalf("wrapped render must count physical rows: reported %d, logical lines %d", lines, logical)
	}

	// With an unknown width it is one row per line — what the model tests assume.
	f.termWidth = 0
	buf.Reset()
	if got, logical := f.render(&buf, style.Plain()), strings.Count(buf.String(), "\r\n"); got != logical {
		t.Fatalf("unknown width must count one row per line: reported %d, logical %d", got, logical)
	}
}

// A paste, or a pty that batches, delivers several keys in one read. Every one of them
// has to count, in order.
func TestTokensSplitsBatchedInput(t *testing.T) {
	got := tokens([]byte("\x1b[B \x1b[A\r"))
	want := []string{"\x1b[B", " ", "\x1b[A", "\r"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Errorf("token %d = %q, want %q", i, got[i], want[i])
		}
	}
	// A bare escape at the end is a key of its own, not a truncated sequence.
	if got := tokens([]byte("x\x1b")); len(got) != 2 || decode(got[1]) != keyCancel {
		t.Fatalf("trailing escape: %q", got)
	}
}
