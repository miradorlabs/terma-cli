package relay

import (
	"context"
	"maps"
	"time"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Run releases, drops and delivers held records until ctx is done, then stops every destination.
func (r *Relay) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	// The janitor weighs files by modification time, so it keeps the wall clock, never Options.Now.
	r.janitor(time.Now())
	r.recoverOutbox()
	lastJanitor := time.Now()
	// The first beat comes a minute after start, so a restarted relay says so soon.
	lastBeat := r.opts.Now().Add(min(time.Minute, r.opts.HeartbeatEvery) - r.opts.HeartbeatEvery)
	beaten := false
	for {
		select {
		case <-ctx.Done():
			// Claims that landed since the last sweep still count; the rest is dropped.
			r.sweep()
			r.dropHeldAtExit()
			close(r.stopping)
			done := make(chan struct{})
			go func() { r.wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(r.opts.Grace):
				r.cancelSend()
				<-done
			}
			r.cancelSend()
			r.countQueuedAtExit()
			return
		case <-tick.C:
			r.sweep()
			if now := time.Now(); now.Sub(lastJanitor) >= time.Minute {
				r.janitor(now)
				lastJanitor = now
			}
			if now := r.opts.Now(); now.Sub(lastBeat) >= r.opts.HeartbeatEvery {
				// Off the sweep's goroutine, so a slow host cannot hold up the relay; the stop
				// waits for it like any send.
				reason := semconv.TermaRelayHeartbeatReasonInterval
				if !beaten {
					reason, beaten = semconv.TermaRelayHeartbeatReasonStart, true
				}
				r.wg.Go(func() { _ = r.heartbeat(ctx, reason, "") })
				lastBeat = now
			}
		}
	}
}

func (r *Relay) janitor(now time.Time) {
	for rt, gone := range r.sweepOutbox(now) {
		r.mu.Lock()
		s := r.senders[rt]
		r.mu.Unlock()
		if s != nil {
			s.forget(gone)
		}
	}
}

// dropHeldAtExit drops what is still held: it lives in memory only, never on disk. Each
// drop is named by what held the part (decide's reason), or "claimed" when a claim landed too late.
func (r *Relay) dropHeldAtExit() {
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	r.mu.Lock()
	held := maps.Clone(r.held)
	clear(r.held)
	r.heldN, r.heldBytes = 0, 0
	r.mu.Unlock()
	r.mu.Lock()
	claimedLost := r.heldLost
	r.heldLost = 0
	r.mu.Unlock()
	defer func() { r.outbox.recordHeldLosses(claimedLost, r.opts.Now()) }()
	for key, parts := range held {
		for _, h := range parts {
			_, _, why, ok, _ := r.decide(key, h.p.pid, h.p.at, h.p.narrow, h.p.claimed)
			if ok {
				why = "claimed"
			}
			// A claimed part that waited for its team's policy is the developer's work, lost.
			if ok || why == whyNoKey || h.pinned != nil {
				claimedLost += h.p.records
			}
			r.stats.dropped(h.p.signal, why+"_at_exit", h.p.records)
			if r.opts.Logf != nil {
				c, _ := r.lookup(r.sessionFor(key))
				r.opts.Logf("drop %s_at_exit %s: %d %s from pid %d; claim %q pids %v", why, key, h.p.records, h.p.signal, h.p.pid, c.ProjectID, c.PIDs)
			}
		}
	}
}

// waitedForLocked counts the held records still waited for: every one not waiting for its
// team's policy (pinned to its claim or, in global mode, the catch-all's), and one that is
// but has waited for it less than waited, however long it was held before. r.mu is held.
func (r *Relay) waitedForLocked(waited time.Duration) int {
	now := r.opts.Now()
	n := 0
	for _, parts := range r.held {
		for _, h := range parts {
			if (h.pinned == nil && !h.awaitingPolicy) || now.Sub(h.awaitingSince) < waited {
				n += h.p.records
			}
		}
	}
	return n
}

// countQueuedAtExit counts the outbox as queued, not lost: the next relay delivers it.
func (r *Relay) countQueuedAtExit() {
	routes, _ := r.outbox.routes()
	for _, rt := range routes {
		entries, _ := r.outbox.list(rt)
		for _, e := range entries {
			r.stats.add("queued_at_exit."+string(e.signal), e.records)
		}
	}
}

// Holding reports whether any part waits in the hold.
func (r *Relay) Holding() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.heldN > 0
}

// Quiesce prepares the relay to stop without losing what it holds in memory: when nothing is
// held, no export is being read or routed, and none has arrived for quiet, it answers every
// later export 503, which an exporter sends again, to the next relay, and reports true. What
// is queued is on disk, so it does not count.
func (r *Relay) Quiesce(quiet time.Duration) bool { return r.quiesce(quiet, false, 0) }

// QuiesceDroppingHold is Quiesce for a relay that may wait no longer for the parts that have
// waited at least waited for their team's policy (pinned, or awaitingPolicy): those are
// dropped at exit, and counted as lost (dropHeldAtExit); an update that fixes what holds
// them, a retired sign-in's, must not wait on them. One that began waiting since, a fresh
// team's moments before its fetch, is still given that long. Every other held part (a
// conversation start, a span waiting for its session) is still waited for, as Quiesce does.
func (r *Relay) QuiesceDroppingHold(quiet, waited time.Duration) bool {
	return r.quiesce(quiet, true, waited)
}

func (r *Relay) quiesce(quiet time.Duration, dropHold bool, waited time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	waiting := r.heldN
	if dropHold {
		waiting = r.waitedForLocked(waited)
	}
	if waiting > 0 || r.inflight > 0 || r.opts.Now().Sub(r.lastSeen) < quiet {
		return false
	}
	r.refusing = true
	return true
}

// Idle reports how long the relay has had no export, if it holds and queues nothing.
func (r *Relay) Idle() (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.heldN > 0 {
		return 0, false
	}
	for _, s := range r.senders {
		if s.busy() {
			return 0, false
		}
	}
	return r.opts.Now().Sub(r.lastSeen), true
}
