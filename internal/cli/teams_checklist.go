package cli

import (
	"cmp"
	"errors"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

type checklistSlot struct{ org, team int }

// teamsForm builds the grouped checkbox form for v. Reusable by `terma teams` and `terma setup`.
func teamsForm(v teamsView) (*prompt.Form, []checklistSlot) {
	form := &prompt.Form{Title: "Send telemetry from this machine to: (space toggles, enter confirms)"}
	var slots []checklistSlot
	for i, o := range v.Organizations {
		heading := orgTitle(o)
		if len(o.Teams) == 0 && o.Note != "" {
			form.Items = append(form.Items, prompt.Item{Label: "(no teams listed)", Kind: prompt.Check, Disabled: true, Reason: o.Note, Heading: heading})
			slots = append(slots, checklistSlot{org: -1, team: -1})
			continue
		}
		for j, t := range o.Teams {
			item := prompt.Item{Label: cmp.Or(t.Name, t.ID), Detail: t.Collects + teamStateNote(t), Kind: prompt.Check, Selected: t.Selected, Heading: heading}
			if (t.Unreadable != "" || o.Inaccessible) && !t.Selected {
				item.Disabled, item.Reason = true, cmp.Or(t.Unreadable, o.Note)
			}
			form.Items = append(form.Items, item)
			slots = append(slots, checklistSlot{org: i, team: j})
			heading = ""
		}
	}
	return form, slots
}

func checklistDiff(v teamsView, slots []checklistSlot, result []prompt.Item, base teamsFlags) teamsFlags {
	ff := base
	for k, s := range slots {
		if s.org < 0 || k >= len(result) {
			continue
		}
		t := v.Organizations[s.org].Teams[s.team]
		switch {
		case result[k].Selected && !t.Selected:
			ff.selects = append(ff.selects, t.ID)
		case !result[k].Selected && t.Selected:
			ff.deselects = append(ff.deselects, t.EnrollmentID)
		}
	}
	return ff
}

// teamsChecklist lists all organizations concurrently on the single user token and runs
// the interactive team checklist, committing changes via CAS against st.
func (app *App) teamsChecklist(cmd *cobra.Command, cfg *config.Config, st routing.State, f teamsFlags) (teamsView, error) {
	return app.runTeamsChecklist(cmd, cfg, st, app.listTeams(cmd.Context(), cfg, st), f)
}

// runTeamsChecklist shows the interactive checklist for an already-fetched teamsView v
// and commits toggles via CAS against st.
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
	if err != nil {
		if errors.Is(err, prompt.ErrCancelled) {
			return teamsView{}, errCancelled
		}
		return teamsView{}, err
	}
	if ff := checklistDiff(v, slots, result, f); ff.changes() {
		return app.applyTeamsWithView(cmd, cfg, st, v, true, ff)
	}
	return v, nil
}
