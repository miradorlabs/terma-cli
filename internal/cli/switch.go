package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

type switchFlags struct {
	noBrowser bool
	// assumeYes is --yes: it skips switch's own question only. Setup still asks for the
	// organization, team and agents, the questions switch is for.
	assumeYes bool
}

func (app *App) newSwitchCommand() *cobra.Command {
	var f switchFlags
	cmd := &cobra.Command{
		Use:   "switch",
		Short: "Sign in as another account, or choose another organization or team",
		Long: `Signs this machine out, then runs ` + "`terma setup`" + ` again, so you can sign in as any
account and choose its organization and team:

  1. Revokes this machine's sign-in, every organization's, as ` + "`terma teardown --sign-out`" + `
     does, and stops the relay, which runs on that sign-in.
  2. Forgets the organization and team setup chose, so setup asks for them again.
  3. Runs setup: sign in in the browser, then choose the organization, the team and your
     agents. Your agents' hooks stay in place throughout.

The browser approves the sign-in as whoever is signed in to the Terma app there. To use
another account, sign out of the app in that browser first, or open the link in a
private window (--no-browser prints it).

It needs a terminal, for setup's questions. If setup stops before it finishes, run ` + "`terma setup`" + ` to sign in again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return app.runSwitch(cmd, f) },
	}
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "print the sign-in URL instead of opening a browser")
	cmd.Flags().BoolVarP(&f.assumeYes, "yes", "y", false, "skip switch's confirmation; setup still asks for the organization, team and agents")
	return cmd
}

func (app *App) runSwitch(cmd *cobra.Command, f switchFlags) error {
	out := cmd.OutOrStdout()
	cfg, err := app.loadConfig()
	if err != nil {
		return err
	}
	// Refused before anything changes, so switch never stops halfway.
	if cfg.APIKey != "" {
		return errNoSessionWithAPIKey
	}
	if os.Getenv("TERMA_ORGANIZATION_ID") != "" {
		return errors.New("TERMA_ORGANIZATION_ID is set and would choose the organization for you; unset it first")
	}
	// A team named up front belongs to the organization being left, and fails only once
	// the machine is signed out.
	if cfg.ProjectID != "" {
		return errors.New("a team is named (--team or TERMA_TEAM_ID); switch asks for the team after you sign in, so drop it first")
	}
	if !app.canAsk() {
		return errors.New("switch needs a terminal: after signing out, setup asks for the organization, team and agents")
	}
	if !f.assumeYes {
		ok, err := confirm(cmd, "Sign out of this machine and set it up again, as another account, organization or team?")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(out, "Cancelled. Nothing was changed.")
			return nil
		}
	}
	if err := app.signOut(cmd); err != nil {
		return err
	}
	// The relay keeps the sign-in it started with; setup starts it again on the new one.
	if dir, err := daemon.Dir(app.stateDir); err == nil {
		daemon.Stop(dir)
	}
	if err := config.UpdateProfile(app.dir, cfg.ProfileName, func(p *config.Profile) {
		p.OrganizationID, p.OrganizationName, p.Team = "", "", ""
	}); err != nil {
		return err
	}
	fmt.Fprintln(out, "In the browser, approve as the account you want: if the Terma app there is signed in as another, sign out of it first or use a private window.")
	fmt.Fprintln(out)
	return app.runSetup(cmd, setupFlags{noBrowser: f.noBrowser})
}
