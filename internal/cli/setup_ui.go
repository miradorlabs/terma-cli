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

// policyFetched says which team and policy are now in force, and what is left to do about
// them: a policy to set up, a list to fill in, or commit hooks to turn on.
func (u *setupUI) policyFetched(team string, pol config.Policy) {
	if team != "" {
		u.Summary("Team", team)
	}
	if pol.AdmitsNone() {
		u.Warn("Collects", doctor.PolicySummary(pol))
		u.Then(doctor.NothingCollectedStep(pol))
		return
	}
	u.Summary("Collects", doctor.PolicySummary(pol))
	if doctor.GitHooksOff(pol, time.Now()) {
		u.Then(doctor.GitHooksOffStep)
	}
}

// teamsConfigured summarizes multiple enrolled teams/organizations and what their
// policies actively collect across the machine.
func (u *setupUI) teamsConfigured(orgs []organization, teams []discoveredTeam, state routing.State) {
	if len(orgs) > 1 {
		u.Summary("Organizations", strings.Join(orgNamesList(orgs), ", "))
	}
	multiOrg := len(orgs) > 1
	labels := make([]string, 0, len(teams))
	var repos []string
	hasPersonal := false
	anyAdmits := false
	gitHooksOff := false
	now := time.Now()
	for _, dt := range teams {
		if multiOrg {
			labels = append(labels, orgTeamLabel(dt.orgName, dt.teamName))
		} else {
			labels = append(labels, dt.teamName)
		}
		pol := dt.team.Policy
		if dt.team.Personal && pol.Global() {
			hasPersonal = true
			anyAdmits = true
		} else if !pol.AdmitsNone() {
			anyAdmits = true
			for _, r := range pol.Repositories {
				if entryBoundToOrg(state, r, pol.Wildcards, dt.team.OrganizationID) {
					repos = appendUnique(repos, r)
				}
			}
		}
		if !pol.AdmitsNone() && doctor.GitHooksOff(pol, now) {
			gitHooksOff = true
		}
	}
	u.Summary("Teams", strings.Join(labels, ", "))
	if !anyAdmits {
		pol := teams[0].team.Policy
		u.Warn("Collects", doctor.PolicySummary(pol))
		u.Then(doctor.NothingCollectedStep(pol))
		return
	}
	var parts []string
	if len(repos) > 0 {
		shown := repos
		extra := 0
		if len(shown) > 3 {
			extra = len(shown) - 3
			shown = shown[:3]
		}
		s := strings.Join(shown, ", ")
		if extra > 0 {
			s += fmt.Sprintf(", and %d more", extra)
		}
		parts = append(parts, s)
	}
	if hasPersonal {
		if len(parts) > 0 {
			parts = append(parts, "plus Personal (everything else)")
		} else {
			parts = append(parts, personalCollects)
		}
	}
	if len(parts) == 0 {
		u.Warn("Collects", "nothing yet — resolve pending bindings with `terma teams`")
		return
	}
	u.Summary("Collects", strings.Join(parts, "; "))
	if gitHooksOff {
		u.Then(doctor.GitHooksOffStep)
	}
}

// entryBoundToOrg reports whether policy entry raw is actively bound to org in state
// (not pending and not refused).
func entryBoundToOrg(state routing.State, raw string, wildcards bool, orgID string) bool {
	if orgID == "" {
		return true
	}
	e, ok := routing.ParseEntry(raw, wildcards)
	if !ok {
		return false
	}
	if b, ok := state.Bindings[e.Key()]; ok {
		return !b.Refused && b.Organization == orgID
	}
	if !e.Wildcard() {
		if ob, ok := state.Bindings[routing.Owner(e.Key())]; ok {
			return !ob.Refused && ob.Organization == orgID
		}
	}
	return false
}

// reportConflicts notifies the developer of every policy conflict found during setup.
func (u *setupUI) reportConflicts(conflicts []policyConflict) {
	for _, c := range conflicts {
		if c.severity == conflictPending {
			u.Warn("Conflict", c.summary)
		} else {
			u.Caution("Conflict", c.summary)
		}
		if c.nextStep != "" {
			u.Then(c.nextStep)
		}
	}
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
