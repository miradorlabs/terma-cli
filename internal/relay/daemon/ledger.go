package daemon

import (
	"maps"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/runs"
)

// What the team router reads of the run ledger (internal/runs): whether a record's run was
// Mixed by its time.

type cachedLedger struct {
	l runs.Snapshot
	// read is when the read began.
	read time.Time
}

// ledgerTTL is how long Route trusts a read of a session's run ledger. A crossing a hook
// records reaches Route at most this long after it. Route only drops early: every part it
// queues is rechecked against a fresh read before it leaves (recheck.Entry), so a read
// this old never lets a part out.
const ledgerTTL = time.Second

// maxLedgers bounds the ledgers Route keeps; past it the expired ones go, then the oldest
// quarter of those left, so the sessions read last stay cached.
const maxLedgers = 4096

// mixedAt reports whether session's run in force at at was Mixed by then (runs.MixedAt),
// and from when: a ledger that cannot be read is Mixed from the start.
func (t *Teams) mixedAt(session string, at time.Time) (time.Time, bool) {
	if session == "" {
		return time.Time{}, false
	}
	return mixedFrom(t.ledger(session), at)
}

// ledger is session's run ledger, read at most once per ledgerTTL. An entry expires
// ledgerTTL after its read began, however often it is used, and only a read begun later
// replaces it: a slow read never puts back what a newer one found.
func (t *Teams) ledger(session string) runs.Snapshot {
	now := time.Now()
	t.ledgerMu.Lock()
	e, ok := t.ledgers[session]
	t.ledgerMu.Unlock()
	if ok && now.Sub(e.read) < t.ledgerTTL {
		return e.l
	}
	l := t.readLedger(t.cfg.StateDir, session)
	t.remember(session, cachedLedger{l: l, read: now})
	return l
}

// remember keeps e as session's ledger unless a read begun later is kept already.
func (t *Teams) remember(session string, e cachedLedger) {
	t.ledgerMu.Lock()
	defer t.ledgerMu.Unlock()
	if have, ok := t.ledgers[session]; ok && !have.read.Before(e.read) {
		return
	}
	if len(t.ledgers) >= maxLedgers {
		maps.DeleteFunc(t.ledgers, func(_ string, c cachedLedger) bool { return e.read.Sub(c.read) >= t.ledgerTTL })
	}
	if n := len(t.ledgers); n >= maxLedgers {
		reads := make([]time.Time, 0, n)
		for _, c := range t.ledgers {
			reads = append(reads, c.read)
		}
		slices.SortFunc(reads, time.Time.Compare)
		cutoff := reads[n/4]
		maps.DeleteFunc(t.ledgers, func(_ string, c cachedLedger) bool { return !c.read.After(cutoff) })
	}
	t.ledgers[session] = e
}

// mixedAt is Teams.mixedAt from the pass's one read of session's ledger.
func (p *recheck) mixedAt(session string, at time.Time) (time.Time, bool) {
	if session == "" {
		return time.Time{}, false
	}
	l, read := p.runs[session]
	if !read {
		l = runs.Read(p.stateDir, session)
		p.runs[session] = l
	}
	return mixedFrom(l, at)
}

// mixedFrom reports whether the run in force at at, in a session's ledger l, was Mixed by
// then, and from when: a ledger that cannot be read is Mixed from the start.
func mixedFrom(l runs.Snapshot, at time.Time) (time.Time, bool) {
	from, mixed := l.MixedAt(at)
	if !mixed || !at.IsZero() && at.Before(from) {
		return time.Time{}, false
	}
	if from.IsZero() {
		from = time.Unix(0, 1)
	}
	return from, true
}

func mixedBlocker(session string) string {
	if session == "" {
		return ""
	}
	return "mixed:" + session
}
