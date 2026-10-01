package cmd

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/install"
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

// OK reports a step that did what it should.
func (u *installUI) OK(label, what string) { u.line(u.detail, u.p.OK("✓"), label, what) }

// summary keeps user-facing choices visible without exposing setup internals.
func (u *installUI) summary(label, what string) { u.line(u.out, u.p.OK("✓"), label, what) }

// Warn reports a step that needs the developer, whose fix is a next step.
func (u *installUI) Warn(label, what string) {
	u.warned = true
	u.line(u.out, u.p.Warn("!"), label, what)
}

// Commit adds the step that commits paths, led by why.
func (u *installUI) Commit(lead string, paths []string) { u.Then(commitList(u.p, lead, paths)) }

// Detail takes the long form, shown under --verbose or --dry-run.
func (u *installUI) Detail() io.Writer { return u.detail }

var _ install.Reporter = (*installUI)(nil)

func (u *installUI) line(out io.Writer, mark, label, what string) {
	fmt.Fprintf(out, "  %s %-*s %s\n", mark, stepLabelWidth, label, u.p.Commands(what))
}

// Then adds a step left for the developer. Lines after the first keep their own
// indentation under the step's number.
func (u *installUI) Then(step string) {
	if !slices.Contains(u.next, step) {
		u.next = append(u.next, step)
	}
}

// finish prints the verdict and the next steps.
func (u *installUI) finish() {
	if u.warned {
		fmt.Fprintf(u.out, "\n%s %s\n", u.p.Warn("!"), u.p.Bold("terma installed — the steps marked ! need you"))
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
// steps.
func (u *installUI) verify(cmd *cobra.Command, runDoctor func(context.Context, bool, doctor.Progress) doctor.Report) {
	fmt.Fprintf(u.detail, "\n%s\n", u.p.Bold("Verifying the chain (terma doctor):"))
	sp := spinner.New(cmd.ErrOrStderr())
	report := runDoctor(cmd.Context(), false, doctor.Progress{
		Start: func(name string) {
			if u.detail == io.Discard {
				sp.Start("Verifying installation…")
			} else {
				sp.Start("Verifying: " + name + "…")
			}
		},
		Note: func(note string) {
			if u.detail != io.Discard {
				sp.Update(note)
			}
		},
		Done: func(c doctor.Check) {
			sp.Stop()
			doctor.RenderCheck(u.detail, c, doctor.NameWidth)
		},
	})
	sp.Stop()
	u.verdict(report)
}

// verdict reports a doctor run as install's Verified step: every problem's fix becomes a
// next step (its name and detail when it names no fix), and the full report is one
// command away.
func (u *installUI) verdict(report doctor.Report) {
	var fixes []string
	skipped := false
	for _, c := range report.Checks {
		switch c.Status {
		case doctor.Pass:
		case doctor.Skip:
			skipped = skipped || c.Key == doctor.KeyScratch || c.Key == doctor.KeyBackend || c.Key == doctor.KeyProject
		default:
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
		u.Warn("Verified", fmt.Sprintf("terma doctor found %d thing(s) to fix", len(fixes)))
		for _, f := range fixes {
			u.Then(f)
		}
		u.Then("Run `terma doctor` for the full report.")
	case skipped:
		u.OK("Verified", "terma doctor: the checks that ran passed; some were skipped")
	default:
		u.OK("Verified", "terma doctor: all checks passed")
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

// commitList is the next step that names the committed files an install or refresh
// wrote: the hooks do nothing for a colleague until the files are merged. lead says why.
// A path is listed once, even when two changes touched it, and the `git add` that
// commits them is drawn in p as a command.
func commitList(p style.Palette, lead string, paths []string) string {
	var unique []string
	for _, p := range paths {
		if !slices.Contains(unique, p) {
			unique = append(unique, p)
		}
	}
	var b strings.Builder
	b.WriteString(lead)
	for _, p := range unique {
		b.WriteString("\n  " + p)
	}
	b.WriteString("\n\n  " + p.Command("git add "+strings.Join(unique, " ")))
	return b.String()
}
