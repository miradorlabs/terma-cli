package cli

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

type teamsChange struct {
	ops               []bindingOp
	untick            []routing.Team
	released          map[string]bool
	ticks             []*tick
	changes, warnings []string
}

// applyTeamsWithView makes f's changes against st as one CAS routing-state write
// (st.ExpectConsent): bindings, then unticks/releases, then ticks. No keys are minted.
func (app *App) applyTeamsWithView(cmd *cobra.Command, cfg *config.Config, st routing.State, v teamsView, listed bool, f teamsFlags) (teamsView, error) {
	ctx := cmd.Context()
	names := loadTeamNames(app.stateDir, cfg.ProfileName)
	ch := &teamsChange{released: map[string]bool{}}
	ask := !f.json && !f.assumeYes && app.canAsk()

	getClient := sync.OnceValues(func() (*api.Client, error) {
		return app.teamsAPIClient(cfg, st.Account)
	})
	list := func() teamsView {
		if !listed {
			c, cerr := getClient()
			v, listed = app.listTeamsWithClient(ctx, cfg, st, c, cerr), true
			names = loadTeamNames(app.stateDir, cfg.ProfileName)
		}
		return v
	}

	planned := st
	planned.Bindings = maps.Clone(st.Bindings)
	if err := resolveBindingFlags(cmd, &planned, f, ask, &names, list, ch); err != nil {
		return teamsView{}, err
	}
	for _, group := range []struct {
		refs    []string
		release bool
	}{{f.deselects, false}, {f.releases, true}} {
		for _, ref := range group.refs {
			t, err := selectedTeam(st, ref, names, list)
			if err != nil {
				return teamsView{}, err
			}
			if state := t.StateAt(time.Now()); group.release && state != routing.Suspended && state != routing.Refused {
				return teamsView{}, fmt.Errorf("%s is %s, not suspended or refused: `terma teams --deselect %s` unticks it", teamLabel(names, t), strings.ReplaceAll(string(state), "_", " "), quoteArg(ref))
			}
			if slices.ContainsFunc(ch.untick, func(u routing.Team) bool { return u.EnrollmentID == t.EnrollmentID }) {
				continue
			}
			ch.untick = append(ch.untick, t)
			ch.released[t.EnrollmentID] = group.release
		}
	}
	if len(f.selects) > 0 {
		c, err := getClient()
		if err != nil {
			return teamsView{}, err
		}
		for _, ref := range f.selects {
			o, t, err := unselectedTeam(list(), ref)
			if err != nil {
				return teamsView{}, err
			}
			if slices.ContainsFunc(ch.ticks, func(tk *tick) bool { return tk.team.TeamID == t.ID }) {
				continue
			}
			tk, err := prepareTick(ctx, c, cfg.AuthURL, o, t)
			if err != nil {
				return teamsView{}, err
			}
			if err := decideBindings(&planned, tk, tickAsker{cmd: cmd, ask: ask, assumeYes: f.assumeYes, names: names, pick: app.pickOne}); err != nil {
				return teamsView{}, err
			}
			ch.ticks = append(ch.ticks, tk)
		}
	}

	next, err := routing.Update(app.stateDir, cfg.ProfileName, st.ExpectConsent(), func(s *routing.State) error {
		return ch.write(s, time.Now())
	})
	if err != nil {
		return teamsView{}, formatCASConflict(err)
	}

	for _, tk := range ch.ticks {
		if committed, ok := next.Team(tk.enrollment); ok {
			_ = routing.StoreTeamPolicy(app.stateDir, committed)
		}
		ch.changes = append(ch.changes, "ticked "+orgTeamLabel(tk.orgName, tk.teamName))
		for _, op := range tk.ops {
			ch.changes = append(ch.changes, describeOp(op, tk.orgName))
		}
		ch.warnings = append(ch.warnings, tk.notes...)
	}
	if listed {
		v.mergeState(next, loadTeamNames(app.stateDir, cfg.ProfileName), time.Now())
	} else {
		c, cerr := getClient()
		v = app.listTeamsWithClient(ctx, cfg, next, c, cerr)
	}
	app.finishUnticks(next, ch, loadTeamNames(app.stateDir, cfg.ProfileName))
	v.Changes, v.Warnings = append(v.Changes, ch.changes...), append(v.Warnings, ch.warnings...)
	return v, nil
}

func formatCASConflict(err error) error {
	switch {
	case errors.Is(err, routing.ErrAccountMismatch):
		return fmt.Errorf("%w: this machine signed in as another account while `terma teams` was running; nothing was changed — run `terma setup`", err)
	case errors.Is(err, routing.ErrStaleGeneration):
		return fmt.Errorf("%w: teams or bindings changed while `terma teams` was running; nothing was changed — run `terma teams` again", err)
	}
	return err
}

func (ch *teamsChange) write(s *routing.State, now time.Time) error {
	for _, op := range ch.ops {
		if err := op.apply(s); err != nil {
			return err
		}
	}
	for _, t := range ch.untick {
		if ch.released[t.EnrollmentID] {
			live, ok := s.Team(t.EnrollmentID)
			if !ok {
				return fmt.Errorf("team %s was unticked meanwhile", t.TeamID)
			}
			if state := live.StateAt(now); state != routing.Suspended && state != routing.Refused {
				return fmt.Errorf("team %s is now %s, not suspended or refused: `terma teams --deselect %s` unticks it", t.TeamID, strings.ReplaceAll(string(state), "_", " "), quoteArg(t.TeamID))
			}
		}
		if !s.Deselect(t.EnrollmentID) {
			return fmt.Errorf("team %s was unticked meanwhile", t.TeamID)
		}
	}
	for _, tk := range ch.ticks {
		for _, op := range tk.ops {
			if err := op.apply(s); err != nil {
				return err
			}
		}
		id, err := s.Select(tk.team)
		if err != nil {
			return err
		}
		tk.enrollment = id
	}
	return nil
}

func (app *App) finishUnticks(next routing.State, ch *teamsChange, names teamNames) {
	for _, t := range ch.untick {
		verb := "unticked "
		if ch.released[t.EnrollmentID] {
			verb = "released "
		}
		ch.changes = append(ch.changes, verb+teamLabel(names, t)+": what was queued for it will be discarded")
		if slices.ContainsFunc(next.Teams, func(o routing.Team) bool { return o.OrganizationID == t.OrganizationID }) {
			continue
		}
		for _, k := range slices.Sorted(maps.Keys(next.Bindings)) {
			if b := next.Bindings[k]; !b.Refused && b.Organization == t.OrganizationID {
				ch.warnings = append(ch.warnings, fmt.Sprintf("%s stays bound to %s, so it collects nowhere until you unbind it: `terma teams --unbind %s`",
					k, cmp.Or(names.Orgs[t.OrganizationID], t.OrganizationID), quoteArg(k)))
			}
		}
	}
	if len(ch.untick) > 0 && len(next.Teams) == 1 && next.Teams[0].Personal && slices.ContainsFunc(slices.Collect(maps.Values(next.Bindings)), func(b routing.Binding) bool { return !b.Refused }) {
		ch.warnings = append(ch.warnings, "while anything stays bound to an organization, a record whose location is unknown never goes to personal")
	}
}

func describeOp(op bindingOp, orgName string) string {
	switch {
	case op.refuse:
		return "refused " + op.key + " to every organization"
	case op.org != "":
		return "bound " + op.key + " to " + cmp.Or(orgName, op.org)
	}
	return "unbound " + op.key
}

func matchUniqueRef[T any](k matchKind[T], items []T, ref string, label func(T) string) (int, error) {
	q := strings.TrimSpace(ref)
	for i := range items {
		if k.id(items[i]) == q {
			return i, nil
		}
	}
	var exact []int
	for i := range items {
		if strings.EqualFold(k.name(items[i]), q) {
			exact = append(exact, i)
		}
	}
	if len(exact) > 1 {
		labels := make([]string, len(exact))
		for i, m := range exact {
			labels[i] = label(items[m])
		}
		return -1, fmt.Errorf("%q matches several %ss (%s) — use the id", q, k.noun, strings.Join(labels, ", "))
	}
	return k.index(items, q)
}

func selectedTeam(st routing.State, ref string, names teamNames, list func() teamsView) (routing.Team, error) {
	for _, t := range st.Teams {
		if t.TeamID == ref || t.EnrollmentID == ref {
			return t, nil
		}
	}
	if len(st.Teams) == 0 {
		return routing.Team{}, fmt.Errorf("no ticked team matches %q: none is ticked", ref)
	}
	live := list()
	candidates := make([]project, 0, len(st.Teams))
	for _, t := range st.Teams {
		candidates = append(candidates, project{ID: t.TeamID, Name: cmp.Or(live.teamName(t.TeamID), names.Teams[t.TeamID])})
	}
	k := matchKind[project]{noun: "ticked team", id: projectKind.id, name: projectKind.name}
	idx, err := matchUniqueRef(k, candidates, ref, func(p project) string {
		return fmt.Sprintf("%s (%s)", cmp.Or(p.Name, p.ID), p.ID)
	})
	if err != nil {
		return routing.Team{}, err
	}
	return st.Teams[idx], nil
}

func (v teamsView) teamName(id string) string {
	for _, o := range v.Organizations {
		for _, t := range o.Teams {
			if t.ID == id {
				return t.Name
			}
		}
	}
	return ""
}

func unselectedTeam(v teamsView, ref string) (orgRow, teamRow, error) {
	type candidate struct {
		org  orgRow
		team teamRow
	}
	var all []candidate
	for _, o := range v.Organizations {
		for _, t := range o.Teams {
			all = append(all, candidate{org: o, team: t})
		}
	}
	k := matchKind[candidate]{noun: "team", id: func(c candidate) string { return c.team.ID }, name: func(c candidate) string { return c.team.Name }}
	idx, err := matchUniqueRef(k, all, ref, func(c candidate) string {
		return fmt.Sprintf("%s (%s)", orgTeamLabel(orgTitle(c.org), cmp.Or(c.team.Name, c.team.ID)), c.team.ID)
	})
	if err != nil {
		var inaccessible []string
		for _, o := range v.Organizations {
			if o.Inaccessible {
				inaccessible = append(inaccessible, orgTitle(o))
			}
		}
		if len(inaccessible) > 0 {
			err = fmt.Errorf("%w (inaccessible organizations not listed: %s)", err, strings.Join(inaccessible, ", "))
		}
		return orgRow{}, teamRow{}, err
	}
	o, t := all[idx].org, all[idx].team
	switch {
	case o.Inaccessible:
		return orgRow{}, teamRow{}, fmt.Errorf("you no longer have access to %s, so %s cannot be ticked", orgTitle(o), cmp.Or(t.Name, t.ID))
	case t.Selected:
		return orgRow{}, teamRow{}, fmt.Errorf("%s %s is already ticked", orgTitle(o), cmp.Or(t.Name, t.ID))
	case t.Unreadable != "":
		return orgRow{}, teamRow{}, fmt.Errorf("%s's policy could not be read (%s), so it cannot be ticked", cmp.Or(t.Name, t.ID), t.Unreadable)
	}
	return o, t, nil
}
