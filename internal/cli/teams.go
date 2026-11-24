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

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

type teamsFlags struct {
	selects, deselects, releases []string
	binds, refuses, unbinds      []string
	json, assumeYes              bool
}

func (f teamsFlags) changes() bool {
	return len(f.selects)+len(f.deselects)+len(f.releases)+len(f.binds)+len(f.refuses)+len(f.unbinds) > 0
}

var errServerKeyTeams = errors.New("this machine signs in with a team server key (TERMA_API_KEY), which stays on its one team: sign in as yourself with `terma setup` to choose teams")

func (app *App) newTeamsCommand() *cobra.Command {
	var f teamsFlags
	cmd := &cobra.Command{
		Use:   "teams",
		Short: "Choose which teams this machine sends telemetry to",
		Long: `List every organization and team your sign-in belongs to and choose which ones
this machine sends telemetry to.

Each team shows what its collection policy asks for before you tick it. Ticking a team
fetches its policy and asks you to decide each binding its policy needs. A binding says
which organization an owner (github.com/acme) or a repository belongs to on this
machine: only that organization collects it, and personal never does. Bindings outlive
unticking: what stays bound to an organization collects nowhere until you unbind it.

Each record goes to the most specific ticked team policy that matches where it ran
(exact repository before wildcard, then selection order), with personal last.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return app.runTeams(cmd, f) },
	}
	fl := cmd.Flags()
	fl.StringArrayVar(&f.selects, "select", nil, "tick a team, by name or id")
	fl.StringArrayVar(&f.deselects, "deselect", nil, "untick it; bindings stay")
	fl.StringArrayVar(&f.releases, "release", nil, "remove a suspended or refused team, by name or id")
	fl.StringArrayVar(&f.binds, "bind", nil, "bind an owner or repository to an organization: <owner-or-repository>=<organization>")
	fl.StringArrayVar(&f.refuses, "refuse", nil, "refuse an owner or repository to every organization")
	fl.StringArrayVar(&f.unbinds, "unbind", nil, "remove the decision on an owner or repository")
	fl.BoolVar(&f.json, "json", false, "print the list, and what changed, as JSON")
	fl.BoolVarP(&f.assumeYes, "yes", "y", false, "take each question's default: bind what a ticked team needs to its organization")
	return cmd
}

func (app *App) runTeams(cmd *cobra.Command, f teamsFlags) error {
	cfg, err := app.loadConfig()
	if err != nil {
		return err
	}
	if cfg.APIKey != "" || cfg.ServerKeySignIn {
		return errServerKeyTeams
	}
	st, err := loadRoutingState(app.stateDir, cfg.ProfileName)
	if err != nil {
		return err
	}
	if st.Account == sendas.NoPersonLane {
		return errServerKeyTeams
	}
	if err := verifyTeamsAccount(app.dir, cfg, st); err != nil {
		return err
	}
	var v teamsView
	switch {
	case f.changes():
		v, err = app.applyTeams(cmd, cfg, st, f)
	case !f.json && app.canAsk():
		v, err = app.teamsChecklist(cmd, cfg, st, f)
	default:
		v = app.listTeams(cmd.Context(), cfg, st)
	}
	if errors.Is(err, errCancelled) {
		fmt.Fprintln(cmd.OutOrStdout(), "Cancelled. Nothing was changed.")
		return nil
	}
	if err != nil {
		return err
	}
	if f.json {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	renderTeams(cmd.OutOrStdout(), v)
	return nil
}

func verifyTeamsAccount(dir string, cfg *config.Config, st routing.State) error {
	if cfg.Account != "" && cfg.Account != st.Account {
		return fmt.Errorf("%w: this profile's routing state belongs to another account — run `terma setup` to choose your teams", routing.ErrAccountMismatch)
	}
	if live := auth.Account(dir, cfg.ProfileName, cfg.AuthURL); auth.ValidAccountKey(live) && live != st.Account {
		return fmt.Errorf("%w: this machine is signed in as another account — run `terma setup` to choose your teams", routing.ErrAccountMismatch)
	}
	return nil
}

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

func renderTeams(w io.Writer, v teamsView) {
	out, p := style.Highlight(w), style.For(w)
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
			fmt.Fprintf(out, "  %s %s  needed by %s: `terma teams --bind %s` or `terma teams --refuse %s`\n",
				p.Warn("!"), pr.Key, pr.Entry, quoteArg(pr.Key+"="+cmp.Or(v.orgName(pr.Organization), pr.Organization)), quoteArg(pr.Key))
		}
	}
}

func orgTitle(o orgRow) string { return cmp.Or(o.Name, o.ID) }

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
		note += ": run `terma setup` to sign in again"
	case routing.Suspended, routing.Refused:
		if t.LastError == routing.OrgAccessLost {
			note += " (" + orgInaccessibleNote + ")"
		}
		note += ", blocking what it lists: `terma teams --release " + quoteArg(cmp.Or(t.Name, t.ID)) + "` removes it"
	case routing.Expired:
		note += ": not refreshed for a week, blocking what it lists"
	}
	return note
}

func quoteArg(s string) string {
	unsafe := func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("._/@:=-", r)
	}
	if s == "" || strings.ContainsFunc(s, unsafe) {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}
