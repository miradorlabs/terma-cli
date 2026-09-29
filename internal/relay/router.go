package relay

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// pendingRoute marks an item whose session cannot be placed yet.
const pendingRoute = ""

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
}

// pass routes every inbox entry once, oldest first. An entry whose sessions are not all
// placed keeps its unplaced remainder in the inbox for the next pass.
func (rt *router) pass(ctx context.Context) {
	entries, err := listEntries(filepath.Join(rt.dir, inboxDir))
	if err != nil {
		rt.logf("list inbox: %v", err)
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if err := rt.route(ctx, e); err != nil {
			rt.logf("route %s: %v", e.name, err)
		}
	}
}

func (rt *router) route(ctx context.Context, e entry) error {
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
	if n > 0 {
		// Replaced in place: the name keeps the received time the hold window runs from.
		return writeEntry(filepath.Join(rt.dir, inboxDir), e, rest)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
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
