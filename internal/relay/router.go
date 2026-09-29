package relay

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// pendingRoute marks an item whose session cannot be placed yet.
const pendingRoute = ""

// recheck is how often an inbox entry waiting on its sessions is looked at again when
// nothing it waits on has become known. Between looks the entry is neither read nor
// rewritten: a Codex Desktop turn leaves megabytes of spans waiting on their trace, and
// re-parsing them every second held a core at half load for the length of the hold.
const recheck = 5 * time.Second

// router moves accepted bodies from the inbox to per-project outboxes. It is the only
// reader of the inbox and runs on one goroutine, so no two passes race over a file.
type router struct {
	dir    string
	res    *resolver
	traces *traceMap
	hold   time.Duration
	now    func() time.Time
	logf   func(string, ...any)
	stats  *stats
	// routed is told each route that has something new to deliver.
	routed func(route string)

	// waiting is what each inbox entry still holding records waits on, by file name.
	waiting map[string]*waitState
	// looked counts entries read and routed (tests).
	looked int
}

// waitState is what an entry's unplaced records wait on: sessions no decision or hook
// record exists for yet, and traces no record has named a session for yet.
type waitState struct {
	sessions map[string]bool
	traces   map[string]bool
	next     time.Time
}

// pass routes every inbox entry that is due, oldest first. An entry whose sessions are
// not all placed keeps its unplaced remainder in the inbox, and is looked at again only
// when something it waits on becomes known, at recheck, or when its hold closes.
func (rt *router) pass(ctx context.Context) {
	entries, err := listEntries(filepath.Join(rt.dir, inboxDir))
	if err != nil {
		rt.logf("list inbox: %v", err)
		return
	}
	if rt.waiting == nil {
		rt.waiting = map[string]*waitState{}
	}
	now := rt.now()
	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		present[e.name] = true
		if ctx.Err() != nil {
			return
		}
		if w := rt.waiting[e.name]; w != nil && !rt.due(w, e, now) {
			continue
		}
		delete(rt.waiting, e.name)
		if err := rt.route(ctx, e); err != nil {
			rt.logf("route %s: %v", e.name, err)
		}
	}
	for name := range rt.waiting {
		if !present[name] {
			delete(rt.waiting, name)
		}
	}
}

// due reports whether a waiting entry is worth reading again: its hold closed, its
// recheck came round, or a session or trace it waits on can now be placed.
func (rt *router) due(w *waitState, e entry, now time.Time) bool {
	if now.Sub(e.received) >= rt.hold || !now.Before(w.next) {
		return true
	}
	for trace := range w.traces {
		if session, _ := rt.traces.get(trace); session != "" {
			return true
		}
	}
	for session := range w.sessions {
		if rt.res.known(session) {
			return true
		}
	}
	return false
}

func (rt *router) route(ctx context.Context, e entry) error {
	rt.looked++
	path := filepath.Join(rt.dir, inboxDir, e.name)
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if e.format != formatJSON {
		// Protobuf cannot be split without a decoder the relay does not carry; the whole
		// request goes to the machine project.
		return rt.moveWhole(e, path, body, machineRoute)
	}
	b, err := parseBatch(e.sig, body)
	if err != nil {
		rt.logf("%s is not OTLP/JSON, set aside: %v", e.name, err)
		rt.stats.addDead(1)
		return rt.moveDead(e, path, "inbox")
	}

	now := rt.now()
	expired := now.Sub(e.received) >= rt.hold
	items := b.items()
	for _, it := range items {
		if it.session != "" {
			rt.traces.put(it.traceID, it.session, it.source, now)
		}
	}
	type decision struct {
		route string
		final bool
	}
	memo := map[string]decision{}
	for _, it := range items {
		session, source := it.session, it.source
		if session == "" && it.traceID != "" {
			session, source = rt.traces.get(it.traceID)
		}
		switch {
		case session != "":
			d, ok := memo[session]
			if !ok {
				d.route, d.final = rt.res.decide(ctx, session, source, now, expired)
				memo[session] = d
			}
			it.route = d.route
			if !d.final {
				it.route = pendingRoute
			}
		case it.traceID != "" && !expired:
			// A span may name its session only through a sibling in its trace that has not
			// arrived yet.
			it.route = pendingRoute
		default:
			// No session at all: every Codex metric, Codex's process-level spans.
			it.route = machineRoute
		}
	}

	seen := map[string]bool{}
	for _, it := range items {
		if it.route == pendingRoute || seen[it.route] {
			continue
		}
		seen[it.route] = true
		out, n, err := b.encode(it.route)
		if err != nil {
			return err
		}
		if err := writeEntry(filepath.Join(rt.dir, outboxDir, it.route), newEntry(e.received, e.sig, formatJSON), out); err != nil {
			return err
		}
		rt.stats.addRouted(n)
		rt.routed(it.route)
	}
	rest, n, err := b.encode(pendingRoute)
	if err != nil {
		return err
	}
	if n == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}

	w := &waitState{sessions: map[string]bool{}, traces: map[string]bool{}, next: now.Add(recheck)}
	for _, it := range items {
		if it.route != pendingRoute {
			continue
		}
		if session, _ := rt.traces.get(it.traceID); it.session == "" && session == "" {
			w.traces[it.traceID] = true
		} else {
			w.sessions[cmp.Or(it.session, session)] = true
		}
	}
	rt.waiting[e.name] = w
	if len(seen) == 0 {
		// Nothing left the entry: the file on disk is already its remainder.
		return nil
	}
	// Replaced in place: the name keeps the received time the hold window runs from.
	return writeEntry(filepath.Join(rt.dir, inboxDir), e, rest)
}

func (rt *router) moveWhole(e entry, path string, body []byte, route string) error {
	if err := writeEntry(filepath.Join(rt.dir, outboxDir, route), newEntry(e.received, e.sig, e.format), body); err != nil {
		return err
	}
	rt.stats.addRouted(1)
	rt.routed(route)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (rt *router) moveDead(e entry, path, route string) error {
	dead := filepath.Join(rt.dir, deadDir)
	if err := os.MkdirAll(dead, 0o700); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(dead, route+"-"+e.name))
}
