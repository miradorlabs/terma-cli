package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// teamsChange is one run's changes, resolved against the routing state and the list.
type teamsChange struct {
	ops      []bindingOp
	untick   []routing.Team
	released map[string]bool
	ticks    []*tick
	changes  []string
	warnings []string
}

// applyTeams makes f's changes as one routing-state write: the decisions first, then the
// unticked and released teams, then the ticked ones, each ready before the lock is taken.
// A ticked team gets its key once it has its enrollment; one that cannot is taken off
// again, with what was decided for it, so a failed tick changes nothing.
func (app *App) applyTeams(cmd *cobra.Command, cfg *config.Config, st routing.State, f teamsFlags) (teamsView, error) {
	ctx := cmd.Context()
	names := loadTeamNames(app.stateDir, cfg.ProfileName)
	ch := &teamsChange{released: map[string]bool{}}
	// --yes takes every default, so it asks nothing even on a terminal.
	ask := !f.json && !f.assumeYes && app.canAsk() && canPrompt()
	var v teamsView
	listed := false
	list := func() teamsView {
		if !listed {
			v, listed = app.listTeams(ctx, cfg, st), true
			names = loadTeamNames(app.stateDir, cfg.ProfileName)
		}
		return v
	}
	if err := resolveBindingFlags(cmd, st, f, ask, names, list, ch); err != nil {
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
			ch.untick = append(ch.untick, t)
			ch.released[t.EnrollmentID] = group.release
		}
	}
	// What the flags decide comes first, so a ticked team is asked only what is left.
	planned := st
	planned.Bindings = cloneBindings(st.Bindings)
	for _, op := range ch.ops {
		if err := op.apply(&planned); err != nil {
			return teamsView{}, err
		}
	}
	for _, ref := range f.selects {
		o, t, err := unselectedTeam(list(), ref)
		if err != nil {
			return teamsView{}, err
		}
		tk, err := app.prepareTick(cmd, cfg, o, t, f.noBrowser)
		if err != nil {
			return teamsView{}, err
		}
		if err := decideBindings(planned, tk, tickAsker{cmd: cmd, ask: ask, assumeYes: f.assumeYes}); err != nil {
			return teamsView{}, err
		}
		for _, op := range tk.ops {
			_ = op.apply(&planned)
		}
		ch.ticks = append(ch.ticks, tk)
	}
	next, err := routing.Update(app.stateDir, cfg.ProfileName, func(s *routing.State) error { return ch.write(s) })
	if err != nil {
		return teamsView{}, err
	}
	failed := app.finishTicks(cmd, cfg, st, ch)
	if after, ok, err := routing.Load(app.stateDir, cfg.ProfileName); err == nil && ok {
		next = after
	}
	if listed {
		v.mergeState(next, loadTeamNames(app.stateDir, cfg.ProfileName), time.Now())
	} else {
		v = app.listTeams(ctx, cfg, next)
	}
	// The listing has named what it found: say what was unticked by name.
	app.finishUnticks(next, ch, loadTeamNames(app.stateDir, cfg.ProfileName))
	v.Changes, v.Warnings = append(v.Changes, ch.changes...), append(v.Warnings, ch.warnings...)
	return v, failed
}

// write makes ch on s: the decisions, then the unticked teams, then the ticked ones.
func (ch *teamsChange) write(s *routing.State) error {
	for _, op := range ch.ops {
		if err := op.apply(s); err != nil {
			return err
		}
	}
	for _, t := range ch.untick {
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
		tk.enrollmentID = id
	}
	return nil
}

// finishTicks gives each ticked team its key, and takes one that gets none off again with
// the decisions made for it, back to before. It returns the first failure.
func (app *App) finishTicks(cmd *cobra.Command, cfg *config.Config, before routing.State, ch *teamsChange) error {
	var failed error
	for _, tk := range ch.ticks {
		err := app.mintTeamKey(cmd.Context(), cfg, tk)
		if err == nil {
			ch.changes = append(ch.changes, "ticked "+tickLabel(tk))
			for _, op := range tk.ops {
				ch.changes = append(ch.changes, describeOp(op, tk.orgName))
			}
			ch.warnings = append(ch.warnings, tk.notes...)
			continue
		}
		_, undoErr := routing.Update(app.stateDir, cfg.ProfileName, func(s *routing.State) error {
			s.Deselect(tk.enrollmentID)
			for _, op := range tk.ops {
				restoreBinding(s, before, op.key)
			}
			return nil
		})
		_ = keystore.DeleteTeam(app.dir, tk.enrollmentID)
		if failed == nil {
			failed = fmt.Errorf("%s was not ticked: creating its key for this machine failed: %w", tickLabel(tk), err)
			if undoErr != nil {
				failed = errors.Join(failed, fmt.Errorf("untick it with `terma teams --deselect %s`: %w", tk.team.TeamID, undoErr))
			}
		}
	}
	return failed
}

// restoreBinding puts key's decision in s back to what it was in before.
func restoreBinding(s *routing.State, before routing.State, key string) {
	key = routing.Normalize(key)
	if b, ok := before.Bindings[key]; ok {
		if s.Bindings == nil {
			s.Bindings = map[string]routing.Binding{}
		}
		s.Bindings[key] = b
		return
	}
	delete(s.Bindings, key)
}

// finishUnticks deletes each unticked team's key and policy file, and says what is bound
// to an organization no ticked team now collects for: it collects nowhere until unbound.
func (app *App) finishUnticks(next routing.State, ch *teamsChange, names teamNames) {
	for _, t := range ch.untick {
		_ = keystore.DeleteTeam(app.dir, t.EnrollmentID)
		if path, err := config.PolicyPath(app.stateDir, t.TeamID); err == nil {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				ch.warnings = append(ch.warnings, "could not delete "+path+": "+err.Error())
			}
		}
		verb := "unticked "
		if ch.released[t.EnrollmentID] {
			verb = "released "
		}
		ch.changes = append(ch.changes, verb+teamLabel(names, t)+": its key on this machine and what was queued for it are gone")
		if slices.ContainsFunc(next.Teams, func(o routing.Team) bool { return o.OrganizationID == t.OrganizationID }) {
			continue
		}
		for _, k := range sortedBindingKeys(next.Bindings) {
			if b := next.Bindings[k]; !b.Refused && b.Organization == t.OrganizationID {
				ch.warnings = append(ch.warnings, fmt.Sprintf("%s stays bound to %s, so it collects nowhere until you unbind it: `terma teams --unbind %s`",
					k, cmp.Or(names.Orgs[t.OrganizationID], t.OrganizationID), k))
			}
		}
	}
	if len(ch.untick) > 0 && boundToAnOrganization(next) && len(next.Teams) == 1 && next.Teams[0].Personal {
		ch.warnings = append(ch.warnings, "while anything stays bound to an organization, a record whose location is unknown never goes to personal")
	}
}

func boundToAnOrganization(s routing.State) bool {
	for _, b := range s.Bindings {
		if !b.Refused {
			return true
		}
	}
	return false
}

func sortedBindingKeys(m map[string]routing.Binding) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func cloneBindings(m map[string]routing.Binding) map[string]routing.Binding {
	out := make(map[string]routing.Binding, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func tickLabel(tk *tick) string { return orgTeamLabel(tk.orgName, tk.teamName) }

// describeOp says what a decision did.
func describeOp(op bindingOp, orgName string) string {
	switch {
	case op.refuse:
		return "refused " + op.key + " to every organization"
	case op.org != "":
		return "bound " + op.key + " to " + cmp.Or(orgName, op.org)
	}
	return "unbound " + op.key
}

// selectedTeam is the ticked team ref names: by team or enrollment id, else by name.
func selectedTeam(st routing.State, ref string, names teamNames, list func() teamsView) (routing.Team, error) {
	for _, t := range st.Teams {
		if t.TeamID == ref || t.EnrollmentID == ref {
			return t, nil
		}
	}
	candidates := make([]project, 0, len(st.Teams))
	for _, t := range st.Teams {
		name := names.Teams[t.TeamID]
		if name == "" {
			name = list().teamName(t.TeamID)
		}
		candidates = append(candidates, project{ID: t.TeamID, Name: name})
	}
	p, err := matchTicked(candidates, ref)
	if err != nil {
		return routing.Team{}, err
	}
	for _, t := range st.Teams {
		if t.TeamID == p.ID {
			return t, nil
		}
	}
	return routing.Team{}, fmt.Errorf("no ticked team matches %q", ref)
}

func matchTicked(candidates []project, ref string) (*project, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no ticked team matches %q: none is ticked", ref)
	}
	return matchKind[project]{noun: "ticked team", id: projectKind.id, name: projectKind.name}.match(candidates, ref)
}

// teamName is team id's name as listed, else "".
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

// unselectedTeam is the listed team ref names, by id or name, which must not be ticked yet.
func unselectedTeam(v teamsView, ref string) (orgRow, teamRow, error) {
	var all []project
	var orgOf []int
	for i, o := range v.Organizations {
		for _, t := range o.Teams {
			all = append(all, project{ID: t.ID, Name: t.Name})
			orgOf = append(orgOf, i)
		}
	}
	idx, err := matchKind[project]{noun: "team", id: projectKind.id, name: projectKind.name}.index(all, ref)
	if err != nil {
		if unsigned := v.unsignedOrgs(); len(unsigned) > 0 {
			err = fmt.Errorf("%w; the teams of %s are listed once you sign in there: `terma teams --org <organization> --select %s`", err, strings.Join(unsigned, ", "), quoteArg(ref))
		}
		return orgRow{}, teamRow{}, err
	}
	o := v.Organizations[orgOf[idx]]
	t := o.Teams[slices.IndexFunc(o.Teams, func(t teamRow) bool { return t.ID == all[idx].ID })]
	if t.Selected {
		return orgRow{}, teamRow{}, fmt.Errorf("%s %s is already ticked", orgTitle(o), cmp.Or(t.Name, t.ID))
	}
	if t.Unreadable != "" {
		return orgRow{}, teamRow{}, fmt.Errorf("%s's policy could not be read (%s), so it cannot be ticked", cmp.Or(t.Name, t.ID), t.Unreadable)
	}
	return o, t, nil
}

// unsignedOrgs names the organizations this machine has no sign-in for.
func (v teamsView) unsignedOrgs() []string {
	var out []string
	for _, o := range v.Organizations {
		if !o.SignedIn && len(o.Teams) == 0 {
			out = append(out, orgTitle(o))
		}
	}
	return out
}
