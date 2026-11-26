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

func (u *setupUI) keep(mark, label, what string) {
	u.lines = append(u.lines, fmt.Sprintf("  %s %-*s %s", mark, stepLabelWidth, label, u.p.Commands(what)))
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

// reportConflicts names every repository two organizations' policies both list, the team
// that collects it, and the order routing tries the teams in, which policy updates change.
func (u *setupUI) reportConflicts(st routing.State, names teamNames) {
	overlaps := routing.Overlaps(st)
	if len(overlaps) == 0 {
		return
	}
	for _, o := range overlaps {
		u.Caution("Conflict", fmt.Sprintf("%s is listed by %s and %s; %s collects it, as its policy is newer",
			o.Entry, teamLabel(names, o.Winner), teamLabel(names, o.Other), teamLabel(names, o.Winner)))
	}
	for i, t := range routing.Order(st) {
		line := fmt.Sprintf("%d. %s — %s", i+1, teamLabel(names, t), doctor.PolicyAge(t))
		if i == 0 {
			u.Caution("Routing order", line)
		} else {
			u.keep(" ", "", line)
		}
	}
	u.Then("Note that " + doctor.OrderChanges + ". Run `terma doctor` to see the current order.")
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
	for _, step := range u.next {
		lines := strings.Split(u.p.Commands(step), "\n")
		fmt.Fprintf(u.out, "  %s %s\n", u.p.Brand("•"), lines[0])
		for _, l := range lines[1:] {
			fmt.Fprintf(u.out, "    %s\n", l)
		}
	}
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
