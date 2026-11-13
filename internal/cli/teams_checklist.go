package cli

import (
	"cmp"
	"errors"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

// checklistSlot is what a checklist line stands for: a team, by its organization's and
// its own index in the view, or with org < 0 the action authorizing the organizations
// without a sign-in.
type checklistSlot struct {
	org, team int
}

// teamsForm is the grouped list as boxes to tick: each organization's teams, then one
// action that authorizes the organizations this machine has no sign-in for, in one approval.
func teamsForm(v teamsView) (*prompt.Form, []checklistSlot) {
	form := &prompt.Form{Title: "Send telemetry from this machine to: (space toggles, enter confirms)"}
	var slots []checklistSlot
	for i, o := range v.Organizations {
		heading := orgTitle(o)
		for j, t := range o.Teams {
			item := prompt.Item{Label: cmp.Or(t.Name, t.ID), Detail: t.Collects + teamStateNote(t), Kind: prompt.Check, Selected: t.Selected, Heading: heading}
			if t.Unreadable != "" && !t.Selected {
				item.Disabled, item.Reason = true, t.Unreadable
			}
			form.Items = append(form.Items, item)
			slots = append(slots, checklistSlot{org: i, team: j})
			heading = ""
		}
	}
	if v.Unauthorized > 0 {
		form.Items = append(form.Items, prompt.Item{Label: authorizeLabel(v.Unauthorized), Detail: authorizeNames(v.unauthorized),
			Kind: prompt.Check, Heading: "Organizations without a sign-in here"})
		slots = append(slots, checklistSlot{org: -1, team: -1})
	}
	return form, slots
}

// teamsChecklist is `terma teams` on a terminal: the grouped list as boxes to tick
// (teamsForm). Ticking the action authorizes the organizations without a sign-in, and the
// list is shown again with their teams. What was ticked and unticked is then made as the
// flags make it (applyTeams).
func (app *App) teamsChecklist(cmd *cobra.Command, cfg *config.Config, st routing.State, f teamsFlags) (teamsView, error) {
	v := app.listTeams(cmd.Context(), cfg, st)
	form, slots := teamsForm(v)
	result, err := prompt.Run(form)
	if errors.Is(err, prompt.ErrCancelled) {
		return teamsView{}, errCancelled
	}
	if err != nil {
		return teamsView{}, err
	}
	signedIn := false
	ff := f
	for k, s := range slots {
		if s.org < 0 {
			if result[k].Selected {
				if err := app.authorizeMore(cmd, cfg, v.unauthorized, f.noBrowser); err != nil {
					return teamsView{}, err
				}
				signedIn = true
			}
			continue
		}
		o := v.Organizations[s.org]
		t := o.Teams[s.team]
		switch {
		case result[k].Selected && !t.Selected:
			ff.selects = append(ff.selects, t.ID)
		case !result[k].Selected && t.Selected:
			ff.deselects = append(ff.deselects, t.EnrollmentID)
		}
	}
	if signedIn {
		if cfg, err = app.loadConfig(); err != nil {
			return teamsView{}, err
		}
		return app.teamsChecklist(cmd, cfg, st, f)
	}
	if !ff.changes() {
		return v, nil
	}
	return app.applyTeams(cmd, cfg, st, ff)
}
