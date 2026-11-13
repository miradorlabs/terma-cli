package cli

import (
	"cmp"
	"context"
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// setupTeams is setup's team step on a machine that routes by teams: the checklist
// `terma teams` shows, in place of the organization and team pickers. A profile without
// a routing state starts a fresh one, saying that bindings made before are not known, and,
// being a new setup, ticks personal when this machine has its sign-in; one it cannot read
// is set aside, never written over. --team ticks that team.
func (app *App) setupTeams(cmd *cobra.Command, ui *setupUI, f setupFlags) func(ctx context.Context, cfg *config.Config) error {
	return func(ctx context.Context, cfg *config.Config) error {
		st, fresh, err := app.setupRoutingState(ui, cfg.ProfileName)
		if err != nil {
			return err
		}
		tf := teamsFlags{noBrowser: f.noBrowser, assumeYes: f.assumeYes}
		if cfg.ProjectID != "" && !slices.ContainsFunc(st.Teams, func(t routing.Team) bool { return t.TeamID == cfg.ProjectID }) {
			tf.selects = append(tf.selects, cfg.ProjectID)
		}
		var v teamsView
		if fresh {
			v = app.listTeams(ctx, cfg, st)
			if id, ok := personalToTick(v); ok && !slices.Contains(tf.selects, id) {
				tf.selects = append(tf.selects, id)
			}
		}
		switch {
		case tf.changes():
			v, err = app.applyTeams(cmd, cfg, st, tf)
		case !f.assumeYes && app.canAsk() && canPrompt():
			v, err = app.teamsChecklist(cmd, cfg, st, tf)
		case !fresh:
			v = app.listTeams(ctx, cfg, st)
		}
		if errors.Is(err, errCancelled) {
			return err
		}
		if err != nil {
			ui.Warn("Teams", err.Error())
			ui.Then("Run `terma teams` to choose the teams this machine sends telemetry to.")
		}
		reportTeams(ui, v)
		return nil
	}
}

// setupRoutingState is the profile's routing state, a fresh one when it has none: fresh
// says so. One that cannot be read is moved aside and replaced, and said, since its teams
// and bindings are lost.
func (app *App) setupRoutingState(ui *setupUI, profile string) (routing.State, bool, error) {
	st, ok, err := routing.Load(app.stateDir, profile)
	if err == nil && ok {
		return st, false, nil
	}
	if err != nil {
		path, perr := routing.StatePath(app.stateDir, profile)
		if perr != nil {
			return routing.State{}, false, perr
		}
		if rerr := os.Rename(path, path+".unreadable"); rerr != nil {
			return routing.State{}, false, rerr
		}
		ui.Caution("Teams", "this machine's routing state could not be read ("+err.Error()+"), so its teams and bindings are lost; it is kept as "+output.TildePath(path+".unreadable"))
	} else {
		ui.Caution("Teams", "no routing state for this profile: bindings made on this machine before, if any, are lost — tick its teams again")
	}
	st, err = routing.Create(app.stateDir, profile, func(*routing.State) error { return nil })
	return st, true, err
}

// personalToTick is the personal organization's one team, ticked by default on a new
// setup, when this machine has that organization's sign-in.
func personalToTick(v teamsView) (string, bool) {
	for _, o := range v.Organizations {
		if o.Type == "personal" && o.SignedIn && len(o.Teams) == 1 && !o.Teams[0].Selected {
			return o.Teams[0].ID, true
		}
	}
	return "", false
}

// reportTeams puts the ticked teams in setup's checklist: what each collects, what
// personal collects and that it stamps commits, and what is left to decide.
func reportTeams(ui *setupUI, v teamsView) {
	var ticked []string
	for _, o := range v.Organizations {
		for _, t := range o.Teams {
			if !t.Selected {
				continue
			}
			ticked = append(ticked, orgTeamLabel(o.Name, cmp.Or(t.Name, t.ID)))
			if o.Type == "personal" {
				ui.Caution("Personal", personalWords(t))
			}
		}
	}
	switch {
	case len(ticked) == 0:
		ui.Warn("Teams", "none ticked, so nothing is collected")
		ui.Then("Run `terma teams` to choose the teams this machine sends telemetry to.")
	default:
		ui.Summary("Teams", strings.Join(ticked, ", "))
	}
	for _, w := range v.Warnings {
		ui.Caution("Teams", w)
	}
	if len(v.Pending) > 0 {
		keys := make([]string, 0, len(v.Pending))
		for _, p := range v.Pending {
			keys = append(keys, p.Key)
		}
		ui.Warn("Bindings", "undecided, so what they match is held: "+strings.Join(keys, ", "))
		ui.Then("Run `terma teams` to decide which organization each belongs to.")
	}
}

// personalWords says what personal collects and that it stamps commits there.
func personalWords(t teamRow) string {
	what := "collects " + t.Collects
	if t.GitHooks {
		what += ", and stamps commits in every repository an agent works in there"
	}
	return what
}
