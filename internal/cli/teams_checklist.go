package cli

import (
	"cmp"
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

type checklistSlot struct{ org, team int }

// pickerDetail is a team's line beside its name in a picker: its description on one line,
// then note. A policy summary is left out, since policies change under the picker.
func pickerDetail(description, note string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(description), " ") + note)
}

// personalLabel names the personal organization's one team in a picker, in place of
// the organization's heading and the team's own name.
const personalLabel = "Personal"

// teamPickerTitle asks the question both team pickers put; the keys are on the line below.
const teamPickerTitle = "Which teams should this machine send to?"

func teamsForm(v teamsView) (*prompt.Form, []checklistSlot) {
	form := &prompt.Form{Title: teamPickerTitle}
	var slots []checklistSlot
	// Personal heads the list; the organizations follow under their headings.
	solo := func(o orgRow) bool { return o.Type == "personal" && len(o.Teams) == 1 }
	order := make([]int, 0, len(v.Organizations))
	for i, o := range v.Organizations {
		if solo(o) {
			order = append(order, i)
		}
	}
	for i, o := range v.Organizations {
		if !solo(o) {
			order = append(order, i)
		}
	}
	for _, i := range order {
		o := v.Organizations[i]
		heading := orgTitle(o)
		if len(o.Teams) == 0 && o.Note != "" {
			form.Items = append(form.Items, prompt.Item{Label: "(no teams listed)", Kind: prompt.Check, Disabled: true, Reason: o.Note, Heading: heading})
			slots = append(slots, checklistSlot{-1, -1})
			continue
		}
		for j, t := range o.Teams {
			item := prompt.Item{Label: cmp.Or(t.Name, t.ID), Detail: pickerDetail(t.Description, teamStateNote(t)), Kind: prompt.Check, Selected: t.Selected, Heading: heading}
			if solo(o) {
				item.Label, item.Heading = personalLabel, ""
			}
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
