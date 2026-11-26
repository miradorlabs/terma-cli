package cli

import (
	"cmp"
	"errors"
	"fmt"
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
	untick            []routing.Team
	released          map[string]bool
	ticks             []*tick
	changes, warnings []string
}

// applyTeamsWithView makes f's changes against st as one CAS routing-state write
// (st.ExpectConsent): unticks/releases, then ticks. No keys are minted.
func (app *App) applyTeamsWithView(cmd *cobra.Command, cfg *config.Config, st routing.State, v teamsView, listed bool, f teamsFlags) (teamsView, error) {
	ctx := cmd.Context()
	names := loadTeamNames(app.stateDir, cfg.ProfileName)
	ch := &teamsChange{released: map[string]bool{}}

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

	for _, g := range []struct {
		refs []string
		rel  bool
	}{{f.deselects, false}, {f.releases, true}} {
		for _, ref := range g.refs {
			t, err := selectedTeam(st, ref, names, list)
			if err != nil {
				return teamsView{}, err
			}
			if state := t.StateAt(time.Now()); g.rel && state != routing.Suspended && state != routing.Refused {
				return teamsView{}, fmt.Errorf("%s is %s, not suspended or refused: `terma teams --deselect %s` unticks it", teamLabel(names, t), strings.ReplaceAll(string(state), "_", " "), quoteArg(ref))
			}
			if !slices.ContainsFunc(ch.untick, func(u routing.Team) bool { return u.EnrollmentID == t.EnrollmentID }) {
				ch.untick, ch.released[t.EnrollmentID] = append(ch.untick, t), g.rel
			}
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
			if err := routing.StoreTeamPolicy(app.stateDir, committed); err != nil {
				return teamsView{}, fmt.Errorf("save policy for %s: %w", orgTeamLabel(tk.orgName, tk.teamName), err)
			}
			if r := v.findTeam(committed.OrganizationID, committed.TeamID); r != nil {
				r.Entries, r.Collects, r.GitHooks, r.Unreadable = committed.Policy.Repositories, collectsSummary(committed.Policy, committed.Personal), committed.Policy.GitHooks, ""
			}
		}
		ch.changes = append(ch.changes, "ticked "+orgTeamLabel(tk.orgName, tk.teamName))
		ch.warnings = append(ch.warnings, tk.notes...)
	}
	if listed {
		v.mergeState(next, loadTeamNames(app.stateDir, cfg.ProfileName), time.Now())
	} else {
		c, cerr := getClient()
		v = app.listTeamsWithClient(ctx, cfg, next, c, cerr)
	}
	app.finishUnticks(ch, loadTeamNames(app.stateDir, cfg.ProfileName))
	v.Changes, v.Warnings, v.state = append(v.Changes, ch.changes...), append(v.Warnings, ch.warnings...), next
	return v, nil
}

func formatCASConflict(err error) error {
	switch {
	case errors.Is(err, routing.ErrAccountMismatch):
		return fmt.Errorf("%w: this machine signed in as another account while `terma teams` was running; nothing was changed — run `terma setup`", err)
	case errors.Is(err, routing.ErrStaleGeneration):
		return fmt.Errorf("%w: teams changed while `terma teams` was running; nothing was changed — run `terma teams` again", err)
	}
	return err
}

func (ch *teamsChange) write(s *routing.State, now time.Time) error {
	for _, t := range ch.untick {
		if live, ok := s.Team(t.EnrollmentID); !ok {
			return fmt.Errorf("team %s was unticked meanwhile", t.TeamID)
		} else if state := live.StateAt(now); ch.released[t.EnrollmentID] && state != routing.Suspended && state != routing.Refused {
			return fmt.Errorf("team %s is now %s, not suspended or refused: `terma teams --deselect %s` unticks it", t.TeamID, strings.ReplaceAll(string(state), "_", " "), quoteArg(t.TeamID))
		}
		s.Deselect(t.EnrollmentID)
	}
	for _, tk := range ch.ticks {
		var err error
		if tk.enrollment, err = s.Select(tk.team); err != nil {
			return err
		}
	}
	return nil
}

func (app *App) finishUnticks(ch *teamsChange, names teamNames) {
	for _, t := range ch.untick {
		verb := "unticked "
		if ch.released[t.EnrollmentID] {
			verb = "released "
		}
		ch.changes = append(ch.changes, verb+teamLabel(names, t)+": what was queued for it will be discarded")
	}
}

func matchUniqueRef[T any](k matchKind[T], items []T, ref string, label func(T) string) (int, error) {
	q := strings.TrimSpace(ref)
	var exact []int
	for i := range items {
		if k.id(items[i]) == q {
			return i, nil
		}
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
	live, candidates := list(), make([]project, 0, len(st.Teams))
	for _, t := range st.Teams {
		name := names.Teams[t.TeamID]
		if r := live.findTeam(t.OrganizationID, t.TeamID); r != nil && r.Name != "" {
			name = r.Name
		}
		candidates = append(candidates, project{ID: t.TeamID, Name: name})
	}
	k := matchKind[project]{noun: "ticked team", id: projectKind.id, name: projectKind.name}
	idx, err := matchUniqueRef(k, candidates, ref, func(p project) string { return fmt.Sprintf("%s (%s)", cmp.Or(p.Name, p.ID), p.ID) })
	if err != nil {
		return routing.Team{}, err
	}
	return st.Teams[idx], nil
}

func unselectedTeam(v teamsView, ref string) (orgRow, teamRow, error) {
	type candidate struct {
		org  orgRow
		team teamRow
	}
	var all []candidate
	for _, o := range v.Organizations {
		for _, t := range o.Teams {
			all = append(all, candidate{o, t})
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
