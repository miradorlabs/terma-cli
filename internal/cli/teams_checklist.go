package cli

import (
	"cmp"
	"errors"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

type checklistSlot struct{ org, team int }

func teamsForm(v teamsView) (*prompt.Form, []checklistSlot) {
	form := &prompt.Form{Title: "Send telemetry from this machine to: (space toggles, enter confirms)"}
	var slots []checklistSlot
	for i, o := range v.Organizations {
		heading := orgTitle(o)
		if len(o.Teams) == 0 && o.Note != "" {
			form.Items = append(form.Items, prompt.Item{Label: "(no teams listed)", Kind: prompt.Check, Disabled: true, Reason: o.Note, Heading: heading})
			slots = append(slots, checklistSlot{-1, -1})
			continue
		}
		for j, t := range o.Teams {
			item := prompt.Item{Label: cmp.Or(t.Name, t.ID), Detail: t.Collects + teamStateNote(t), Kind: prompt.Check, Selected: t.Selected, Heading: heading}
			if (t.Unreadable != "" || o.Inaccessible) && !t.Selected {
				item.Disabled, item.Reason = true, cmp.Or(t.Unreadable, o.Note)
			}
			form.Items, slots, heading = append(form.Items, item), append(slots, checklistSlot{i, j}), ""
		}
	}
	return form, slots
}

func checklistDiff(v teamsView, slots []checklistSlot, result []prompt.Item, base teamsFlags) teamsFlags {
	for k, s := range slots {
		if s.org < 0 || k >= len(result) {
			continue
		}
		switch t := v.Organizations[s.org].Teams[s.team]; {
		case result[k].Selected && !t.Selected:
			base.selects = append(base.selects, t.ID)
		case !result[k].Selected && t.Selected:
			base.deselects = append(base.deselects, t.EnrollmentID)
		}
	}
	return base
}

func (app *App) runTeamsChecklist(cmd *cobra.Command, cfg *config.Config, st routing.State, v teamsView, f teamsFlags) (teamsView, error) {
	form, slots := teamsForm(v)
	if len(form.Items) == 0 {
		return v, nil
	}
	run := app.runForm
	if run == nil {
		run = prompt.Run
	}
	result, err := run(form)
	if errors.Is(err, prompt.ErrCancelled) {
		return teamsView{}, errCancelled
	}
	if err != nil {
		return teamsView{}, err
	}
	if ff := checklistDiff(v, slots, result, f); ff.changes() {
		return app.applyTeamsWithView(cmd, cfg, st, v, true, ff)
	}
	return v, nil
}

func (app *App) setupMultiTeamChecklist(cmd *cobra.Command, cfg *config.Config, st routing.State, primary routing.Team, primaryName string, ui *setupUI) (routing.Team, string, bool, error) {
	v, count := app.listTeams(cmd.Context(), cfg, st), 0
	for _, o := range v.Organizations {
		count += len(o.Teams)
	}
	if count <= 1 {
		return primary, primaryName, false, nil
	}
	v, err := app.runTeamsChecklist(cmd, cfg, st, v, teamsFlags{})
	if err != nil || len(v.Changes) == 0 {
		return primary, primaryName, false, err
	}
	var ticked []string
	for _, o := range v.Organizations {
		for _, t := range o.Teams {
			if t.Selected {
				ticked = append(ticked, orgTeamLabel(o.Name, cmp.Or(t.Name, t.ID)))
			}
		}
	}
	if len(ticked) == 0 {
		ui.Warn("Teams", "none ticked, so nothing is collected")
		ui.Then("Run `terma teams` to choose the teams this machine sends telemetry to.")
	} else {
		ui.Summary("Teams", strings.Join(ticked, ", "))
	}
	for _, w := range v.Warnings {
		ui.Caution("Teams", w)
	}
	if len(v.Pending) > 0 {
		ui.Then("Run `terma teams` to decide pending repository bindings.")
	}
	if len(v.state.Teams) == 0 {
		return routing.Team{}, "", true, nil
	}
	if i := slices.IndexFunc(v.state.Teams, func(tm routing.Team) bool {
		return tm.TeamID == primary.TeamID && tm.OrganizationID == primary.OrganizationID
	}); i >= 0 {
		return v.state.Teams[i], primaryName, true, nil
	}
	chosen := v.state.Teams[0]
	if r := v.findTeam(chosen.OrganizationID, chosen.TeamID); r != nil && r.Name != "" {
		return chosen, r.Name, true, nil
	}
	return chosen, chosen.TeamID, true, nil
}
