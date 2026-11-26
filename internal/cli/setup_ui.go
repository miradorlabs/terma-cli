package cli

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
	"golang.org/x/term"
)

// setupUI reports setup's steps, verdict and next steps; successful steps and details
// print only under --verbose.
type setupUI struct {
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

func newSetupUI(out io.Writer, verbose bool) *setupUI {
	detail := io.Discard
	if verbose {
		detail = out
	}
	return &setupUI{out: out, detail: detail, p: style.For(out), title: "Setup complete"}
}

const stepLabelWidth = 13

// OK reports a step that did what it should.
func (u *setupUI) OK(label, what string) { u.line(u.detail, u.p.OK("✓"), label, what) }

// Summary keeps user-facing choices visible without exposing setup internals.
func (u *setupUI) Summary(label, what string) { u.keep(u.p.OK("✓"), label, what) }

// Warn reports a step that needs the developer, whose fix is a next step.
func (u *setupUI) Warn(label, what string) {
	u.warned = true
	u.keep(u.p.Warn("!"), label, what)
}

// Caution reports something setup settled for the developer that they should know of,
// without leaving them a step.
func (u *setupUI) Caution(label, what string) { u.keep(u.p.Warn("!"), label, what) }

// keep adds a checklist line, wrapping what under itself.
func (u *setupUI) keep(mark, label, what string) {
	const indent = 2 + 1 + 1 + stepLabelWidth + 1
	lines := wrapStep(what, cmp.Or(terminalColumns(u.out), 80)-indent)
	u.lines = append(u.lines, fmt.Sprintf("  %s %-*s %s", mark, stepLabelWidth, label, u.p.Commands(lines[0])))
	for _, l := range lines[1:] {
		u.lines = append(u.lines, strings.Repeat(" ", indent)+u.p.Commands(l))
	}
}

// Detail takes the long form, shown under --verbose.
func (u *setupUI) Detail() io.Writer { return u.detail }

func (u *setupUI) line(out io.Writer, mark, label, what string) {
	fmt.Fprintf(out, "  %s %-*s %s\n", mark, stepLabelWidth, label, u.p.Commands(what))
}

// Then adds a step left for the developer, once.
func (u *setupUI) Then(step string) {
	if !slices.Contains(u.next, step) {
		u.next = append(u.next, step)
	}
}

// policyFetched says which team is now in force, and what is left to do about its policy:
// a policy to set up, a list to fill in, or commit hooks to turn on.
func (u *setupUI) policyFetched(team string, pol config.Policy) {
	if team != "" {
		u.Summary("Team", team)
	}
	u.policyNextSteps(pol, !pol.AdmitsNone(), doctor.GitHooksOff(pol, time.Now()))
}

// teamsConfigured summarizes the enrolled organizations and teams.
func (u *setupUI) teamsConfigured(orgs []organization, teams []discoveredTeam) {
	if len(orgs) > 1 {
		u.Summary("Organizations", strings.Join(orgNamesList(orgs), ", "))
	}
	labels := make([]string, 0, len(teams))
	anyAdmits, gitHooksOff := false, false
	now := time.Now()
	for _, dt := range teams {
		switch {
		case dt.team.Personal:
			labels = append(labels, personalLabel)
		case len(orgs) > 1:
			labels = append(labels, orgTeamLabel(dt.orgName, dt.teamName))
		default:
			labels = append(labels, dt.teamName)
		}
		if pol := dt.team.Policy; !pol.AdmitsNone() {
			anyAdmits = true
			gitHooksOff = gitHooksOff || doctor.GitHooksOff(pol, now)
		}
	}
	u.Summary("Teams", strings.Join(labels, ", "))
	u.policyNextSteps(teams[0].team.Policy, anyAdmits, gitHooksOff)
}

// policyNextSteps warns when nothing is collected at all, and otherwise asks for commit
// hooks where a policy has them off.
func (u *setupUI) policyNextSteps(pol config.Policy, admits, gitHooksOff bool) {
	switch {
	case !admits:
		u.Warn("Collects", doctor.PolicySummary(pol))
		u.Then(doctor.NothingCollectedStep(pol))
	case gitHooksOff:
		u.Then(doctor.GitHooksOffStep)
	}
}

// reportConflicts names the selected teams whose policies list the same repositories.
func (u *setupUI) reportConflicts(st routing.State, names teamNames) {
	if line := conflictLine(st, names, len(names.Orgs) > 1); line != "" {
		u.Caution("Policy conflict", line)
		u.Then(conflictStep)
	}
}

// conflictStep points to where each conflict is explained.
const conflictStep = "Run `terma doctor` to see which team collects each repository."

// conflictLine lists the teams in st whose policies overlap, or "" when none do.
func conflictLine(st routing.State, names teamNames, multi bool) string {
	var labels []string
	for _, o := range routing.Overlaps(st) {
		for _, t := range []routing.Team{o.Winner, o.Other} {
			if l := summaryLabel(names, multi, t); !slices.Contains(labels, l) {
				labels = append(labels, l)
			}
		}
	}
	if len(labels) == 0 {
		return ""
	}
	return "between " + strings.Join(labels, ", ")
}

// summaryLabel names t as the Teams line does: with its organization when there are several.
func summaryLabel(names teamNames, multi bool, t routing.Team) string {
	if t.Personal {
		return personalLabel
	}
	name := cmp.Or(names.Teams[t.TeamID], t.TeamID)
	if multi {
		return orgTeamLabel(names.Orgs[t.OrganizationID], name)
	}
	return name
}

func orgNamesList(orgs []organization) []string {
	out := make([]string, 0, len(orgs))
	for _, o := range orgs {
		name := cmp.Or(o.Name, o.ID)
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// printLines prints the checklist so far.
func (u *setupUI) printLines() {
	if len(u.lines) > 0 {
		fmt.Fprintln(u.out)
		for _, l := range u.lines {
			fmt.Fprintln(u.out, l)
		}
	}
	u.lines = nil
}

func (u *setupUI) finish() {
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
	width := cmp.Or(terminalColumns(u.out), 80) - 4
	for _, step := range u.next {
		lines := wrapStep(step, width)
		fmt.Fprintf(u.out, "  %s %s\n", u.p.Brand("•"), u.p.Commands(lines[0]))
		for _, l := range lines[1:] {
			fmt.Fprintf(u.out, "    %s\n", u.p.Commands(l))
		}
	}
}

// terminalColumns is w's width when it is a terminal, or 0.
func terminalColumns(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	cols, _, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0
	}
	return cols
}

// wrapStep breaks a next step at spaces into lines of at most width columns, never inside a
// `quoted command`, keeping the step's own line breaks.
func wrapStep(step string, width int) []string {
	var out []string
	for para := range strings.SplitSeq(step, "\n") {
		line := ""
		for _, word := range stepWords(para) {
			switch {
			case line == "":
				line = word
			case len([]rune(line))+1+len([]rune(word)) <= width:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		out = append(out, line)
	}
	return out
}

// stepWords splits s at spaces outside backquotes.
func stepWords(s string) []string {
	var words []string
	var b strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '`':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == ' ' && !inQuote:
			if b.Len() > 0 {
				words = append(words, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		words = append(words, b.String())
	}
	return words
}

// setupHeaderInfo is the column beside the logo; it reads only local state, so it is safe
// to show before signing in.
func (app *App) setupHeaderInfo(cfg *config.Config, p style.Palette) []string {
	info := []string{p.Bold("Terma CLI") + " " + p.Dim("("+app.version+")")}

	switch {
	case cfg.APIKey != "":
		info = append(info, p.Dim("using TERMA_API_KEY"))
	default:
		cred, err := auth.LoadIdentity(app.dir, cfg.ProfileName)
		if err != nil {
			info = append(info, p.Dim("not signed in — setup will sign you in"))
			break
		}
		who := cmp.Or(cred.UserEmail, "signed in")
		org := cmp.Or(cfg.OrganizationName, cfg.OrganizationID)
		if org != "" {
			who += "  " + p.Dim("("+org+")")
		}
		info = append(info, who)
	}

	if cwd, err := os.Getwd(); err == nil {
		info = append(info, p.Dim(output.TildePath(cwd)))
	}
	return info
}
