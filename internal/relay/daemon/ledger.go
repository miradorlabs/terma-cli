package daemon

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

type cachedLedger struct {
	l    runs.Snapshot
	read time.Time
}

// ledgerTTL is how long Route trusts a read of a session's run ledger; every queued part
// is rechecked against a fresh read before leaving.
const ledgerTTL = time.Second

// maxLedgers bounds the ledgers Route caches in memory.
const maxLedgers = 4096

// mixedInterval reports whether session's run covering [start, end] was Mixed and from when.
// An unreadable ledger fails closed (Mixed from Unix(0, 1)).
func (t *Teams) mixedInterval(session string, start, end time.Time) (time.Time, bool) {
	if session == "" {
		return time.Time{}, false
	}
	return mixedIntervalFrom(t.ledger(session), start, end)
}

func (t *Teams) ledger(session string) runs.Snapshot {
	now := t.deps.Now()
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

func (p *recheck) mixedInterval(session string, start, end time.Time) (time.Time, bool) {
	if session == "" {
		return time.Time{}, false
	}
	l, read := p.runs[session]
	if !read {
		l = runs.Read(p.stateDir, session)
		p.runs[session] = l
	}
	return mixedIntervalFrom(l, start, end)
}

func mixedIntervalFrom(l runs.Snapshot, start, end time.Time) (time.Time, bool) {
	from, mixed := l.MixedInterval(start, end)
	if !mixed {
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

// PruneRuns prunes expired run ledgers under stateDir while preserving every session
// referenced by the .teams/ relay outbox or the hook spool (events-v2.jsonl). On any
// partial scan error from either queue, pruning is skipped so no queued session's ledger
// is lost.
func PruneRuns(ctx context.Context, stateDir string, queuedOutbox func() (map[string]bool, error), now time.Time) {
	queued := map[string]bool{}
	if queuedOutbox != nil {
		fromOutbox, err := queuedOutbox()
		if err != nil {
			return
		}
		maps.Copy(queued, fromOutbox)
	}
	sp, err := spool.Open(filepath.Join(stateDir, spool.Dir))
	if err != nil {
		return
	}
	if err := sp.Sessions(ctx, func(sessionID string) { queued[sessionID] = true }); err != nil {
		return
	}
	runs.Prune(ctx, stateDir, now, queued)
}

func pruneState(ctx context.Context, stateDir string, r *relay.Relay, now time.Time) {
	claim.Prune(stateDir, now)
	PruneRuns(ctx, stateDir, r.QueuedSessions, now)
}
