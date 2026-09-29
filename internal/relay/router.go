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

// maxInboxBytes bounds what waits in the inbox for its session to be placed.
const maxInboxBytes = 64 << 20

// router moves accepted bodies from the inbox to per-project outboxes. It is the only
// reader of the inbox and runs on one goroutine, so no two passes race over a file.
type router struct {
	dir    string
	res    *resolver
	traces *traceMap
	// hold is how long a record whose session is known but not placed waits;
	// traceHold how long one that names no session waits for its trace to name one. A
	// Codex turn exports its child spans as each ends, before the turn span that names
	// the session, so a long turn's children need far longer than a session does.
	hold      time.Duration
	traceHold time.Duration
	now       func() time.Time
	logf      func(string, ...any)
	stats     *stats
	// routed is told each route that has something new to deliver.
	routed func(route string)

	// inboxBytes bounds the inbox (maxInboxBytes when zero).
	inboxBytes int64
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
	// Over the inbox's bound, the oldest entries are placed now, as if their holds had
	// closed: a flood of records that name no session must not grow the inbox for the
	// half hour a trace may wait.
	force := rt.overBound(entries)
	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		present[e.name] = true
		if ctx.Err() != nil {
			return
		}
		forced := force[e.name]
		if w := rt.waiting[e.name]; w != nil && !forced && !rt.due(w, e, now) {
			continue
		}
		delete(rt.waiting, e.name)
		if err := rt.route(ctx, e, forced); err != nil {
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
	age := now.Sub(e.received)
	if (len(w.sessions) > 0 && age >= rt.hold) || age >= rt.traceHold || !now.Before(w.next) {
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

// overBound names the oldest inbox entries to place now so the inbox fits
// rt.inboxBytes; none while it fits.
func (rt *router) overBound(entries []entry) map[string]bool {
	limit := rt.inboxBytes
	if limit <= 0 {
		limit = maxInboxBytes
	}
	sizes := make([]int64, len(entries))
	var total int64
	for i, e := range entries {
		if info, err := os.Stat(filepath.Join(rt.dir, inboxDir, e.name)); err == nil {
			sizes[i] = info.Size()
			total += sizes[i]
		}
	}
	force := map[string]bool{}
	for i, e := range entries { // oldest first
		if total <= limit {
			break
		}
		force[e.name] = true
		total -= sizes[i]
	}
	if len(force) > 0 {
		rt.logf("inbox over %d bytes: placing its %d oldest entries now", limit, len(force))
	}
	return force
}

func (rt *router) route(ctx context.Context, e entry, force bool) error {
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
		// Protobuf cannot be split or filtered without a decoder the relay does not carry
		// (terma configures every agent for OTLP/JSON; this is an exporter a developer
		// overrode). The whole request goes to the machine project — unless its policy
		// withholds content, which could not be enforced on it: then it is set aside.
		if policy, _ := loadContentPolicy(rt.dir, machineRoute); !policy.allowsAll() {
			rt.logf("%s is protobuf and the machine project withholds content; set aside unsent", e.name)
			rt.stats.addDead(1)
			return rt.moveDead(e, path, "unfilterable")
		}
		return rt.moveWhole(e, path, body, machineRoute)
	}
	b, err := parseBatch(e.sig, body)
	if err != nil {
		rt.logf("%s is not OTLP/JSON, set aside: %v", e.name, err)
		rt.stats.addDead(1)
		return rt.moveDead(e, path, "inbox")
	}

	now := rt.now()
	age := now.Sub(e.received)
	expired, traceExpired := age >= rt.hold || force, age >= rt.traceHold || force
	items := b.items()
	for _, it := range items {
		if it.session != "" {
			rt.traces.put(it.traceID, it.session, it.source, now)
		}
	}
	for _, it := range items {
		session, source := it.session, it.source
		if session == "" && it.traceID != "" {
			session, source = rt.traces.get(it.traceID)
		}
		switch {
		case session != "":
			route, final := rt.res.decide(ctx, session, source, it.at, now, expired)
			it.route = route
			if !final {
				it.route = pendingRoute
			}
		case it.traceID != "" && !traceExpired:
			// A span may name its session only through a sibling in its trace that has not
			// arrived yet: a Codex turn's children arrive before the turn span.
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
		// The route's content policy, applied before anything reaches its outbox: what a
		// project withholds is never written for it, let alone sent.
		if policy, _ := loadContentPolicy(rt.dir, it.route); !policy.allowsAll() {
			rt.stats.addWithheld(b.withhold(it.route, policy))
		}
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
