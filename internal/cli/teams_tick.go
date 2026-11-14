package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

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
	// ops are the bindings the developer decided for it, made with it.
	ops []bindingOp
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

// tickAsker is how ticking asks about a binding: nil when no one can be asked.
type tickAsker struct {
	cmd       *cobra.Command
	ask       bool
	assumeYes bool
}

// decideBindings works out the bindings tk's policy needs that have no decision in st yet,
// and asks the developer for each: bind it to the team's organization, refuse it, or decide
// later, which holds what it matches. An entry under an owner decided otherwise is
// skipped, never offered. An organization listing only exact entries is offered its owners
// too, the listed repositories alone being the default. Without anyone to ask, --yes takes
// the defaults and anything else leaves them to decide.
func decideBindings(st routing.State, tk *tick, a tickAsker) error {
	t := tk.team
	if t.Personal || t.Policy.Global() {
		return nil
	}
	var need []routing.Entry
	exactOnly := true
	for _, raw := range t.Policy.Repositories {
		e, ok := routing.ParseEntry(raw, t.Policy.Wildcards)
		if !ok || slices.ContainsFunc(need, func(n routing.Entry) bool { return n.Key() == e.Key() }) {
			continue
		}
		exactOnly = exactOnly && !e.Wildcard()
		switch who, decided := decisionFor(st, e); {
		case decided && who == t.OrganizationID:
		case decided:
			tk.notes = append(tk.notes, raw+" is skipped: "+who)
		default:
			need = append(need, e)
		}
	}
	if len(need) == 0 {
		return nil
	}
	if exactOnly {
		return decideOwners(tk, need, a)
	}
	for _, e := range need {
		op, err := askBinding(a, tk, e.Key(), "needed by "+e.Raw)
		if err != nil {
			return err
		}
		if op != nil {
			tk.ops = append(tk.ops, *op)
		}
	}
	return nil
}

// decisionFor is the decision covering what e needs: who it is bound to (an organization
// id), or why it is skipped, and whether there is one at all.
func decisionFor(st routing.State, e routing.Entry) (string, bool) {
	keys := []string{e.Key()}
	if !e.Wildcard() {
		keys = append(keys, routing.Owner(e.Key()))
	}
	for _, k := range keys {
		if b, ok := st.Bindings[k]; ok {
			if b.Refused {
				return k + " is refused to every organization", true
			}
			return b.Organization, true
		}
	}
	return "", false
}

// decideOwners offers, for each owner of an exact-only team's listed repositories, to bind
// just those repositories (the default) or the whole owner.
func decideOwners(tk *tick, need []routing.Entry, a tickAsker) error {
	var owners []string
	for _, e := range need {
		if o := routing.Owner(e.Key()); !slices.Contains(owners, o) {
			owners = append(owners, o)
		}
	}
	for _, owner := range owners {
		var repos []string
		for _, e := range need {
			if routing.Owner(e.Key()) == owner {
				repos = append(repos, e.Key())
			}
		}
		whole := false
		switch {
		case a.ask:
			i, err := pick(a.cmd, fmt.Sprintf("%s lists %d of %s's repositories. Bind to %s:", tk.teamName, len(repos), owner, cmp.Or(tk.orgName, tk.team.OrganizationID)), []pickRow{
				{Label: "just the listed repositories", Note: "the owner's other repositories stay free: personal may collect them", Default: true},
				{Label: "the whole owner " + owner, Note: "every repository under it belongs to " + cmp.Or(tk.orgName, "the organization")},
				{Label: "decide later", Note: "what they match is held until you decide"},
			}, func(string) (int, error) { return -1, errors.New("answer with a number") })
			if err != nil {
				return err
			}
			if i == 2 {
				continue
			}
			whole = i == 1
		case !a.assumeYes:
			continue
		}
		if whole {
			tk.ops = append(tk.ops, bindingOp{key: owner, org: tk.team.OrganizationID})
			continue
		}
		for _, r := range repos {
			tk.ops = append(tk.ops, bindingOp{key: r, org: tk.team.OrganizationID})
		}
	}
	return nil
}

// askBinding asks how to decide key, which tk's entry needs: nil leaves it to decide later.
func askBinding(a tickAsker, tk *tick, key, why string) (*bindingOp, error) {
	org := cmp.Or(tk.orgName, tk.team.OrganizationID)
	switch {
	case a.ask:
		i, err := pick(a.cmd, fmt.Sprintf("%s (%s):", key, why), []pickRow{
			{Label: "bind it to " + org, Note: "only " + org + " collects it; personal never does", Default: true},
			{Label: "refuse it", Note: "no organization collects it; personal may"},
			{Label: "decide later", Note: "what it matches is held until you decide"},
		}, func(string) (int, error) { return -1, errors.New("answer with a number") })
		switch {
		case err != nil:
			return nil, err
		case i == 0:
			return &bindingOp{key: key, org: tk.team.OrganizationID}, nil
		case i == 1:
			return &bindingOp{key: key, refuse: true}, nil
		}
		return nil, nil
	case a.assumeYes:
		return &bindingOp{key: key, org: tk.team.OrganizationID}, nil
	}
	return nil, nil
}
