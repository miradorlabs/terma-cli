package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

type tick struct {
	team         routing.Team
	orgName      string
	teamName     string
	ops          []bindingOp
	notes        []string
	enrollmentID string
}

type bindingOp struct {
	key      string
	org      string
	refuse   bool
	override bool
}

func (op bindingOp) apply(s *routing.State) error {
	switch {
	case op.refuse:
		return s.Refuse(op.key)
	case op.org != "":
		return s.Bind(op.key, op.org, op.override)
	}
	return s.Unbind(op.key)
}

// prepareTick fetches t's policy on the shared user-token client. Only a personal
// organization may collect everything; non-personal global policies collect nothing.
func prepareTick(ctx context.Context, client *api.Client, authURL string, o orgRow, t teamRow) (*tick, error) {
	personal := o.Type == "personal"
	pol, err := fetchTeamPolicy(ctx, client, authURL, o.ID, t.ID)
	switch {
	case teamNotFound(err):
		return nil, fmt.Errorf("team %s is no longer in %s", cmp.Or(t.Name, t.ID), orgTitle(o))
	case orgNotAccessibleErr(err):
		return nil, fmt.Errorf("you no longer have access to %s, so %s cannot be ticked", orgTitle(o), cmp.Or(t.Name, t.ID))
	case err != nil:
		return nil, fmt.Errorf("fetch %s's policy: %w", cmp.Or(t.Name, t.ID), err)
	}
	tk := &tick{orgName: o.Name, teamName: cmp.Or(t.Name, t.ID)}
	if pol.Global() && !personal {
		pol.Mode, pol.Repositories = config.ModeRepo, nil
	}
	if !pol.Global() && len(pol.Repositories) == 0 {
		tk.notes = append(tk.notes, tk.teamName+" collects nothing until an admin chooses its repositories")
	}
	tk.team = routing.Team{OrganizationID: o.ID, AuthURL: authURL, TeamID: t.ID, Personal: personal, Policy: pol}
	return tk, nil
}

type tickAsker struct {
	cmd       *cobra.Command
	ask       bool
	assumeYes bool
}

// decideBindings resolves the undecided bindings tk's policy needs: prompts interactively,
// takes defaults under --yes, or leaves them pending.
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
