package relay

import (
	"strconv"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// procPrefix keys a part that names neither session nor trace by the process that sent it.
const procPrefix = "proc:"

func procKey(pid int) string { return procPrefix + strconv.Itoa(pid) }

// Past maxProcSessions a process counts as serving many sessions.
const (
	maxProcs        = 4096
	maxProcSessions = 256
)

// exitGrace lets an exporter's shutdown flush still name a session before its process is attributed.
const exitGrace = 10 * time.Second

type procState struct {
	sessions map[string]bool
	overflow bool
	lastSeen time.Time
	exitedAt time.Time
}

// decideExited attributes a sessionless part to the one session its exited sender named, if claimed.
func (r *Relay) decideExited(pid int, narrow bool) (claim.Claim, Policy, string, bool, attribution) {
	r.mu.Lock()
	st := r.procs[pid]
	var sessions []string
	var exited time.Time
	overflow := false
	if st != nil {
		for s := range st.sessions {
			sessions = append(sessions, s)
		}
		exited, overflow = st.exitedAt, st.overflow
	}
	r.mu.Unlock()
	switch {
	case st == nil || len(sessions) == 0:
		return claim.Claim{}, Policy{}, whyProcessIdle, false, attribution{}
	case exited.IsZero() || r.opts.Now().Sub(exited) < exitGrace:
		return claim.Claim{}, Policy{}, whyProcessRunning, false, attribution{}
	case overflow || len(sessions) != 1:
		return claim.Claim{}, Policy{}, whyAmbiguous, false, attribution{}
	}
	c, pol, why, ok := r.decideClaimed(sessions[0], pid, time.Time{}, narrow)
	if !ok {
		return claim.Claim{}, Policy{}, why, false, attribution{}
	}
	return c, pol, "", true, attribution{how: "process", session: sessions[0]}
}

func (r *Relay) learnProcess(pid int, session string) {
	if pid == 0 || session == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.procs[pid]
	if st == nil {
		if len(r.procs) >= maxProcs {
			r.stats.add("process_index_full", 1)
			return
		}
		st = &procState{sessions: map[string]bool{}}
		r.procs[pid] = st
	}
	st.lastSeen = r.opts.Now()
	if !st.sessions[session] {
		if len(st.sessions) >= maxProcSessions {
			st.overflow = true
			return
		}
		st.sessions[session] = true
	}
}

func (r *Relay) watchProcesses(now time.Time) {
	alive := r.opts.ProcessAlive
	r.mu.Lock()
	defer r.mu.Unlock()
	for pid, st := range r.procs {
		switch {
		case !st.exitedAt.IsZero():
			// Kept past the longest hold, so the sweep that drops its parts still knows why.
			if now.Sub(st.exitedAt) > 2*r.opts.TraceHold {
				delete(r.procs, pid)
			}
		case alive != nil && !alive(pid):
			st.exitedAt = now
		case now.Sub(st.lastSeen) > 2*r.opts.TraceHold:
			// Never exits, or its pid was reused; either way nothing held can be its.
			delete(r.procs, pid)
		}
	}
}
