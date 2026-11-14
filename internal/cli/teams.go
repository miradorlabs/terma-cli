package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

// teamsFlags are `terma teams`' changes, for the desktop apps and scripts; with none, it
// lists the teams, and on a terminal lets the developer tick and untick them.
type teamsFlags struct {
	selects, deselects, releases []string
	binds, refuses, unbinds      []string
	// org is --org: an organization to sign in to first, so its teams can be listed and ticked.
	org string
	// authorize is --authorize: sign in first, in one approval, to the organizations this
	// machine has no sign-in for, auth.MaxCodes at a time.
	authorize bool
	json      bool
	noBrowser bool
	// assumeYes takes every question's default answer: a binding a ticked team needs is
	// made to its organization, and a decision against an owner's is made as asked.
	assumeYes bool
}

// changes reports whether f asks for any change.
func (f teamsFlags) changes() bool {
	return len(f.selects)+len(f.deselects)+len(f.releases)+len(f.binds)+len(f.refuses)+len(f.unbinds) > 0
}

func (app *App) newTeamsCommand() *cobra.Command {
	var f teamsFlags
	cmd := &cobra.Command{
		Use:   "teams",
		Short: "Choose the teams this machine sends telemetry to, across your organizations",
		Long: `Lists every organization you belong to, and lets you tick the teams this machine sends
telemetry to. Each team shows what its policy collects, so you see what you agree to
before you tick it. An organization's teams are listed once this machine has a sign-in
there: --authorize signs in to every one without, in one approval, and --org to one.

Ticking a team signs in to its organization if needed, fetches its policy, creates its
key for this machine and asks you to decide each binding its policy needs. A binding says
which organization an owner (github.com/acme) or a repository belongs to on this
machine: only that organization collects it, and personal never does. Bindings outlive
unticking: what stays bound to an organization collects nowhere until you unbind it.

Each record goes to the first ticked team whose policy matches where it ran,
organizations first and personal last.

Without a terminal, change it with flags (each may repeat):

  --select <team>                       tick a team, by name or id
  --deselect <team>                     untick it; its key and queued records go
  --release <team>                      remove a suspended or refused team
  --bind <owner-or-repository>=<org>    bind it to an organization
  --refuse <owner-or-repository>        refuse it to every organization
  --unbind <owner-or-repository>        remove the decision
  --authorize                           sign in to the organizations without a sign-in here,
                                        20 per approval
  --json                                print the list as JSON`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return app.runTeams(cmd, f) },
	}
	fl := cmd.Flags()
	fl.StringArrayVar(&f.selects, "select", nil, "tick a team, by name or id")
	fl.StringArrayVar(&f.deselects, "deselect", nil, "untick a team, by name or id")
	fl.StringArrayVar(&f.releases, "release", nil, "remove a suspended or refused team, by name or id")
	fl.StringArrayVar(&f.binds, "bind", nil, "bind an owner or repository to an organization: <owner-or-repository>=<organization>")
	fl.StringArrayVar(&f.refuses, "refuse", nil, "refuse an owner or repository to every organization")
	fl.StringArrayVar(&f.unbinds, "unbind", nil, "remove the decision on an owner or repository")
	fl.StringVar(&f.org, "org", "", "sign in to this organization first, by name or id, to list and tick its teams")
	fl.BoolVar(&f.authorize, "authorize", false, "sign in first, in one approval, to the organizations without a sign-in here (20 at a time)")
	fl.BoolVar(&f.json, "json", false, "print the list, and what changed, as JSON")
	fl.BoolVar(&f.noBrowser, "no-browser", false, "print the sign-in URL instead of opening a browser")
	fl.BoolVarP(&f.assumeYes, "yes", "y", false, "take each question's default: bind what a ticked team needs to its organization")
	return cmd
}

func (app *App) runTeams(cmd *cobra.Command, f teamsFlags) error {
	cfg, err := app.loadConfig()
	if err != nil {
		return err
	}
	if cfg.APIKey != "" || cfg.ServerKeySignIn {
		return errors.New("this machine signs in with a team server key (TERMA_API_KEY), which stays on its one team: sign in as yourself with `terma setup` to choose teams")
	}
	if err := app.migrateFirst(cmd, cfg.ProfileName); err != nil {
		return err
	}
	if cfg, err = app.loadConfig(); err != nil {
		return err
	}
	st, err := loadRoutingState(app.stateDir, cfg.ProfileName)
	if err != nil {
		return err
	}
	var v teamsView
	if f.org != "" {
		if _, err := app.signInTo(cmd, cfg, parseOrgRef(f.org), f.noBrowser); err != nil {
			return err
		}
		if cfg, err = app.loadConfig(); err != nil {
			return err
		}
	}
	if f.authorize {
		if err := app.runAuthorize(cmd, cfg, f.noBrowser); err != nil {
			return err
		}
		if cfg, err = app.loadConfig(); err != nil {
			return err
		}
	}
	switch {
	case f.changes():
		if v, err = app.applyTeams(cmd, cfg, st, f); err != nil {
			return err
		}
	case !f.json && !f.authorize && app.canAsk() && canPrompt():
		if v, err = app.teamsChecklist(cmd, cfg, st, f); err != nil {
			if errors.Is(err, errCancelled) {
				fmt.Fprintln(cmd.OutOrStdout(), "Cancelled. Nothing was changed.")
				return nil
			}
			return err
		}
	default:
		v = app.listTeams(cmd.Context(), cfg, st)
	}
	if f.json {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	renderTeams(cmd.OutOrStdout(), v)
	return nil
}

// loadRoutingState is the profile's routing state, which `terma teams` changes; a missing
// or unreadable one is never started afresh here (`terma setup` does, and says so).
func loadRoutingState(stateDir, profile string) (routing.State, error) {
	st, ok, err := routing.Load(stateDir, profile)
	switch {
	case err != nil:
		return routing.State{}, fmt.Errorf("this machine's routing state cannot be read, so nothing is sent: run `terma setup` to choose your teams again: %w", err)
	case !ok:
		return routing.State{}, errors.New("this profile has no routing state, so nothing is sent: run `terma setup` to choose your teams")
	}
	return st, nil
}

// renderTeams prints the list as the plan draws it: each organization's teams, ticked or
// not, with what they collect, then the bindings and what is left to decide.
func renderTeams(w io.Writer, v teamsView) {
	out := style.Highlight(w)
	p := style.For(w)
	for _, c := range v.Changes {
		fmt.Fprintf(out, "%s %s\n", p.OK("✓"), c)
	}
	for _, warn := range v.Warnings {
		fmt.Fprintf(out, "%s %s\n", p.Warn("!"), warn)
	}
	if len(v.Changes)+len(v.Warnings) > 0 {
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out, p.Bold("Send telemetry from this machine to:"))
	if v.Unlisted != "" {
		fmt.Fprintf(out, "  %s\n", p.Dim("organizations not listed: "+v.Unlisted))
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, o := range v.Organizations {
		if !o.SignedIn && len(o.Teams) == 0 {
			// Offered together, by the one action below.
			continue
		}
		fmt.Fprintf(tw, "\n  %s\n", orgTitle(o))
		if o.Note != "" {
			fmt.Fprintf(tw, "    %s\n", p.Dim(o.Note))
		}
		for _, t := range o.Teams {
			box := "[ ]"
			if t.Selected {
				box = "[x]"
			}
			fmt.Fprintf(tw, "    %s %s\t%s%s\n", box, cmp.Or(t.Name, t.ID), t.Collects, teamStateNote(t))
		}
	}
	_ = tw.Flush()
	if v.Unauthorized > 0 {
		fmt.Fprintf(out, "\n  %s: `terma teams --authorize`\n    %s\n", authorizeLabel(v.Unauthorized), p.Dim(authorizeNames(v.unauthorized)))
	}
	if len(v.Bindings) > 0 {
		fmt.Fprintf(out, "\n%s\n", p.Bold("Bindings:"))
		tw = tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		for _, b := range v.Bindings {
			who := cmp.Or(b.Name, b.Organization)
			if b.Refused {
				who = "refused to every organization"
			}
			fmt.Fprintf(tw, "  %s\t%s\n", b.Key, who)
		}
		_ = tw.Flush()
	}
	if len(v.Pending) > 0 {
		fmt.Fprintf(out, "\n%s\n", p.Bold("To decide (what these match is held until you do):"))
		for _, pr := range v.Pending {
			fmt.Fprintf(out, "  %s %s  needed by %s: `terma teams --bind %s=%s` or `terma teams --refuse %s`\n",
				p.Warn("!"), pr.Key, pr.Entry, pr.Key, quoteArg(cmp.Or(v.orgName(pr.Organization), pr.Organization)), pr.Key)
		}
	}
}

func orgTitle(o orgRow) string { return cmp.Or(o.Name, o.ID) }

// teamStateNote is a selected team's state beside its list, when it cannot send.
func teamStateNote(t teamRow) string {
	switch {
	case t.Unreadable != "":
		return "  (" + t.Unreadable + ")"
	case !t.Selected || t.State == "" || t.State == routing.Active:
		return ""
	}
	note := "  — " + strings.ReplaceAll(string(t.State), "_", " ")
	switch t.State {
	case routing.SignInNeeded:
		note += ": `terma teams --org <its organization>` signs in again"
	case routing.Suspended, routing.Refused:
		note += ", blocking what it lists: `terma teams --release " + quoteArg(cmp.Or(t.Name, t.ID)) + "` removes it"
	case routing.Expired:
		note += ": not refreshed for a week, blocking what it lists"
	}
	return note
}

// quoteArg quotes s for a shell when it has a space.
func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t'\"") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}
