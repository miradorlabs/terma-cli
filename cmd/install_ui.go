package cmd

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/spinner"
	"github.com/miradorlabs/terma-cli/internal/style"
)

// installUI reports the project, capture choice, warnings, result, and next steps.
// Successful setup steps and their details are only printed under --verbose.
type installUI struct {
	out    io.Writer
	detail io.Writer
	p      style.Palette
	warned bool
	next   []string
	// reloading says a next step already tells the developer to reload their shell, so
	// doctor's warning that this shell does not route yet is not news.
	reloading bool
}

func newInstallUI(out io.Writer, verbose bool) *installUI {
	detail := io.Discard
	if verbose {
		detail = out
	}
	return &installUI{out: out, detail: detail, p: style.For(out)}
}

// stepLabelWidth is the column the step labels are padded to, so the details line up.
const stepLabelWidth = 13

// ok reports a step that did what it should.
func (u *installUI) ok(label, what string) { u.line(u.detail, u.p.OK("✓"), label, what) }

// summary keeps user-facing choices visible without exposing setup internals.
func (u *installUI) summary(label, what string) { u.line(u.out, u.p.OK("✓"), label, what) }

// warn reports a step that needs the developer, whose fix is a next step.
func (u *installUI) warn(label, what string) {
	u.warned = true
	u.line(u.out, u.p.Warn("!"), label, what)
}

func (u *installUI) line(out io.Writer, mark, label, what string) {
	fmt.Fprintf(out, "  %s %-*s %s\n", mark, stepLabelWidth, label, u.p.Commands(what))
}

// code draws lines the developer pastes whole — a PATH line, shell functions — the way
// a quoted command is drawn, each indented under its step; a shell comment stays dim.
func (u *installUI) code(block string) string {
	var lines []string
	for l := range strings.SplitSeq(strings.TrimRight(block, "\n"), "\n") {
		l = strings.TrimRight(l, " ")
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			lines = append(lines, "    "+u.p.Dim(l))
		} else {
			lines = append(lines, "    "+u.p.Command(l))
		}
	}
	return strings.Join(lines, "\n")
}

// then adds a step left for the developer. Lines after the first keep their own
// indentation under the step's number.
func (u *installUI) then(step string) {
	if !slices.Contains(u.next, step) {
		u.next = append(u.next, step)
	}
}

// finish prints the verdict and the next steps.
func (u *installUI) finish() {
	if u.warned {
		finish := "terma installed — see the warnings above"
		if len(u.next) > 0 {
			finish = "terma installed — finish with the next steps below"
		}
		fmt.Fprintf(u.out, "\n%s %s\n", u.p.Warn("!"), u.p.Bold(finish))
	} else {
		fmt.Fprintf(u.out, "\n%s %s\n", u.p.OK("✓"), u.p.Bold("terma installed"))
	}
	if len(u.next) == 0 {
		return
	}
	fmt.Fprintf(u.out, "\n%s\n", u.p.Bold("Next steps:"))
	for i, step := range u.next {
		lines := strings.Split(u.p.Commands(step), "\n")
		fmt.Fprintf(u.out, "  %s %s\n", u.p.Brand(fmt.Sprintf("%d.", i+1)), lines[0])
		for _, l := range lines[1:] {
			if l == "" {
				fmt.Fprintln(u.out)
				continue
			}
			fmt.Fprintf(u.out, "     %s\n", l)
		}
	}
}

// verify runs doctor behind a spinner and reports it as one step, its fixes as next
// steps. Warnings solely about shell activation are left out when a next step says to reload
// the shell: install ran in a shell that predates the PATH block, so doctor, running in
// the same process, cannot see the shims yet — that is the step, not a second problem.
func (u *installUI) verify(cmd *cobra.Command) {
	fmt.Fprintf(u.detail, "\n%s\n", u.p.Bold("Verifying the chain (terma doctor):"))
	sp := spinner.New(cmd.ErrOrStderr())
	report := runDoctor(cmd.Context(), false, doctorProgress{
		start: func(name string) {
			if u.detail == io.Discard {
				sp.Start("Verifying installation…")
			} else {
				sp.Start("Verifying: " + name + "…")
			}
		},
		note: func(note string) {
			if u.detail != io.Discard {
				sp.Update(note)
			}
		},
		done: func(c doctor.Check) {
			sp.Stop()
			doctor.RenderCheck(u.detail, c, doctor.NameWidth)
		},
	})
	sp.Stop()
	u.verdict(report)
}

// verdict reports a doctor run as install's Verified step: every problem's fix becomes a
// next step (its name and detail when it names no fix). The Verified line names
// `terma doctor`, which has the full report.
func (u *installUI) verdict(report doctor.Report) {
	var fixes, flagged, consequences []string
	skipped := false
	for _, c := range report.Checks {
		switch {
		case c.Status == doctor.Pass:
		case c.Status == doctor.Skip:
			skipped = skipped || c.Key == doctor.KeyScratch || c.Key == doctor.KeyBackend || c.Key == doctor.KeyProject
		case (c.Key == doctor.KeyRouting || c.NeedsShellActivationOnly) && u.reloading:
		default:
			// An inconclusive check is a consequence of another one; the Verified line
			// names the cause, and the check itself only when nothing else was flagged.
			if c.Inconclusive {
				consequences = append(consequences, c.Name)
			} else {
				flagged = append(flagged, c.Name)
			}
			fix := c.Name + ": " + c.Detail
			if c.Fix != "" {
				fix = doctorFixStep(c.Fix)
			}
			if !slices.Contains(fixes, fix) {
				fixes = append(fixes, fix)
			}
		}
	}
	switch {
	case len(fixes) > 0:
		if len(flagged) == 0 {
			flagged = consequences
		}
		u.warn("Verified", "`terma doctor` flagged "+strings.Join(flagged, ", "))
		for _, f := range fixes {
			u.then(f)
		}
	case skipped:
		u.ok("Verified", "terma doctor: the checks that ran passed; some were skipped")
	default:
		u.ok("Verified", "terma doctor: all checks passed")
	}
}

// doctorFixStep turns a doctor fix into a next step: one that starts with a terma
// command is that command to run, with whatever explains it after; anything else is
// already a sentence.
func doctorFixStep(fix string) string {
	if strings.HasPrefix(fix, "terma ") {
		command, rest := fix, ""
		if i := strings.IndexAny(fix, "(—;"); i > 0 {
			command, rest = strings.TrimSpace(fix[:i]), " "+fix[i:]
		}
		return "Run `" + command + "`" + rest + "."
	}
	return strings.ToUpper(fix[:1]) + fix[1:]
}
