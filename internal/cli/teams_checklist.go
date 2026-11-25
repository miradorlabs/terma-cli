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
