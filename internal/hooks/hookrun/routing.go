package hookrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// ErrFailClosed signals that a reach hook could not verify or persist routing state,
// instructing dispatch.Run to exit non-zero so extension plugins fail closed.
var ErrFailClosed = errors.New("reach classification failed closed")

// Routing holds a profile's routing state and compiled Router for one hook process.
// When Env.Routing is non-nil it replaces Policy and Team; nil keeps the pre-cutover fallback.
type Routing struct {
	Profile            string
	WithholdPlacements bool // Council 7: true while a live !RunInfo.Routing relay holds the lock.
	state              *routing.State
	router             *routing.Router
	mu                 sync.Mutex
	runs               map[string]*ledgerRun
	writeErr           error
}
type ledgerRun struct {
	id              string
	locations       []string
	active, mixedAt time.Time
	aside           bool
}

// NewRouting builds a Routing for profile from st; nil fails closed (Usable() == false).
func NewRouting(profile string, st *routing.State) *Routing {
	rt := &Routing{Profile: profile, runs: map[string]*ledgerRun{}}
	if st != nil {
		s := *st
		rt.state, rt.router = &s, routing.NewRouter(s)
	}
	return rt
}

// LoadRouting loads profile's routing state under stateDir; missing or unreadable fails closed.
func LoadRouting(stateDir, profile string) *Routing {
	if st, ok, err := routing.Load(stateDir, profile); err == nil && ok {
		return NewRouting(profile, &st)
	}
	return NewRouting(profile, nil)
}

// Usable reports whether rt has a loaded, compiled routing state.
func (rt *Routing) Usable() bool { return rt != nil && rt.router != nil }

// GitHooks reports whether Route sends the repository at gitDir to a team with git_hooks on.
func (rt *Routing) GitHooks(gitDir string, now time.Time) bool {
	return rt.Usable() && gitDir != "" && rt.stampsCommits(rt.router.Route(locationOf(gitDir, gitx.RepositoryFS(gitDir)), now))
}
func (rt *Routing) stampsCommits(d routing.Decision) bool {
	t, ok := rt.state.Team(d.EnrollmentID)
	return d.Outcome == routing.Send && ok && t.Policy.GitHooks
}
func (rt *Routing) skillsCollectedFor(r *Repo) bool {
	if !rt.Usable() {
		return false
	}
	if r != nil && r.dest != nil {
		t, ok := rt.state.Team(r.dest.d.EnrollmentID)
		p, tc := t.Policy.Content()
		return r.dest.d.Outcome == routing.Send && ok && p && tc
	}
	return slices.ContainsFunc(rt.state.Teams, func(t routing.Team) bool { p, tc := t.Policy.Content(); return p && tc })
}
func (e Env) admitsOrigin(origin string) bool {
	if e.Routing == nil {
		return e.Policy.Admits(config.Repository{Origin: origin})
	}
	return e.Routing.route(routing.Location{Kind: routing.Repository, Repository: origin}, e.Time()).collected()
}
func (rt *Routing) route(loc routing.Location, now time.Time) *dest {
	if !rt.Usable() {
		return &dest{loc: loc, d: routing.Decision{Outcome: routing.Drop}}
	}
	return &dest{loc: loc, d: rt.router.Route(loc, now)}
}
func (e Env) resolveRouted(ctx context.Context, r *Repo) (*Repo, error) {
	if !e.Routing.Usable() {
		return nil, ErrNotAdmitted
	}
	if r == nil {
		var err error
		if r, err = e.routeHere(ctx); err != nil {
			return nil, err
		}
	}
	if !r.dest.collected() {
		return nil, ErrNotAdmitted
	}
	storeDir, err := project.StoreDir(e.StateDir, r.Root, r.GitDir)
	if err != nil {
		return nil, err
	}
	r.Store = e.store(storeDir)
	return r, nil
}
func (e Env) routeHere(ctx context.Context) (*Repo, error) {
	root, gitDir, id, remote, err := e.locate(ctx)
	if err != nil {
		return nil, err
	}
	name, wt := CheckoutNames(root, gitDir)
	return &Repo{Root: root, GitDir: gitDir, Name: name, Worktree: wt, Repository: id, Remote: remote, dest: e.Routing.route(locationOf(gitDir, id.Origin), e.Time())}, nil
}
func (e Env) sandboxedGitRepo(ctx context.Context) *Repo {
	if e.Routing == nil || e.Routing.Usable() || !e.inSandbox() {
		return nil
	}
	r, err := e.routeHere(ctx)
	if err != nil || r.GitDir == "" {
		return nil
	}
	storeDir, err := project.StoreDir(e.StateDir, r.Root, r.GitDir)
	if err != nil {
		storeDir = filepath.Join(r.GitDir, project.GitStoreDir)
	}
	r.dest, r.Store = nil, e.store(storeDir)
	return r
}
func locationOf(gitDir, origin string) routing.Location {
	if gitDir != "" && routing.Owner(origin) != "" {
		return routing.Location{Kind: routing.Repository, Repository: origin}
	}
	return routing.Location{Kind: routing.OutsideGit}
}

type dest struct {
	loc routing.Location
	d   routing.Decision
}

func (d *dest) collected() bool {
	return d != nil && (d.d.Outcome == routing.Send || d.d.Outcome == routing.Hold)
}
func (d *dest) key() string { return d.d.EnrollmentID + "\x00" + holdKey(d.d) }

// Collected reports whether records in r may leave the machine.
func (r *Repo) Collected() bool {
	if r.dest != nil {
		return r.dest.collected()
	}
	return r.ProjectID != ""
}

// RouteKey identifies r's routing destination for deduplicating repeated evidence snapshots.
func (r *Repo) RouteKey() string {
	if r.dest != nil {
		return r.dest.key()
	}
	return r.ProjectID
}
func (r *Repo) routeSalt() string {
	if r.dest != nil {
		return "\x00" + r.dest.key()
	}
	return ""
}
func holdKey(d routing.Decision) string {
	if d.Outcome == routing.Hold && d.Reason == routing.ReasonPending {
		return d.Authority.Key
	}
	return ""
}

// Routed stamps ev with r's routed destination and the session's current run when routed.
func (e Env) Routed(r *Repo, ev spool.Event) spool.Event {
	if e.Routing == nil || r == nil || r.dest == nil {
		return ev
	}
	return e.destine(ev, r.dest, false, e.runOf(ev.SessionID))
}
func (e Env) destine(ev spool.Event, d *dest, own bool, run string) spool.Event {
	if ev.Account == "" && !ev.NoPerson {
		l := e.lane()
		ev.Account, ev.NoPerson = l.Account, l.NoPerson
	}
	ev.Profile, ev.Enrollment, ev.HoldKey, ev.Repository = e.Routing.Profile, d.d.EnrollmentID, holdKey(d.d), config.Repository{}
	if d.loc.Kind == routing.Repository {
		ev.Repository.Origin = d.loc.Repository
	}
	ev.Run, ev.OwnLocation, ev.Global = run, own, false
	delete(ev.Attrs, AttrProjectID)
	return ev
}
func (e Env) runOf(sessionID string) string { return e.currentRun(sessionID).id }
func (e Env) currentRun(sessionID string) ledgerRun {
	rt := e.Routing
	if rt == nil || sessionID == "" {
		return ledgerRun{}
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	cur, ok := rt.runs[sessionID]
	if !ok {
		p, _ := runs.Locate(e.StateDir, sessionID)
		cur = &ledgerRun{id: p.Run, locations: p.Locations, active: p.Active, mixedAt: p.MixedAt, aside: p.Aside}
		rt.runs[sessionID] = cur
	}
	out := *cur
	out.locations = slices.Clip(out.locations)
	return out
}
func (e Env) keepRun(sessionID string, now time.Time) {
	due := false
	e.Routing.updateRun(sessionID, func(cur *ledgerRun) {
		if cur.id != "" && now.Sub(cur.active) > runs.RefreshEvery {
			cur.active, due = now, true
		}
	})
	if due {
		if err := runs.Refresh(e.StateDir, sessionID, now); err != nil {
			e.Logf("renew the run ledger: %v", err)
		}
	}
}
func (rt *Routing) updateRun(sessionID string, fn func(*ledgerRun)) {
	rt.mu.Lock()
	if cur, ok := rt.runs[sessionID]; ok {
		fn(cur)
	}
	rt.mu.Unlock()
}

// Turns judges and routes per-turn agent records by each turn's working directory.
type Turns struct {
	ctx    context.Context
	env    Env
	admits map[string]bool
	dests  map[string]*dest
}

// Turns returns a per-turn judge for this hook invocation.
func (e Env) Turns(ctx context.Context) *Turns {
	return &Turns{ctx: ctx, env: e, admits: map[string]bool{}, dests: map[string]*dest{}}
}

// Admits reports whether a turn in cwd may be spooled.
func (t *Turns) Admits(cwd string) bool {
	if t.env.Routing != nil {
		return cwd != "" && t.dest(cwd).collected()
	}
	if ok, seen := t.admits[cwd]; cwd == "" || seen {
		return cwd != "" && ok
	}
	t.admits[cwd] = t.env.Admits(t.ctx, cwd)
	return t.admits[cwd]
}

// RouteKey returns cwd's routed destination key (matching Repo.RouteKey), or "" if unrouted/uncollected.
func (t *Turns) RouteKey(cwd string) string {
	if d := t.dest(cwd); t.env.Routing != nil && cwd != "" && d.collected() {
		return d.key()
	}
	return ""
}

// Exclusive reports whether cwd is admitted, respects GIT_DIR isolation, and routes to r.
func (t *Turns) Exclusive(r *Repo, cwd string) bool {
	return cwd != "" && (os.Getenv("GIT_DIR") == "" || cwd == t.env.Cwd) && t.Admits(cwd) && (t.env.Routing == nil || (r != nil && r.dest != nil && t.RouteKey(cwd) == r.RouteKey()))
}

// Routed stamps ev using cwd's routed destination (OwnLocation: true) while keeping the session run.
func (t *Turns) Routed(r *Repo, cwd string, ev spool.Event) spool.Event {
	if t.env.Routing == nil || cwd == "" {
		return t.env.Routed(r, ev)
	}
	return t.env.destine(ev, t.dest(cwd), true, t.env.runOf(ev.SessionID))
}
func (t *Turns) dest(cwd string) *dest {
	if d, ok := t.dests[cwd]; ok {
		return d
	}
	d := &dest{loc: routing.Location{Kind: routing.Unknown}, d: routing.Decision{Outcome: routing.Drop}}
	if t.env.Routing.Usable() && cwd != "" {
		e := t.env
		e.Cwd = cwd
		if r, err := e.routeHere(t.ctx); err == nil {
			d = r.dest
		}
	}
	t.dests[cwd] = d
	return d
}
func (rt *Routing) dropRunCache(ids ...string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, id := range ids {
		delete(rt.runs, id)
	}
}

func (rt *Routing) noteWriteErr(err error) {
	if rt == nil || err == nil {
		return
	}
	rt.mu.Lock()
	if rt.writeErr == nil {
		rt.writeErr = err
	}
	rt.mu.Unlock()
}

func (rt *Routing) takeWriteErr() error {
	if rt == nil {
		return nil
	}
	rt.mu.Lock()
	err := rt.writeErr
	rt.writeErr = nil
	rt.mu.Unlock()
	return err
}

