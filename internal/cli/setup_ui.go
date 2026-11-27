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
)

// setupUI reports setup's steps, verdict and next steps; successful steps and details
// print only under --verbose.
type setupUI struct {
	out    io.Writer
	detail io.Writer
	p      style.Palette
	warned bool
	// lines are the checklist, printed together at finish so prompts never split it, and
	// labels the label of each line's step (a wrapped line's too).
	lines, labels []string
	next          []string
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
	lines := wrapStep(what, style.Columns(u.out)-indent)
	u.lines = append(u.lines, fmt.Sprintf("  %s %-*s %s", mark, stepLabelWidth, label, u.p.Commands(lines[0])))
	u.labels = append(u.labels, label)
	for _, l := range lines[1:] {
		u.lines = append(u.lines, strings.Repeat(" ", indent)+u.p.Commands(l))
		u.labels = append(u.labels, label)
	}
}

// drop takes back the checklist lines of label: a step a later one undid.
func (u *setupUI) drop(label string) {
	var lines, labels []string
	for i, l := range u.lines {
		if u.labels[i] != label {
			lines, labels = append(lines, l), append(labels, u.labels[i])
		}
	}
	u.lines, u.labels = lines, labels
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

// teamsConfigured summarizes the enrolled organizations and the teams st ticks, including
// one whose policy could not be read this run, which keeps its last one.
func (u *setupUI) teamsConfigured(orgs []organization, st routing.State, names teamNames) {
	if len(orgs) > 1 {
		u.Summary("Organizations", strings.Join(orgNamesList(orgs), ", "))
	}
	dests := make([]orgTeam, 0, len(st.Teams))
	anyAdmits, gitHooksOff := false, false
	now := time.Now()
	for _, t := range st.Teams {
		dests = append(dests, orgTeam{cmp.Or(names.Orgs[t.OrganizationID], t.OrganizationID), cmp.Or(names.Teams[t.TeamID], t.TeamID), t.Personal})
		if pol := t.Policy; !pol.AdmitsNone() {
			anyAdmits = true
			gitHooksOff = gitHooksOff || doctor.GitHooksOff(pol, now)
		}
	}
	u.Summary("Sending to", sendingTo(dests))
	u.policyNextSteps(st.Teams[0].Policy, anyAdmits, gitHooksOff)
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
	if line := conflictLine(st, names); line != "" {
		u.Caution("Conflict", line)
	}
}

// doctorStep is setup's one pointer to `terma doctor`, which explains every ! line.
const doctorStep = "Run `terma doctor` to check everything and see which team collects each repository."

// conflictStep points to where each conflict is explained.
const conflictStep = "Run `terma doctor` to see which team collects each repository."

// conflictLine names the teams in st whose policies overlap, or is "" when none do.
func conflictLine(st routing.State, names teamNames) string {
	var ids, labels []string
	for _, o := range routing.Overlaps(st) {
		for _, t := range []routing.Team{o.Winner, o.Other} {
			if slices.Contains(ids, t.EnrollmentID) {
				continue
			}
			ids = append(ids, t.EnrollmentID)
			// Each name is quoted, as a team name may hold commas, "and" or "›".
			if l := "'" + teamLabel(names, t) + "'"; !slices.Contains(labels, l) {
				labels = append(labels, l)
			}
		}
	}
	switch {
	case len(ids) == 0:
		return ""
	case len(labels) == 1 && len(ids) == 2:
		return "Two teams named " + labels[0] + " list the same repository."
	case len(labels) == 1:
		return fmt.Sprintf("%d teams named %s list the same repositories.", len(ids), labels[0])
	case len(labels) == 2:
		return labels[0] + " and " + labels[1] + " list the same repository."
	}
	return strings.Join(labels[:len(labels)-1], ", ") + " and " + labels[len(labels)-1] + " list the same repositories."
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
	u.lines, u.labels = nil, nil
}

// headline opens the summary: the title in bold green, or the warn title in bold yellow
// when a step needs the developer.
func (u *setupUI) headline() {
	if u.warned {
		fmt.Fprintf(u.out, "\n%s\n", u.p.Warn(u.p.Bold("! "+cmp.Or(u.warnTitle, u.title+". Some steps need you."))))
	} else {
		fmt.Fprintf(u.out, "\n%s\n", u.p.OK(u.p.Bold("✓ "+u.title)))
	}
}

func (u *setupUI) finish() {
	u.headline()
	u.printLines()
	if len(u.next) == 0 {
		return
	}
	fmt.Fprintf(u.out, "\n%s\n", u.p.Bold("Next steps:"))
	width := style.Columns(u.out) - 4
	for _, step := range u.next {
		lines := wrapStep(step, width)
		fmt.Fprintf(u.out, "  %s %s\n", u.p.Brand("•"), u.p.Commands(lines[0]))
		for _, l := range lines[1:] {
			fmt.Fprintf(u.out, "    %s\n", u.p.Commands(l))
		}
	}
}

// wrapStep breaks a next step into lines of at most width columns, never inside a
// `quoted command`, keeping the step's own line breaks.
func wrapStep(step string, width int) []string {
	var out []string
	for para := range strings.SplitSeq(step, "\n") {
		out = append(out, style.Wrap(para, width)...)
	}
	return out
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
