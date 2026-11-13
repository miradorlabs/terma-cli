package cli

import (
	"cmp"
	"errors"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

// teamsChecklist is `terma teams` on a terminal: the grouped list as boxes to tick. An
// organization this machine has no sign-in for offers one, and its teams follow it. What
// was ticked and unticked is then made as the flags make it (applyTeams).
func (app *App) teamsChecklist(cmd *cobra.Command, cfg *config.Config, st routing.State, f teamsFlags) (teamsView, error) {
	v := app.listTeams(cmd.Context(), cfg, st)
	type slot struct {
		org, team int
	}
	form := &prompt.Form{Title: "Send telemetry from this machine to: (space toggles, enter confirms)"}
	var slots []slot
	for i, o := range v.Organizations {
		heading := orgTitle(o)
		if !o.SignedIn && len(o.Teams) == 0 {
			form.Items = append(form.Items, prompt.Item{Label: "sign in to see its teams", Kind: prompt.Check, Heading: heading})
			slots = append(slots, slot{org: i, team: -1})
			continue
		}
		for j, t := range o.Teams {
			item := prompt.Item{Label: cmp.Or(t.Name, t.ID), Detail: t.Collects + teamStateNote(t), Kind: prompt.Check, Selected: t.Selected, Heading: heading}
			if t.Unreadable != "" && !t.Selected {
				item.Disabled, item.Reason = true, t.Unreadable
			}
			form.Items = append(form.Items, item)
			slots = append(slots, slot{org: i, team: j})
			heading = ""
		}
	}
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
		o := v.Organizations[s.org]
		if s.team < 0 {
			if result[k].Selected {
				if _, err := app.signInTo(cmd, cfg, orgRef{ID: o.ID, Name: o.Name}, f.noBrowser); err != nil {
					return teamsView{}, err
				}
				signedIn = true
			}
			continue
		}
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
