package cli

import (
	"cmp"
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// tick is a team made ready to tick: signed in, its policy fetched and its bindings
// decided. Only once all of that is done is it added, under a new enrollment.
type tick struct {
	team     routing.Team
	orgName  string
	teamName string
	cred     *auth.Credential
	// notes say what its policy needs that it will not collect, or what it collects nothing of.
	notes []string
	// enrollmentID is its enrollment once selected.
	enrollmentID string
}

// bindingOp is one decision about an owner or a repository.
type bindingOp struct {
	key string
	// org binds it; refuse refuses it; neither unbinds it.
	org      string
	refuse   bool
	override bool
}

// apply makes op on s.
func (op bindingOp) apply(s *routing.State) error {
	switch {
	case op.refuse:
		return s.Refuse(op.key)
	case op.org != "":
		return s.Bind(op.key, op.org, op.override)
	}
	return s.Unbind(op.key)
}

// signInTo signs in to the organization ref names, reusing a stored sign-in there, through
// setup's own sign-in flow; one approved in the browser for another organization is refused.
func (app *App) signInTo(cmd *cobra.Command, cfg *config.Config, ref orgRef, noBrowser bool) (*auth.Credential, error) {
	res, err := app.signIn(cmd, cfg, signInOptions{org: ref, noBrowser: noBrowser, pauseBeforeBrowser: canPrompt()})
	if err != nil {
		return nil, err
	}
	if ref.ID != "" && res.cred.OrganizationID != ref.ID {
		return nil, fmt.Errorf("%s was not authorized in the browser, so nothing was ticked there", ref)
	}
	return res.cred, nil
}

// prepareTick signs in to o if this machine has no sign-in there, and fetches t's policy
// with that sign-in. An organization other than a personal one never collects everything:
// such a policy collects nothing until an admin chooses repositories.
func (app *App) prepareTick(cmd *cobra.Command, cfg *config.Config, o orgRow, t teamRow, noBrowser bool) (*tick, error) {
	cred, ok := app.orgSignIn(cfg, o.ID)
	if !ok {
		var err error
		if cred, err = app.signInTo(cmd, cfg, orgRef{ID: o.ID, Name: o.Name}, noBrowser); err != nil {
			return nil, err
		}
	}
	personal := o.Type == "personal"
	pol, err := app.teamPolicy(cmd.Context(), cfg, cred, o.ID, t.ID)
	switch {
	case teamNotFound(err):
		return nil, fmt.Errorf("team %s is no longer in %s", cmp.Or(t.Name, t.ID), orgTitle(o))
	case err != nil:
		return nil, fmt.Errorf("fetch %s's policy: %w", cmp.Or(t.Name, t.ID), err)
	}
	tk := &tick{cred: cred, orgName: o.Name, teamName: cmp.Or(t.Name, t.ID)}
	if pol.Global() && !personal {
		pol.Mode, pol.Repositories = config.ModeRepo, nil
	}
	if !pol.Global() && len(pol.Repositories) == 0 {
		tk.notes = append(tk.notes, tk.teamName+" collects nothing until an admin chooses its repositories")
	}
	tk.team = routing.Team{OrganizationID: o.ID, AuthURL: cfg.AuthURL, TeamID: t.ID, Personal: personal, Policy: pol}
	return tk, nil
}

// mintTeamKey creates the team's key for this machine with its organization's sign-in and
// keeps it under the enrollment (keys.v2.json), through the relay's own minting, which
// serializes creation on the machine: a key the relay made meanwhile is kept.
func (app *App) mintTeamKey(ctx context.Context, cfg *config.Config, tk *tick) error {
	team := tk.team
	team.EnrollmentID = tk.enrollmentID
	return daemon.MintTeamKey(ctx, cfg, team, app.createTeamKey)
}
