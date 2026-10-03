package relay

import (
	"context"
	"strings"
	"time"
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
			// Claims that landed since the last sweep still count; the rest waits on disk for the next relay.
			r.sweep()
			r.flushHeld()
			r.countHeldAtExit()
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
			r.flushHeld()
			if now := time.Now(); now.Sub(lastJanitor) >= time.Minute {
				r.janitor(now)
				lastJanitor = now
			}
			if now := r.opts.Now(); now.Sub(lastBeat) >= r.opts.HeartbeatEvery {
				// Off the sweep's goroutine, so a slow host cannot hold up the relay.
				reason := HeartbeatInterval
				if !beaten {
					reason, beaten = HeartbeatStart, true
				}
				go func() { _ = r.heartbeat(ctx, reason) }()
				lastBeat = now
			}
		}
	}
}

func (r *Relay) janitor(now time.Time) {
	for rt, n := range r.sweepOutbox(now) {
		r.mu.Lock()
		s := r.senders[rt]
		r.mu.Unlock()
		if s != nil {
			s.delivered(n)
		}
	}
}

// countHeldAtExit counts what the store keeps for the next relay, or drops what it could
// not keep, and lets go of the hold without touching the store.
func (r *Relay) countHeldAtExit() {
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	for key, parts := range r.held {
		for _, h := range parts {
			if _, kept := r.store.on[h.p]; kept {
				r.stats.add("held_at_exit."+string(h.p.signal), h.p.records)
				continue
			}
			reason := "unclaimed_at_exit"
			if strings.HasPrefix(key, tracePrefix) {
				reason = "no_session_trace_at_exit"
			}
			r.stats.dropped(h.p.signal, reason, h.p.records)
		}
	}
	clear(r.held)
	r.heldN, r.heldBytes = 0, 0
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
