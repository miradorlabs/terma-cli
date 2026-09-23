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
