package hookrun

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

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
	if cwd == "" {
		return false
	}
	if t.env.Routing != nil {
		return t.dest(cwd).collected()
	}
	if ok, seen := t.admits[cwd]; seen {
		return ok
	}
	ok := t.env.Admits(t.ctx, cwd)
	t.admits[cwd] = ok
	return ok
}

// RouteKey returns cwd's routed destination key (matching Repo.RouteKey), or "" if unrouted/uncollected.
func (t *Turns) RouteKey(cwd string) string {
	if t.env.Routing == nil || cwd == "" {
		return ""
	}
	if d := t.dest(cwd); d.collected() {
		return d.d.EnrollmentID + "\x00" + holdKey(d.d)
	}
	return ""
}

// Matches reports whether cwd routes to the same destination as r (always true when unrouted).
func (t *Turns) Matches(r *Repo, cwd string) bool {
	if t.env.Routing == nil {
		return true
	}
	return r != nil && r.dest != nil && t.RouteKey(cwd) == r.RouteKey()
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
	if t.env.Routing.Usable() {
		e := t.env
		e.Cwd = cwd
		if r, err := e.routeHere(t.ctx); err == nil {
			d = r.dest
		}
	}
	t.dests[cwd] = d
	return d
}
