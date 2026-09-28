package style

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// Only a terminal gets colour. A buffer is what every test captures, and what the
// strings they compare against are written for.
func TestForABufferIsPlain(t *testing.T) {
	p := For(&bytes.Buffer{})
	if p.Enabled() {
		t.Fatal("a buffer is not a terminal")
	}
	for _, got := range []string{p.Brand("x"), p.Bold("x"), p.Dim("x"), p.OK("x"), p.Warn("x"), p.Fail("x")} {
		if got != "x" {
			t.Fatalf("plain palette altered text: %q", got)
		}
	}
	if Terminal(&bytes.Buffer{}) {
		t.Fatal("a buffer is not a terminal")
	}
}

func TestNoColorAndAgentsDisableColour(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if enabled(os.Stdout) {
		t.Fatal("NO_COLOR must win even on a terminal")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLAUDECODE", "1")
	if enabled(os.Stdout) || Terminal(os.Stdout) {
		t.Fatal("an agent driving the CLI reads text, not escape sequences")
	}
}

func TestBrandSequenceFollowsTheTerminalsDepth(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	if !strings.Contains(brandSequence(), "38;2;139;108;255") {
		t.Fatalf("truecolor should get the exact purple: %q", brandSequence())
	}
	t.Setenv("COLORTERM", "")
	t.Setenv("TERM", "xterm-256color")
	if brandSequence() != brand256 {
		t.Fatalf("256-colour terminals get the nearest index: %q", brandSequence())
	}
	t.Setenv("TERM", "vt100")
	if brandSequence() != brandBasic {
		t.Fatalf("anything else gets a basic colour: %q", brandSequence())
	}
}

func TestHeaderLaysInfoBesideTheLogo(t *testing.T) {
	info := []string{"Terma CLI", "dawson@mirador.org  (Terma Dev · Org: Mirador Dev)", "~/Projects/Mirador/terma-cli"}
	h := Header(Plain(), info)
	if strings.Contains(h, "\x1b[") {
		t.Fatal("a plain header must carry no escape sequences")
	}
	for _, want := range append([]string{"\u2588"}, info...) {
		if !strings.Contains(h, want) {
			t.Fatalf("header missing %q:\n%s", want, h)
		}
	}
	t.Logf("\n%s", h) // eyeball with -v
}

// A quoted command is drawn as one on a terminal, its backticks dropped so a copy is the
// command alone; plain text keeps the quotes that mark it.
func TestCommandsDrawsQuotedCommands(t *testing.T) {
	const msg = "Run `source ~/.zshrc` in this terminal, then `terma update --refresh`."
	if got := Plain().Commands(msg); got != msg {
		t.Fatalf("plain text changed: %q", got)
	}
	p := Palette{enabled: true, brand: brandBasic}
	want := "Run " + bold + brandBasic + "source ~/.zshrc" + reset + " in this terminal, then " +
		bold + brandBasic + "terma update --refresh" + reset + "."
	if got := p.Commands(msg); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if got := p.Commands("a lone ` backtick\nand `another`"); !strings.Contains(got, "a lone ` backtick") {
		t.Fatalf("an unmatched backtick is left alone: %q", got)
	}
}

// Highlight is the writer itself when the writer gets plain text, and a styled writer
// keeps answering For and Terminal about the writer underneath.
func TestHighlight(t *testing.T) {
	var buf bytes.Buffer
	if w := Highlight(&buf); w != &buf {
		t.Fatal("a buffer gets plain text, so Highlight has nothing to do")
	}
	h := highlighter{w: &buf, p: Palette{enabled: true, brand: brandBasic}}
	if !For(h).Enabled() || Terminal(h) {
		t.Fatal("For reads the highlighter's palette; Terminal asks about the writer beneath")
	}
	if _, err := h.Write([]byte("run `terma doctor`\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); strings.Contains(got, "`") || !strings.Contains(got, "terma doctor") {
		t.Fatalf("written through the highlighter: %q", got)
	}
}
