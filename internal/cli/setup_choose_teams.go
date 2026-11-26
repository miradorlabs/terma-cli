package cli

import (
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

// chooseSetupTeams asks which teams this machine sends to, personal among them. A developer
// whose only team is personal is asked nothing, and personal is ticked; --org and --team
// have already named the teams. A first setup offers every team ticked, a re-run the teams
// ticked now; with --yes or no terminal that offer is taken as it stands.
func (app *App) chooseSetupTeams(cmd *cobra.Command, disc *setupDiscovery, assumeYes bool) error {
	if disc.scoped || !slices.ContainsFunc(disc.teams, func(dt discoveredTeam) bool { return !dt.team.Personal }) {
		return nil
	}
	rerun := disc.baseline.found && disc.baseline.sameAccount && len(disc.baseline.state.Teams) > 0
	keep := make([]bool, len(disc.teams))
	for i, dt := range disc.teams {
		keep[i] = !rerun || slices.ContainsFunc(disc.baseline.state.Teams, func(t routing.Team) bool { return t.TeamID == dt.team.TeamID })
	}
	canAsk := app.canAsk
	if canAsk == nil {
		canAsk = canPrompt
	}
	if !assumeYes && canAsk() {
		form := &prompt.Form{Title: "Send telemetry from this machine to: (space toggles, enter confirms)"}
		prevOrg := ""
		for i, dt := range disc.teams {
			heading := ""
			if dt.team.OrganizationID != prevOrg {
				heading, prevOrg = dt.orgName, dt.team.OrganizationID
			}
			item := prompt.Item{Label: dt.teamName, Detail: pickerDetail(dt.description, ""), Kind: prompt.Check, Selected: keep[i], Heading: heading}
			if dt.team.Personal && !slices.ContainsFunc(disc.teams, func(o discoveredTeam) bool {
				return o.team.OrganizationID == dt.team.OrganizationID && o.team.TeamID != dt.team.TeamID
			}) {
				item.Label, item.Heading, item.Gap = personalLabel, "", true
			}
			form.Items = append(form.Items, item)
		}
		fmt.Fprintln(cmd.OutOrStdout())
		result, err := app.runForm(form)
		if errors.Is(err, prompt.ErrCancelled) {
			return errCancelled
		}
		if err != nil {
			return err
		}
		for i := range keep {
			keep[i] = i < len(result) && result[i].Selected
		}
	}
	disc.untick = map[string]bool{}
	for i, dt := range disc.teams {
		if !keep[i] {
			disc.untick[dt.team.TeamID] = true
		}
	}
	disc.teams = slices.DeleteFunc(disc.teams, func(dt discoveredTeam) bool { return disc.untick[dt.team.TeamID] })
	if len(disc.teams) == 0 {
		return errors.New("no team chosen: run `terma setup` again and tick at least one")
	}
	return nil
}
