package doctor

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

// Section is one titled block of doctor's report.
type Section struct {
	Title string
	Lines []Line
	// Footnote is a short aside under the lines, drawn dim.
	Footnote string
}

// Line is one entry of a section: a verdict mark, an optional key, its text, and a quieter
// note beside or under it. Depth nests it under the line before.
type Line struct {
	Mark  Mark
	Key   string
	Text  string
	Note  string
	Depth int
	// Quiet draws the text dim, as an aside to the line above.
	Quiet bool
}

// Mark is the glyph a line opens with.
type Mark int

// The marks: none, and one per check status.
const (
	NoMark Mark = iota
	MarkOK
	MarkWarn
	MarkFail
	MarkSkip
)

// markOf is the mark for a check's status.
func markOf(s Status) Mark {
	switch s {
	case Pass:
		return MarkOK
	case Warn:
		return MarkWarn
	case Fail:
		return MarkFail
	}
	return MarkSkip
}

// NameWidth is the column check names pad to, so a streamed report lines up.
const NameWidth = 24

// keyWidth is the column a key/value line's key pads to.
const keyWidth = 12

// Printer draws doctor's report to a writer in its palette, wrapping to Cols (0: no wrap).
type Printer struct {
	W    io.Writer
	P    style.Palette
	Cols int
	// started says something was printed, so the next title needs a blank line first.
	started bool
}

// NewPrinter draws to w, coloured and wrapped only on a terminal.
func NewPrinter(w io.Writer) *Printer {
	return &Printer{W: w, P: style.For(w), Cols: style.Columns(w)}
}

func (pr *Printer) glyph(m Mark) string {
	switch m {
	case MarkOK:
		return pr.P.OK("✓")
	case MarkWarn:
		return pr.P.Warn("!")
	case MarkFail:
		return pr.P.Fail("✗")
	case MarkSkip:
		return pr.P.Dim("–")
	}
	return " "
}

// Title starts a section.
func (pr *Printer) Title(t string) {
	if pr.started {
		fmt.Fprintln(pr.W)
	}
	pr.started = true
	fmt.Fprintln(pr.W, pr.P.Bold(t))
}

// Sections draws each section in turn.
func (pr *Printer) Sections(ss []Section) {
	for _, s := range ss {
		pr.Title(s.Title)
		for _, l := range s.Lines {
			pr.Line(l)
		}
		if s.Footnote != "" {
			pr.block("    ", "    ", s.Footnote, pr.P.Dim)
		}
	}
}

// Line draws l: its text after the mark, wrapped with a hanging indent, and its note on
// the same line when both fit, else beneath the text.
func (pr *Printer) Line(l Line) {
	lead := "  " + pr.glyph(l.Mark) + " " + strings.Repeat("   ", l.Depth)
	hang := strings.Repeat(" ", 4+3*l.Depth)
	if l.Key != "" {
		lead += pr.P.Dim(pad(l.Key, keyWidth)) + " "
		hang += strings.Repeat(" ", keyWidth+1)
	}
	if l.Quiet {
		pr.block(lead, hang, l.Text, pr.P.Dim)
		return
	}
	pr.wrapped(lead, hang, l.Text, l.Note)
}

// Check draws one check's line. A warning's or failure's fix waits for Summary; a skip's,
// which is only information, is drawn under it.
func (pr *Printer) Check(c Check) {
	pr.started = true
	lead := "  " + pr.glyph(markOf(c.Status)) + " " + pad(c.Name, NameWidth) + "  "
	hang := strings.Repeat(" ", 4+NameWidth+2)
	detail := pr.P.Commands
	if c.Status == Skip {
		detail = pr.P.Dim
	}
	lines := style.Wrap(c.Detail, pr.width(hang))
	fmt.Fprintln(pr.W, lead+detail(lines[0]))
	for _, l := range lines[1:] {
		fmt.Fprintln(pr.W, hang+detail(l))
	}
	if c.Status == Skip && c.Fix != "" {
		pr.block(hang, hang, "→ "+c.Fix, pr.P.Dim)
	}
}

// Summary closes the report: what needs the developer, each with its fix on a line of its
// own, or that setup is ready.
func (pr *Printer) Summary(r Report) {
	type action struct {
		fix   string
		mark  Mark
		names []string
	}
	var actions []*action
	skipped := false
	for _, c := range r.Checks {
		if c.Status == Skip && (c.Key == KeyBackend || c.Key == KeyProject) {
			skipped = true
		}
		if c.Status == Pass || c.Status == Skip {
			continue
		}
		fix := strings.TrimSpace(c.Fix)
		if fix == "" {
			fix = strings.TrimSpace(c.Detail)
		}
		i := slices.IndexFunc(actions, func(a *action) bool { return a.fix == fix })
		if i < 0 {
			actions = append(actions, &action{fix: fix})
			i = len(actions) - 1
		}
		a := actions[i]
		a.mark = max(a.mark, markOf(c.Status))
		if c.Name != "" && !slices.Contains(a.names, c.Name) {
			a.names = append(a.names, c.Name)
		}
	}
	if len(actions) == 0 {
		if pr.started {
			fmt.Fprintln(pr.W)
		}
		if skipped {
			fmt.Fprintf(pr.W, "%s %s: the checks that ran passed, and some were skipped.\n", pr.glyph(MarkSkip), pr.P.Bold("Verification incomplete"))
		} else {
			fmt.Fprintf(pr.W, "%s %s: every check passed.\n", pr.glyph(MarkOK), pr.P.Bold("Setup ready"))
		}
		return
	}
	pr.Title("Setup needs attention")
	for _, a := range actions {
		if len(a.names) == 0 {
			pr.fix("  "+pr.glyph(a.mark)+" ", "    ", a.fix)
			continue
		}
		fmt.Fprintf(pr.W, "  %s %s\n", pr.glyph(a.mark), strings.Join(a.names, ", "))
		pr.fix("    ", "    ", a.fix)
	}
}

// fix draws a fix: a command alone on its line, whole, so it copies as it is; prose
// wrapped, its quoted commands drawn as commands.
func (pr *Printer) fix(lead, hang, fix string) {
	if first, rest, ok := strings.Cut(fix, "\n"); ok {
		pr.fix(lead, hang, first)
		pr.fix(hang, hang, rest)
		return
	}
	if cmd, ok := bareCommand(fix); ok {
		fmt.Fprintln(pr.W, lead+pr.P.Command(cmd))
		return
	}
	lines := style.Wrap(fix, pr.width(hang))
	fmt.Fprintln(pr.W, lead+fixText(pr.P, lines[0]))
	for _, l := range lines[1:] {
		fmt.Fprintln(pr.W, hang+pr.P.Commands(l))
	}
}

// bareCommand is fix when it is a command and nothing else: wholly quoted, or a terma
// command with no aside.
func bareCommand(fix string) (string, bool) {
	if inner, ok := strings.CutPrefix(fix, "`"); ok && strings.HasSuffix(inner, "`") && !strings.Contains(inner[:len(inner)-1], "`") {
		return inner[:len(inner)-1], true
	}
	if strings.HasPrefix(fix, "terma ") && !strings.ContainsAny(fix, "(—;`") {
		return fix, true
	}
	return "", false
}

// wrapped draws text after lead, continuing at hang, with note beside it when it fits on
// the last line and beneath it otherwise.
func (pr *Printer) wrapped(lead, hang, text, note string) {
	width := pr.width(hang)
	lines := style.Wrap(text, width)
	last := len(lines) - 1
	beside := note != "" && (width <= 0 || runes(lines[last])+2+runes(note) <= width)
	for i, l := range lines {
		prefix := hang
		if i == 0 {
			prefix = lead
		}
		l = pr.P.Commands(l)
		if i == last && beside {
			l += "  " + pr.P.Dim(note)
		}
		fmt.Fprintln(pr.W, prefix+l)
	}
	if note != "" && !beside {
		pr.block(hang, hang, note, pr.P.Dim)
	}
}

// block draws text after lead, wrapped at hang, every line in draw.
func (pr *Printer) block(lead, hang, text string, draw func(string) string) {
	for i, l := range style.Wrap(text, pr.width(hang)) {
		fmt.Fprintln(pr.W, pick(i == 0, lead, hang)+draw(l))
	}
}

// width is what is left of a line after indent, or 0 when nothing wraps.
func (pr *Printer) width(indent string) int {
	if pr.Cols <= 0 {
		return 0
	}
	return max(pr.Cols-runes(indent), 20)
}

func runes(s string) int { return utf8.RuneCountInString(s) }

// pad right-pads s with spaces to n columns.
func pad(s string, n int) string { return s + strings.Repeat(" ", max(n-runes(s), 0)) }

// RenderCheck prints one check's line.
func RenderCheck(w io.Writer, c Check) { NewPrinter(w).Check(c) }

// RenderSummary names the remaining setup actions, without claiming measured coverage.
func RenderSummary(w io.Writer, r Report) {
	(&Printer{W: w, P: style.For(w), Cols: style.Columns(w), started: true}).Summary(r)
}

// fixText draws the terma command a fix leads with, and any quoted in it, as commands.
func fixText(p style.Palette, fix string) string {
	if !strings.HasPrefix(fix, "terma ") {
		return p.Commands(fix)
	}
	head, tail := fix, ""
	if i := strings.IndexAny(fix, "(—;"); i > 0 {
		head = strings.TrimRight(fix[:i], " ")
		tail = fix[len(head):]
	}
	return p.Command(head) + p.Commands(tail)
}
