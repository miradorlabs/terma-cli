// Package style is the one place the CLI decides whether, and how, to colour what it
// prints. Every command asks it for a Palette bound to the writer it is about to use,
// and gets back either ANSI-wrapped text or the text untouched.
//
// Colour is a terminal courtesy, never part of the output: a pipe, a file, an agent
// harness, NO_COLOR, and TERM=dumb all get plain text, and tests that capture output in
// a buffer see exactly the strings they compare against.
package style

import (
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// Palette wraps text in the CLI's colours, or leaves it alone when disabled.
type Palette struct {
	enabled bool
	// brand is the escape that selects terma's purple, chosen for the terminal's
	// colour depth so the mark looks the same in a truecolor terminal and a 256-colour
	// one instead of falling back to whatever the theme calls magenta.
	brand string
}

// For returns the palette for w: coloured when w is a terminal a person is reading,
// plain for anything else. Only *os.File writers can be terminals; a buffer never is.
func For(w io.Writer) Palette {
	f, ok := w.(*os.File)
	if !ok || !enabled(f) {
		return Palette{}
	}
	return Palette{enabled: true, brand: brandSequence()}
}

// Plain is the palette that colours nothing, for callers that already know.
func Plain() Palette { return Palette{} }

// Terminal reports whether w is a terminal a person is watching — the gate for
// progress that redraws in place. NO_COLOR does not turn this off: it asks for no
// colour, not for no feedback.
func Terminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || agentMode() || os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// Enabled reports whether this palette colours anything.
func (p Palette) Enabled() bool { return p.enabled }

// enabled is the gate every terminal courtesy in this package runs through.
func enabled(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if agentMode() {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// agentEnvVars mirrors output.AgentMode: a coding agent driving the CLI reads text,
// and escape sequences in it are noise. Duplicated rather than imported so this
// package stays a leaf that output and prompt can both depend on.
var agentEnvVars = []string{
	"CLAUDECODE",
	"CLAUDE_CODE",
	"CURSOR_TRACE_ID",
	"CLINE_ACTIVE",
	"GITHUB_COPILOT_CLI",
	"AIDER_MODEL",
}

func agentMode() bool {
	for _, key := range agentEnvVars {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}

// Terma's purple, #8b6cff, as the web app draws its mark.
const (
	brandTrueColor = "\x1b[38;2;139;108;255m"
	brand256       = "\x1b[38;5;141m"
	brandBasic     = "\x1b[95m"
)

// BrandSequence is the raw escape for the brand colour, chosen for the terminal
// this process was started from. It is for output a program renders on the
// user's behalf without a TTY of its own — Claude Code's status line reads a
// pipe and draws the escapes itself — and it honours NO_COLOR like the palette.
func BrandSequence() string {
	if os.Getenv("NO_COLOR") != "" {
		return ""
	}
	return brandSequence()
}

// Reset is the escape that ends a coloured run.
const Reset = reset

// brandSequence picks the closest rendering the terminal can show.
func brandSequence() string {
	switch strings.ToLower(os.Getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return brandTrueColor
	}
	termName := os.Getenv("TERM")
	if strings.Contains(termName, "256color") || strings.Contains(termName, "direct") || os.Getenv("COLORTERM") != "" {
		return brand256
	}
	return brandBasic
}

const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	red    = "\x1b[31m"
)

func (p Palette) wrap(seq, s string) string {
	if !p.enabled || s == "" {
		return s
	}
	return seq + s + reset
}

// Brand is terma's purple: the mark, a cursor, a highlighted choice.
func (p Palette) Brand(s string) string { return p.wrap(p.brand, s) }

// Bold is for titles and the one line a reader must not miss.
func (p Palette) Bold(s string) string { return p.wrap(bold, s) }

// Dim is for the explanatory text beside a choice, and for what was skipped.
func (p Palette) Dim(s string) string { return p.wrap(dim, s) }

// OK colours a status word for something that passed. OK, Warn and Fail use the colours
// the web app gives the same three states.
func (p Palette) OK(s string) string { return p.wrap(green, s) }

// Warn colours a status word for something that works but wants attention.
func (p Palette) Warn(s string) string { return p.wrap(yellow, s) }

// Fail colours a status word for something that does not work, bold as well as red so
// it survives a terminal theme in which red is hard to see.
func (p Palette) Fail(s string) string { return p.wrap(red+bold, s) }

// logoLines renders the four rounded squares from docs/assets/terma-logo-dark.svg.
// A terminal cell is roughly twice as tall as it is wide, so each six-column tile
// occupies three rows. Half blocks round the corners; the opposite diagonal is dim.
func logoLines(p Palette) (lines []string, width int) {
	tile := []string{"▄████▄", "██████", "▀████▀"}
	const gap = "  "
	for _, row := range tile {
		lines = append(lines, p.Brand(row)+gap+p.Dim(p.Brand(row)))
	}
	width = 2*utf8.RuneCountInString(tile[0]) + len(gap)
	lines = append(lines, strings.Repeat(" ", width))
	for _, row := range tile {
		lines = append(lines, p.Dim(p.Brand(row))+gap+p.Brand(row))
	}
	return lines, width
}

// Header draws the logo with an information column to its right — for `terma setup`,
// "Terma CLI", the signed-in account, and the working directory — vertically centred
// against the logo. On a terminal too narrow to sit them side by side, the column is
// stacked beneath the logo instead. Callers gate it on an Enabled palette.
func Header(p Palette, info []string) string {
	lines, w := logoLines(p)
	widest := 0
	for _, s := range info {
		if n := visibleWidth(s); n > widest {
			widest = n
		}
	}
	cols := terminalCols()
	sideBySide := cols <= 0 || cols >= 2+w+3+widest

	var b strings.Builder
	b.WriteByte('\n')
	if sideBySide {
		start := max((len(lines)-len(info))/2, 0)
		for r, l := range lines {
			b.WriteString("  ")
			b.WriteString(l)
			if i := r - start; i >= 0 && i < len(info) && info[i] != "" {
				b.WriteString("   ")
				b.WriteString(info[i])
			}
			b.WriteByte('\n')
		}
		return b.String()
	}
	for _, l := range lines {
		b.WriteString("  ")
		b.WriteString(strings.TrimRight(l, " "))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	for _, s := range info {
		if s != "" {
			b.WriteString("  ")
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ansiSGR matches the colour escapes the palette emits, so a coloured string's visible
// width can be measured.
var ansiSGR = regexp.MustCompile("\x1b\\[[0-9;]*m")

// visibleWidth is the column count a string occupies, ignoring colour escapes.
func visibleWidth(s string) int {
	return utf8.RuneCountInString(ansiSGR.ReplaceAllString(s, ""))
}

// terminalCols is the width of stdout when it is a terminal, else 0.
func terminalCols() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	return 0
}
