// Package style decides whether and how the CLI colours output: only a terminal a
// person watches gets colour, never a pipe, a buffer, an agent, NO_COLOR or TERM=dumb.
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
	// brand is terma's purple at the terminal's colour depth.
	brand string
}

// For returns the palette for w: coloured only when w is a terminal a person is reading.
func For(w io.Writer) Palette {
	if h, ok := w.(highlighter); ok {
		return h.p
	}
	f, ok := w.(*os.File)
	if !ok || !enabled(f) {
		return Palette{}
	}
	return Palette{enabled: true, brand: brandSequence()}
}

// Plain is the palette that colours nothing, for callers that already know.
func Plain() Palette { return Palette{} }

// Terminal reports whether w is a terminal a person is watching, for progress that
// redraws in place; NO_COLOR asks for no colour, not no feedback.
func Terminal(w io.Writer) bool {
	if h, ok := w.(highlighter); ok {
		w = h.w
	}
	f, ok := w.(*os.File)
	if !ok || AgentMode() || os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// Enabled reports whether this palette colours anything.
func (p Palette) Enabled() bool { return p.enabled }

func enabled(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if AgentMode() {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// agentEnvVars are set by coding agents that run CLIs; any of them means the reader is a
// model, to which escape sequences are noise.
var agentEnvVars = []string{
	"CLAUDECODE",
	"CLAUDE_CODE",
	"CURSOR_TRACE_ID",
	"CLINE_ACTIVE",
	"GITHUB_COPILOT_CLI",
	"AIDER_MODEL",
}

// AgentMode reports whether the CLI is being driven by a coding agent.
func AgentMode() bool {
	for _, key := range agentEnvVars {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}

// Light purple, #a78bfa, keeps terminal text readable on dark backgrounds.
const (
	brandTrueColor = "\x1b[38;2;167;139;250m"
	brand256       = "\x1b[38;5;141m"
	brandBasic     = "\x1b[95m"
)

// BrandSequence is the raw brand escape for output another program draws from a pipe,
// such as an agent's status line; it honours NO_COLOR.
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

// OK colours a status word for something that passed.
func (p Palette) OK(s string) string { return p.wrap(green, s) }

// Warn colours a status word for something that works but wants attention.
func (p Palette) Warn(s string) string { return p.wrap(yellow, s) }

// Fail colours a status word for something that does not work, bold for themes where red is faint.
func (p Palette) Fail(s string) string { return p.wrap(red+bold, s) }

// Command is something the reader is meant to run or paste, bold in terma's purple.
func (p Palette) Command(s string) string { return p.wrap(bold+p.brand, s) }

// quoted is a command as every message names one: in backticks, on one line.
var quoted = regexp.MustCompile("`([^`\n]+)`")

// Commands draws every quoted command in s as a Command without its backticks, so a copy
// is the command alone; plain text keeps them.
func (p Palette) Commands(s string) string {
	if !p.enabled {
		return s
	}
	return quoted.ReplaceAllStringFunc(s, func(m string) string { return p.Command(m[1 : len(m)-1]) })
}

// Highlight is w with its quoted commands drawn as Commands; each write is styled on its
// own, so a quoted command must not span two.
func Highlight(w io.Writer) io.Writer {
	p := For(w)
	if !p.enabled {
		return w
	}
	return highlighter{w: w, p: p}
}

type highlighter struct {
	w io.Writer
	p Palette
}

func (h highlighter) Write(b []byte) (int, error) {
	if _, err := io.WriteString(h.w, h.p.Commands(string(b))); err != nil {
		return 0, err
	}
	return len(b), nil
}

// logoLines renders the logo's four rounded squares; a cell is twice as tall as wide,
// so each six-column tile takes three rows.
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

// Header draws the logo with info centred to its right, or beneath it on a narrow terminal.
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

// ansiSGR matches the colour escapes the palette emits.
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
