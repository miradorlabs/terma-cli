package hookrun

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Turns judges what an agent recorded in each of its turns by the working directory the
// turn names, once per directory: a thread resumed in a collected repository still holds
// its earlier turns, which a hook there reads, and those stay on the machine when nothing
// collects where they ran. On a migrated machine a turn's directory is also the location
// of what it recorded, which routes by it rather than by the session's run. What a turn
// recorded is content, so the run turning Mixed still drops it (runs.Locational).
type Turns struct {
	ctx    context.Context
	env    Env
	admits map[string]bool
	dests  map[string]*dest
}

// Turns is a judge of the turns this hook reads.
func (e Env) Turns(ctx context.Context) *Turns {
	return &Turns{ctx: ctx, env: e, admits: map[string]bool{}, dests: map[string]*dest{}}
}

// Admits reports whether what a turn in cwd recorded may be spooled: whether the team
// policy collects the working copy there, or on a migrated machine whether Route sends or
// holds it. A turn naming no directory is never collected.
func (t *Turns) Admits(cwd string) bool {
	if cwd == "" {
		return false
	}
	if t.env.Routing != nil {
		return t.dest(cwd).collected()
	}
	ok, seen := t.admits[cwd]
	if !seen {
		ok = t.env.Admits(t.ctx, cwd)
		t.admits[cwd] = ok
	}
	return ok
}

// Routed is ev, read from a turn in cwd, as Env.Routed has it spooled, except that on a
// migrated machine its location is its own: cwd's, with the destination Route gave cwd. It
// still names the session's run, whose Mixed state drops it as any content. An event read
// from no turn, cwd "", is attributed through the session's run, at r.
func (t *Turns) Routed(r *Repo, cwd string, ev spool.Event) spool.Event {
	if t.env.Routing == nil || cwd == "" {
		return t.env.Routed(r, ev)
	}
	return t.env.destine(ev, t.dest(cwd), true, t.env.runOf(ev.SessionID))
}

// dest is where routing sends what a turn in cwd recorded; a drop where nothing can be
// located.
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
