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
	team                          routing.Team
	orgName, teamName, enrollment string
	ops                           []bindingOp
	notes                         []string
}

type bindingOp struct {
	key, org         string
	refuse, override bool
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
	cmd            *cobra.Command
	ask, assumeYes bool
	names          teamNames
	pick           func(*cobra.Command, string, []pickRow, func(string) (int, error)) (int, error)
}

func (a tickAsker) choose(title string, rows []pickRow) (int, error) {
	if a.pick == nil {
		a.pick = pick
	}
	return a.pick(a.cmd, title, rows, func(string) (int, error) { return -1, errors.New("answer with a number") })
}

// decideBindings resolves the undecided bindings tk's policy needs: prompts interactively,
// takes defaults under --yes, or leaves them pending.
func decideBindings(planned *routing.State, tk *tick, a tickAsker) error {
	t := tk.team
	if t.Personal || t.Policy.Global() {
		return nil
	}
	var need, skipped []routing.Entry
	exactOnly := true
	for _, raw := range t.Policy.Repositories {
		e, ok := routing.ParseEntry(raw, t.Policy.Wildcards)
		if !ok || slices.ContainsFunc(need, func(n routing.Entry) bool { return n.Key() == e.Key() }) {
			continue
		}
		exactOnly = exactOnly && !e.Wildcard()
		switch who, decided := decisionFor(*planned, e); {
		case decided && who == t.OrganizationID:
		case decided:
			skipped = append(skipped, e)
		default:
			need = append(need, e)
		}
	}
	if len(need) > 0 && exactOnly {
		if err := decideOwners(planned, tk, need, a); err != nil {
			return err
		}
	} else {
		for _, e := range need {
			if op, err := askBinding(a, tk, e.Key(), "needed by "+e.Raw); err != nil {
				return err
			} else if op != nil {
				if err := addTickOp(planned, tk, a, *op); err != nil {
					return err
				}
			}
		}
	}
	for _, e := range skipped {
		if who, decided := decisionFor(*planned, e); decided && who != t.OrganizationID {
			tk.notes = append(tk.notes, e.Raw+" is skipped: "+who)
		}
	}
	return nil
}

func addTickOp(planned *routing.State, tk *tick, a tickAsker, op bindingOp) error {
	if routing.Owner(op.key) == "" {
		if err := keepRepositoryDecisions(a.cmd, planned, op.key, op.org, tk.orgName, op.refuse, a.names, a.ask, &tk.ops, nil, &tk.notes); err != nil {
			return err
		}
	}
	if err := op.apply(planned); err != nil {
		return err
	}
	tk.ops = append(tk.ops, op)
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

func decideOwners(planned *routing.State, tk *tick, need []routing.Entry, a tickAsker) error {
	var owners []string
	byOwner := map[string][]string{}
	for _, e := range need {
		o := routing.Owner(e.Key())
		if len(byOwner[o]) == 0 {
			owners = append(owners, o)
		}
		byOwner[o] = append(byOwner[o], e.Key())
	}
	for _, owner := range owners {
		keys := byOwner[owner]
		if a.ask {
			i, err := a.choose(fmt.Sprintf("%s lists %d of %s's repositories. Bind to %s:", tk.teamName, len(keys), owner, cmp.Or(tk.orgName, tk.team.OrganizationID)), []pickRow{
				{Label: "just the listed repositories", Note: "the owner's other repositories stay free: personal may collect them", Default: true},
				{Label: "the whole owner " + owner, Note: "every repository under it belongs to " + cmp.Or(tk.orgName, "the organization")},
				{Label: "decide later", Note: "what they match is held until you decide"},
			})
			switch {
			case err != nil:
				return err
			case i == 2:
				continue
			case i == 1:
				keys = []string{owner}
			}
		} else if !a.assumeYes {
			continue
		}
		for _, k := range keys {
			if err := addTickOp(planned, tk, a, bindingOp{key: k, org: tk.team.OrganizationID}); err != nil {
				return err
			}
		}
	}
	return nil
}

func askBinding(a tickAsker, tk *tick, key, why string) (*bindingOp, error) {
	if !a.ask {
		if a.assumeYes {
			return &bindingOp{key: key, org: tk.team.OrganizationID}, nil
		}
		return nil, nil
	}
	org := cmp.Or(tk.orgName, tk.team.OrganizationID)
	i, err := a.choose(fmt.Sprintf("%s (%s):", key, why), []pickRow{
		{Label: "bind it to " + org, Note: "only " + org + " collects it; personal never does", Default: true},
		{Label: "refuse it", Note: "no organization collects it; personal may"},
		{Label: "decide later", Note: "what it matches is held until you decide"},
	})
	if err != nil || i == 2 {
		return nil, err
	}
	if i == 1 {
		return &bindingOp{key: key, refuse: true}, nil
	}
	return &bindingOp{key: key, org: tk.team.OrganizationID}, nil
}
