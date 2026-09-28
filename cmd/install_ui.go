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

// installUI is how install reports: one marked line per step as it finishes, then the
// verdict, then what is left for the developer, numbered. The long-form account of each
// step — the files, the keys, the policies — goes to detail, which is the same writer
// under --verbose and nowhere otherwise: a developer who wants to know whether install
// worked should not have to read how.
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
func (u *installUI) ok(label, what string) { u.line(u.p.OK("✓"), label, what) }

// warn reports a step that needs the developer, whose fix is a next step.
func (u *installUI) warn(label, what string) {
	u.warned = true
	u.line(u.p.Warn("!"), label, what)
}

func (u *installUI) line(mark, label, what string) {
	fmt.Fprintf(u.out, "  %s %-*s %s\n", mark, stepLabelWidth, label, what)
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
		fmt.Fprintf(u.out, "\n%s %s\n", u.p.Warn("!"), u.p.Bold("terma installed — the steps marked ! need you"))
	} else {
		fmt.Fprintf(u.out, "\n%s %s\n", u.p.OK("✓"), u.p.Bold("terma installed"))
	}
	if len(u.next) == 0 {
		return
	}
	fmt.Fprintf(u.out, "\n%s\n", u.p.Bold("Next steps:"))
	for i, step := range u.next {
		lines := strings.Split(step, "\n")
		fmt.Fprintf(u.out, "  %d. %s\n", i+1, lines[0])
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
// steps. The shell-routing warning is left out when a next step already says to reload
// the shell: install ran in a shell that predates the PATH block, so doctor, running in
// the same process, cannot see the shims yet — that is the step, not a second problem.
func (u *installUI) verify(cmd *cobra.Command) {
	fmt.Fprintf(u.detail, "\n%s\n", u.p.Bold("Verifying the chain (terma doctor):"))
	sp := spinner.New(cmd.ErrOrStderr())
	report := runDoctor(cmd.Context(), false, doctorProgress{
		start: func(name string) { sp.Start("Verifying: " + name + "…") },
		note:  sp.Update,
		done: func(c doctor.Check) {
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
		switch {
		case c.Status == doctor.Pass:
		case c.Status == doctor.Skip:
			skipped = skipped || c.Key == doctor.KeyScratch || c.Key == doctor.KeyBackend || c.Key == doctor.KeyProject
		case c.Key == doctor.KeyRouting && u.reloading:
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
		u.warn("Verified", fmt.Sprintf("terma doctor found %d thing(s) to fix", len(fixes)))
		for _, f := range fixes {
			u.then(f)
		}
		u.then("Run `terma doctor` for the full report.")
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

// commitList is the next step that names the committed files an install or refresh
// wrote: the hooks do nothing for a colleague until the files are merged. lead says why.
// A path is listed once, even when two changes touched it.
func commitList(lead string, paths []string) string {
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
	b.WriteString("\n\n  git add " + strings.Join(unique, " "))
	return b.String()
}
