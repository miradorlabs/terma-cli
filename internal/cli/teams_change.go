package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

// teamsChange is one run's changes, resolved against the routing state and the list.
type teamsChange struct {
	ops []bindingOp
	// decided are the bindings this run decided for the teams that need them, and orgNames
	// the names of the organizations their needs name.
	decided  []neededOp
	orgNames map[string]string
	untick   []routing.Team
	released map[string]bool
	ticks    []*tick
	changes  []string
	warnings []string
}

// applyTeams makes f's changes as one routing-state write: the decisions first, then the
// unticked and released teams, then the ticked ones, each ready before the lock is taken.
// A ticked team gets its key once it has its enrollment; one that cannot is taken off
// again, with what was decided for it, so a failed tick changes nothing. Both writes check
// their result: an owner this run decided never takes another organization's repository
// that is left undecided.
func (app *App) applyTeams(cmd *cobra.Command, cfg *config.Config, st routing.State, f teamsFlags) (teamsView, error) {
	ctx := cmd.Context()
	names := loadTeamNames(app.stateDir, cfg.ProfileName)
	ch := &teamsChange{released: map[string]bool{}}
	// --yes takes every default, so it asks nothing even on a terminal.
	ask := !f.json && !f.assumeYes && app.canAsk() && (app.runForm != nil || canPrompt())
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
	// What the flags decide comes first, and the teams they untick are gone, so a ticked
	// team is asked only what is left, and an unticked one needs nothing.
	planned := st
	planned.Bindings = maps.Clone(st.Bindings)
	planned.Teams = slices.Clone(st.Teams)
	flagged := map[string]bool{}
	for _, op := range ch.ops {
		if err := op.apply(&planned); err != nil {
			return teamsView{}, err
		}
		flagged[op.key] = true
	}
	for _, t := range ch.untick {
		planned.Deselect(t.EnrollmentID)
	}
	var needs []bindingNeed
	for _, ref := range f.selects {
		o, t, err := unselectedTeam(list(), ref)
		if err != nil {
			return teamsView{}, err
		}
		tk, err := app.prepareTick(cmd, cfg, o, t, f.noBrowser)
		if err != nil {
			return teamsView{}, err
		}
		needs = append(needs, tickNeeds(planned, tk)...)
		ch.ticks = append(ch.ticks, tk)
	}
	// Every undecided binding is weighed together, earlier ones included, so a key two
	// organizations need is one question, never settled by whichever was ticked last.
	mode := app.bindingMode(f, ask)
	needs = append(pendingNeeds(planned, func(id string) string {
		if n := names.Orgs[id]; n != "" {
			return n
		}
		lv := list()
		return lv.orgName(id)
	}, func(id string) string { return names.Teams[id] }), needs...)
	// A key this run's flags decided or unbound is not asked or decided again, but it still
	// counts for the rules, so an owner beside it waits for it.
	for i := range needs {
		needs[i].flagged = flagged[needs[i].Key]
	}
	decided, err := decideNeeds(needs, mode, app.form())
	if err != nil {
		return teamsView{}, err
	}
	ch.decided, ch.orgNames = decided, orgNames(needs)
	next, err := routing.Update(app.stateDir, cfg.ProfileName, ch.write)
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

// write makes ch on s: the flags' decisions, then the unticked teams, then the decisions
// for the teams that need them, then the ticked teams; then it holds any owner decided
// that would take what it must not (holdOwners).
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
	before := maps.Clone(s.Bindings)
	for _, d := range ch.decided {
		if err := d.op.apply(s); err != nil {
			return err
		}
	}
	for _, tk := range ch.ticks {
		id, err := s.Select(tk.team)
		if err != nil {
			return err
		}
		tk.enrollmentID = id
	}
	var held []string
	ch.decided, held = ch.holdOwners(s, ch.decided, before)
	ch.warnings = append(ch.warnings, held...)
	return nil
}

// holdOwners checks s, as a run leaves it: each owner among decided that takes a
// repository another organization needs with no decision of its own
// (routing.State.OwnersTaking) goes back to what before had. It returns the decisions that
// stand, and why each owner it held is left to decide.
func (ch *teamsChange) holdOwners(s *routing.State, decided []neededOp, before map[string]routing.Binding) (stand []neededOp, held []string) {
	keys := make([]string, 0, len(decided))
	for _, d := range decided {
		keys = append(keys, d.op.key)
	}
	taken := s.OwnersTaking(keys)
	if len(taken) == 0 {
		return decided, nil
	}
	stand = make([]neededOp, 0, len(decided))
	for _, d := range decided {
		n, ok := taken[d.op.key]
		if !ok {
			stand = append(stand, d)
			continue
		}
		restoreBinding(s, before, d.op.key)
		held = append(held, fmt.Sprintf("%s is left to decide: binding it to %s would also give it %s, which %s needs; decide that first",
			d.op.key, d.orgName, n.Key, cmp.Or(ch.orgNames[n.Organization], n.Organization)))
	}
	return stand, held
}

// finishTicks gives each ticked team its key, and takes one that gets none off again. A
// decision of this run is undone with it only when every team that needed it failed and no
// team ticked before needs it; then the result is checked again, since an owner left
// standing may have relied on a decision undone (holdOwners). It returns the first
// failure, with any owner that check held.
func (app *App) finishTicks(cmd *cobra.Command, cfg *config.Config, before routing.State, ch *teamsChange) error {
	var failed error
	lost := map[*tick]bool{}
	for _, tk := range ch.ticks {
		err := app.mintTeamKey(cmd.Context(), cfg, tk)
		if err == nil {
			ch.changes = append(ch.changes, "ticked "+tickLabel(tk))
			ch.warnings = append(ch.warnings, tk.notes...)
			continue
		}
		lost[tk] = true
		if failed == nil {
			failed = fmt.Errorf("%s was not ticked: creating its key for this machine failed: %w", tickLabel(tk), err)
		}
	}
	if len(lost) > 0 {
		var undo []string
		stand := make([]neededOp, 0, len(ch.decided))
		for _, d := range ch.decided {
			if !d.earlier && len(d.holders) > 0 && !slices.ContainsFunc(d.holders, func(tk *tick) bool { return !lost[tk] }) {
				undo = append(undo, d.op.key)
				continue
			}
			stand = append(stand, d)
		}
		ch.decided = stand
		var held []string
		_, undoErr := routing.Update(app.stateDir, cfg.ProfileName, func(s *routing.State) error {
			for tk := range lost {
				s.Deselect(tk.enrollmentID)
			}
			for _, key := range undo {
				restoreBinding(s, before.Bindings, key)
			}
			ch.decided, held = ch.holdOwners(s, stand, before.Bindings)
			return nil
		})
		for tk := range lost {
			_ = keystore.DeleteTeam(app.dir, tk.enrollmentID)
			if undoErr != nil {
				failed = errors.Join(failed, fmt.Errorf("untick %s with `terma teams --deselect %s`: %w", tickLabel(tk), tk.team.TeamID, undoErr))
			}
		}
		// A failed run prints its error alone, so an owner held with the ticks is said there.
		if undoErr == nil {
			for _, h := range held {
				failed = errors.Join(failed, errors.New(h))
			}
		}
	}
	for _, d := range ch.decided {
		ch.changes = append(ch.changes, describeOp(d.op, d.orgName))
	}
	return failed
}

// restoreBinding puts key's decision in s back to what before had.
func restoreBinding(s *routing.State, before map[string]routing.Binding, key string) {
	key = routing.Normalize(key)
	if b, ok := before[key]; ok {
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
		for _, k := range slices.Sorted(maps.Keys(next.Bindings)) {
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

// bindingMode is how this run decides the bindings left to decide: --bind-all binds them
// even on a terminal; the checklist asks about every one; a terminal asks about what this
// run ticks; --yes binds what it ticks; anything else leaves them to decide.
func (app *App) bindingMode(f teamsFlags, ask bool) bindingMode {
	switch {
	case f.bindAll:
		return bindPending
	case ask && f.checklist:
		return askPending
	case ask:
		return askTicked
	case f.assumeYes:
		return bindTicked
	}
	return leavePending
}

// form draws a checklist and reads its answer: prompt.Run unless runForm stands in.
func (app *App) form() func(*prompt.Form) ([]prompt.Item, error) {
	if app.runForm != nil {
		return app.runForm
	}
	return prompt.Run
}
