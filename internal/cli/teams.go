package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

type teamsFlags struct {
	selects, deselects, releases []string
	json                         bool
}

func (f teamsFlags) changes() bool {
	return len(f.selects)+len(f.deselects)+len(f.releases) > 0
}

var errServerKeyTeams = errors.New("this machine signs in with a team server key (TERMA_API_KEY), which stays on its one team: sign in as yourself with `terma setup` to choose teams")

func (app *App) newTeamsCommand() *cobra.Command {
	var f teamsFlags
	cmd := &cobra.Command{
		Use:   "teams",
		Short: "Choose which teams this machine sends telemetry to",
		Long: `List every organization and team your sign-in belongs to and choose which ones
this machine sends telemetry to. Each team's policy decides which repositories it
collects. Where two organizations' policies list the same repository, the most
recently updated policy collects it; personal collects what no ticked organization does.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return app.runTeams(cmd, f) },
	}
	fl := cmd.Flags()
	fl.StringArrayVar(&f.selects, "select", nil, "tick a team, by name or id")
	fl.StringArrayVar(&f.deselects, "deselect", nil, "untick a team, by name or id")
	fl.StringArrayVar(&f.releases, "release", nil, "remove a suspended or refused team, by name or id")
	fl.BoolVar(&f.json, "json", false, "print the list, and what changed, as JSON")
	return cmd
}

func (app *App) runTeams(cmd *cobra.Command, f teamsFlags) error {
	if app.flags.output != "" {
		switch format, err := app.resolveFormat(); {
		case err != nil:
			return err
		case format == output.FormatJSON:
			f.json = true
		case format != output.FormatTable:
			return fmt.Errorf("`terma teams` supports table and json output, not %s", format)
		}
	}
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
		v, err = app.applyTeamsWithView(cmd, cfg, st, teamsView{}, false, f)
	case !f.json && app.canAsk():
		v, err = app.runTeamsChecklist(cmd, cfg, st, app.listTeams(cmd.Context(), cfg, st), f)
	default:
		v = app.listTeams(cmd.Context(), cfg, st)
	}
	switch {
	case errors.Is(err, errCancelled):
		fmt.Fprintln(cmd.OutOrStdout(), "Cancelled. Nothing was changed.")
		return nil
	case err != nil:
		return err
	case f.json:
		return output.Render(cmd.OutOrStdout(), output.FormatJSON, output.Table{}, v)
	}
	if v.state.Profile == "" {
		v.state = st
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
	switch st, ok, err := routing.Load(stateDir, profile); {
	case err != nil:
		return routing.State{}, fmt.Errorf("this machine's routing state cannot be read, so nothing is sent: run `terma setup` to choose your teams again: %w", err)
	case !ok:
		return routing.State{}, errors.New("this profile has no routing state, so nothing is sent: run `terma setup` to choose your teams")
	default:
		return st, nil
	}
}

// renderTeams prints the selected teams, what changed and any policy conflict, as setup does.
func renderTeams(w io.Writer, v teamsView) {
	names := teamNames{Teams: map[string]string{}, Orgs: map[string]string{}}
	for _, o := range v.Organizations {
		names.Orgs[o.ID] = orgTitle(o)
		for _, t := range o.Teams {
			names.Teams[t.ID] = cmp.Or(t.Name, t.ID)
		}
	}
	u := newSetupUI(w, false)
	var dests []orgTeam
	for _, t := range v.state.Teams {
		dests = append(dests, orgTeam{names.Orgs[t.OrganizationID], cmp.Or(names.Teams[t.TeamID], t.TeamID), t.Personal})
	}
	if len(dests) == 0 {
		u.Warn("Sending to", "Nowhere. Nothing is sent.")
	} else {
		u.Summary("Sending to", sendingTo(dests))
	}
	for _, o := range v.Organizations {
		for _, t := range o.Teams {
			if t.Selected {
				if label, action := teamAttention(t); label != "" {
					u.Warn(label, teamLabel(names, routing.Team{TeamID: t.ID, OrganizationID: o.ID, Personal: o.Type == "personal"})+". "+action)
				}
			}
		}
	}
	if v.Unlisted != "" {
		u.Caution("Note", "Your organizations could not be listed.")
	}
	for _, warn := range v.Warnings {
		u.Caution("Note", warn)
	}
	line := conflictLine(v.state, names)
	if line != "" {
		u.Caution("Conflict", line)
	}
	u.title, u.warnTitle = "All set. Nothing changed.", "Nothing changed. Some teams need you."
	if len(v.Changes) > 0 {
		u.title, u.warnTitle = "Changes applied", "Changes applied. Some teams need you."
	}
	u.headline()
	u.printLines()
	if line != "" {
		fmt.Fprintf(w, "\n%s\n", u.p.Commands(conflictStep))
	}
}

// teamAttention names what is wrong with a selected team and what the developer can do.
func teamAttention(t teamRow) (label, action string) {
	switch {
	case t.Unreadable != "":
		return "Unreadable", "Its policy could not be read."
	case t.State == routing.SignInNeeded:
		return "Signed out", "Run `terma setup` to sign in again."
	case t.State == routing.Suspended, t.State == routing.Refused:
		return "Blocked", "Run `terma teams --release " + quoteArg(cmp.Or(t.Name, t.ID)) + "` to remove it."
	case t.State == routing.Expired:
		return "Out of date", "Its policy could not be refreshed. Check the network."
	}
	return "", ""
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
		return note + ": run `terma setup` to sign in again"
	case routing.Suspended, routing.Refused:
		if t.LastError == routing.OrgAccessLost {
			note += " (" + orgInaccessibleNote + ")"
		}
		return note + ", blocking what it lists: `terma teams --release " + quoteArg(cmp.Or(t.Name, t.ID)) + "` removes it"
	case routing.Expired:
		return note + ": not refreshed for a week, blocking what it lists"
	}
	return note
}

func quoteArg(s string) string {
	if s == "" || strings.ContainsFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("._/@:=-", r)
	}) {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}
