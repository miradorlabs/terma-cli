package hookrun

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Routing is how hooks route on a migrated machine (config.Migrated): the profile's routing
// state and one Router compiled from it. A hook calls Route for its local effects: it writes
// a manifest, spools an event or installs commit hooks only for a location Route sends or
// holds, and every event it spools records the destination Route gave it, never a project.
// Env.Routing is nil on a machine not migrated, where hooks keep the single team's rules,
// claim.Mark included.
type Routing struct {
	// Profile is the profile whose routing state this is, which every spooled event names.
	Profile string
	// state and router are nil when the state is missing, unreadable or not yet committed
	// by the migration: hooks then route nothing, write nothing and spool nothing.
	state  *routing.State
	router *routing.Router

	mu sync.Mutex
	// runs is the run each session's hooks name, by session ID, and where the run ledger
	// says it has been: read once per process.
	runs map[string]*ledgerRun
}

// ledgerRun is a session's run as one hook process knows it: its ID, the locations the
// run ledger records for it, as read and with those this process has added since, and the
// ledger's last activity (runs.Position).
type ledgerRun struct {
	id        string
	locations []string
	active    time.Time
}

// NewRouting is profile's routing under st, its routing state: nil for one missing,
// unreadable, or still being prepared by the migration, under which a hook sends nothing.
func NewRouting(profile string, st *routing.State) *Routing {
	rt := &Routing{Profile: profile, runs: map[string]*ledgerRun{}}
	if st != nil && st.Ready() {
		s := *st
		rt.state, rt.router = &s, routing.NewRouter(s)
	}
	return rt
}

// LoadRouting reads profile's routing state under the state directory stateDir and compiles
// its Router, which routes everything one hook records: hooks load it once per invocation.
func LoadRouting(stateDir, profile string) *Routing {
	st, ok, err := routing.LoadReady(stateDir, profile)
	if err != nil || !ok {
		return NewRouting(profile, nil)
	}
	return NewRouting(profile, &st)
}

// Usable reports whether rt can route at all: its state was read and committed.
func (rt *Routing) Usable() bool { return rt != nil && rt.router != nil }

// GitHooks reports whether routing sends the repository whose git directory is gitDir to a
// team whose policy turns git_hooks on: where a migrated machine installs terma's commit
// hooks, and where prepare-commit-msg stamps. A repository Route holds or drops gets
// neither, so turning git_hooks off stops trailers where hooks are already installed.
func (rt *Routing) GitHooks(gitDir string, now time.Time) bool {
	if !rt.Usable() || gitDir == "" {
		return false
	}
	return rt.stampsCommits(rt.router.Route(locationOf(gitDir, gitx.RepositoryFS(gitDir)), now))
}

// stampsCommits reports whether d sends to a team whose policy turns git_hooks on.
func (rt *Routing) stampsCommits(d routing.Decision) bool {
	if d.Outcome != routing.Send {
		return false
	}
	t := rt.state.Team(d.EnrollmentID)
	return t != nil && t.Policy.GitHooks
}

// route is where what loc records goes now, with loc; a drop when rt routes nothing.
func (rt *Routing) route(loc routing.Location, now time.Time) *dest {
	if !rt.Usable() {
		return &dest{loc: loc, d: routing.Decision{Outcome: routing.Drop}}
	}
	return &dest{loc: loc, d: rt.router.Route(loc, now)}
}

// resolveRouted is resolve on a migrated machine: the working copy at Cwd and its session
// store when Route sends or holds its location, else ErrNotAdmitted.
func (e Env) resolveRouted(ctx context.Context) (*Repo, error) {
	if !e.Routing.Usable() {
		return nil, ErrNotAdmitted
	}
	r, err := e.routeHere(ctx)
	if err != nil {
		return nil, err
	}
	if !r.dest.collected() {
		return nil, ErrNotAdmitted
	}
	storeDir, err := project.StoreDir(e.StateDir, r.Root, r.GitDir)
	if err != nil {
		return nil, err
	}
	r.Store = session.Open(storeDir)
	return r, nil
}

// routeHere is the working copy at Cwd on a migrated machine, located and routed whoever
// collects it, with no session store.
func (e Env) routeHere(ctx context.Context) (*Repo, error) {
	root, gitDir, id, remote, err := e.locate(ctx)
	if err != nil {
		return nil, err
	}
	r := &Repo{Root: root, GitDir: gitDir, Repository: id, Remote: remote, dest: e.Routing.route(locationOf(gitDir, id.Origin), e.Time())}
	r.Name, r.Worktree = CheckoutNames(root, gitDir)
	return r, nil
}

// locationOf is where a hook in the checkout whose git directory is gitDir, and whose
// origin admission reads as origin, runs: that repository when its origin names a hosted
// one, else outside git, as routing matches nothing else by repository. "" for gitDir is
// a folder outside any checkout.
func locationOf(gitDir, origin string) routing.Location {
	if gitDir != "" && routing.Owner(origin) != "" {
		return routing.Location{Kind: routing.Repository, Repository: origin}
	}
	return routing.Location{Kind: routing.OutsideGit}
}

// dest is where routing sends what one location records: the location, as spooled events
// and the relay's placements name it, and the decision Route made for it.
type dest struct {
	loc routing.Location
	d   routing.Decision
}

// collected reports whether what is recorded there may leave the machine, now or once its
// team can send: Route sends or holds it.
func (d *dest) collected() bool {
	return d != nil && (d.d.Outcome == routing.Send || d.d.Outcome == routing.Hold)
}

// kind is the placement kind of d's location.
func (d *dest) kind() claim.Kind {
	if d.loc.Kind == routing.Repository {
		return claim.KindRepository
	}
	return claim.KindOutsideGit
}

// Collected reports whether what is recorded in r may leave the machine: r is claimed for
// a project, or on a migrated machine, routing sends or holds its location.
func (r *Repo) Collected() bool {
	if r.dest != nil {
		return r.dest.collected()
	}
	return r.ProjectID != ""
}

// RouteKey is what r's records are routed by: its project, or on a migrated machine the
// destination routing gave its location. A hook that keeps evidence from being spooled
// twice keys it by this too, so a session that moves to another destination sends it there.
func (r *Repo) RouteKey() string {
	if r.dest != nil {
		return r.dest.d.EnrollmentID + "\x00" + holdKey(r.dest.d)
	}
	return r.ProjectID
}

// routeSalt is what a hash of r's evidence adds for its destination: nothing where the
// hashed attributes carry r's project, as on a machine not migrated.
func (r *Repo) routeSalt() string {
	if r.dest == nil {
		return ""
	}
	return "\x00" + r.RouteKey()
}

// holdKey is the pending binding holding a record d holds, "" for any other decision.
func holdKey(d routing.Decision) string {
	if d.Outcome == routing.Hold && d.Reason == routing.ReasonPending {
		return d.Authority.Key
	}
	return ""
}

// Routed is ev, which a handler appends to the spool itself rather than through EmitFor,
// attributed through the session's run, as it is to be spooled: on a migrated machine it
// records r's destination, the session's run and no project; on any other it is ev as the
// handler stamped it.
func (e Env) Routed(r *Repo, ev spool.Event) spool.Event {
	if e.Routing == nil || r == nil || r.dest == nil {
		return ev
	}
	return e.destine(ev, r.dest, false, e.runOf(ev.SessionID))
}

// destine stamps ev with the destination d gave its location: the profile, the enrollment
// sending or holding it and any pending binding holding it, its location as delivery
// rechecks it (the repository, or none outside git), its run, and whether that location is
// its own rather than its run's. A project a handler stamped is removed: an event routed
// by its destination never names one.
func (e Env) destine(ev spool.Event, d *dest, own bool, run string) spool.Event {
	ev.Profile = e.Routing.Profile
	ev.Enrollment, ev.HoldKey = d.d.EnrollmentID, holdKey(d.d)
	ev.Repository = config.Repository{}
	if d.loc.Kind == routing.Repository {
		ev.Repository.Origin = d.loc.Repository
	}
	ev.Run, ev.OwnLocation, ev.Global = run, own, false
	delete(ev.Attrs, AttrProjectID)
	return ev
}

// runOf is the run sessionID's hooks name: the one a session start began in this process,
// else the latest the run ledger records for it (runs.Locations), however long the session
// has been idle; "" before any run began.
func (e Env) runOf(sessionID string) string {
	run, _ := e.currentRun(sessionID)
	return run
}

// currentRun is runOf with where the run has been, as the run ledger records it: read with
// the run, once per process.
func (e Env) currentRun(sessionID string) (run string, locations []string) {
	rt := e.Routing
	if rt == nil || sessionID == "" {
		return "", nil
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	cur, ok := rt.runs[sessionID]
	if !ok {
		p, _ := runs.Locate(e.StateDir, sessionID)
		cur = &ledgerRun{id: p.Run, locations: p.Locations, active: p.Active}
		rt.runs[sessionID] = cur
	}
	return cur.id, slices.Clip(cur.locations)
}

// setRun records that sessionID's hooks name run from now on, a run that has been nowhere,
// begun at at.
func (rt *Routing) setRun(sessionID, run string, at time.Time) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.runs[sessionID] = &ledgerRun{id: run, active: at}
}

// keepRun renews sessionID's run ledger (runs.Refresh) once its last activity, as this
// process read it with the run (currentRun), is older than runs.RefreshEvery. A run that
// stays in one place records nothing new, so without it the ledger, and a Mixed run's
// crossing with it, would be pruned while the session runs. It costs no read, and a write
// at most once a day.
func (e Env) keepRun(sessionID string, now time.Time) {
	rt := e.Routing
	rt.mu.Lock()
	cur, ok := rt.runs[sessionID]
	due := ok && cur.id != "" && now.Sub(cur.active) > runs.RefreshEvery
	if due {
		cur.active = now
	}
	rt.mu.Unlock()
	if !due {
		return
	}
	if err := runs.Refresh(e.StateDir, sessionID, now); err != nil {
		e.Logf("renew the run ledger: %v", err)
	}
}

// recorded notes that the run ledger now records locations for sessionID's run run, as
// runs.Touch returned them.
func (rt *Routing) recorded(sessionID, run string, locations []string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if cur, ok := rt.runs[sessionID]; ok && cur.id == run {
		cur.locations = locations
	}
}
