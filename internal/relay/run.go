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
				r.wg.Go(func() { _ = r.heartbeat(ctx, reason) })
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
	for key, parts := range held {
		for _, h := range parts {
			_, _, why, ok, _ := r.decide(key, h.p.pid, h.p.at, h.p.narrow)
			if ok {
				why = "claimed"
			}
			r.stats.dropped(h.p.signal, why+"_at_exit", h.p.records)
			if r.opts.Logf != nil {
				c, _ := r.lookup(r.sessionFor(key))
				r.opts.Logf("drop %s_at_exit %s: %d %s from pid %d; claim %q pids %v", why, key, h.p.records, h.p.signal, h.p.pid, c.ProjectID, c.PIDs)
			}
		}
	}
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
