package cli

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/install"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

// installUI reports install's steps, verdict and next steps; successful steps and details
// print only under --verbose.
type installUI struct {
	out    io.Writer
	detail io.Writer
	p      style.Palette
	warned bool
	// lines are the checklist, printed together at finish so prompts never split it.
	lines []string
	next  []string
	// title closes a run that needs nothing more; warnTitle, when set, one that does.
	title, warnTitle string
}

func newInstallUI(out io.Writer, verbose bool) *installUI {
	detail := io.Discard
	if verbose {
		detail = out
	}
	return &installUI{out: out, detail: detail, p: style.For(out), title: "terma installed"}
}

const stepLabelWidth = 13

// OK reports a step that did what it should.
func (u *installUI) OK(label, what string) { u.line(u.detail, u.p.OK("✓"), label, what) }

// Summary keeps user-facing choices visible without exposing setup internals.
func (u *installUI) Summary(label, what string) { u.keep(u.p.OK("✓"), label, what) }

// Warn reports a step that needs the developer, whose fix is a next step.
func (u *installUI) Warn(label, what string) {
	u.warned = true
	u.keep(u.p.Warn("!"), label, what)
}

func (u *installUI) keep(mark, label, what string) {
	u.lines = append(u.lines, fmt.Sprintf("  %s %-*s %s", mark, stepLabelWidth, label, u.p.Commands(what)))
}

// Commit adds the step that commits paths, led by why.
func (u *installUI) Commit(lead string, paths []string) { u.Then(commitList(u.p, lead, paths)) }

// Detail takes the long form, shown under --verbose or --dry-run.
func (u *installUI) Detail() io.Writer { return u.detail }

var _ install.Reporter = (*installUI)(nil)

func (u *installUI) line(out io.Writer, mark, label, what string) {
	fmt.Fprintf(out, "  %s %-*s %s\n", mark, stepLabelWidth, label, u.p.Commands(what))
}

// Then adds a step left for the developer, once.
func (u *installUI) Then(step string) {
	if !slices.Contains(u.next, step) {
		u.next = append(u.next, step)
	}
}

// printLines prints the checklist so far.
func (u *installUI) printLines() {
	if len(u.lines) > 0 {
		fmt.Fprintln(u.out)
		for _, l := range u.lines {
			fmt.Fprintln(u.out, l)
		}
	}
	u.lines = nil
}

func (u *installUI) finish() {
	u.printLines()
	if u.warned {
		fmt.Fprintf(u.out, "\n%s %s\n", u.p.Warn("!"), u.p.Bold(cmp.Or(u.warnTitle, u.title+" — the steps marked ! need you")))
	} else {
		fmt.Fprintf(u.out, "\n%s %s\n", u.p.OK("✓"), u.p.Bold(u.title))
	}
	if len(u.next) == 0 {
		return
	}
	fmt.Fprintf(u.out, "\n%s\n", u.p.Bold("Next steps:"))
	for _, step := range u.next {
		lines := strings.Split(u.p.Commands(step), "\n")
		fmt.Fprintf(u.out, "  %s %s\n", u.p.Brand("•"), lines[0])
		for _, l := range lines[1:] {
			fmt.Fprintf(u.out, "    %s\n", l)
		}
	}
}

// commitList is the next step that commits paths, led by lead; terma's own directory is
// added whole.
func commitList(p style.Palette, lead string, paths []string) string {
	var unique []string
	for _, path := range paths {
		if strings.HasPrefix(path, termaproject.Dir+"/") {
			path = termaproject.Dir
		}
		if !slices.Contains(unique, path) {
			unique = append(unique, path)
		}
	}
	return lead + "\n  " + p.Command("git add "+strings.Join(unique, " "))
}
