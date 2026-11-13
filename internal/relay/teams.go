package relay

import (
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// On a migrated machine the relay places each record by where it ran, onto one of the
// profile's selected teams, and sends it with that team's key and content rules alone
// (Router). The engine never reads the routing state itself: the daemon decides, with
// internal/routing and the run ledger, and the engine holds, queues, rechecks and sends.

// Router decides for a migrated machine (Options.Router). Until Migrated reports true,
// and wherever it is nil, the single-team rules apply: Lookup, Resolve and CatchAll.
type Router interface {
	// Migrated reports whether the machine routes by teams. It turns on once, when the
	// relay migrates the machine at start, and never off.
	Migrated() bool
	// UnknownToPersonal reports whether a record whose location is unknown, arriving now,
	// may go to personal: a record that arrived while it could not never may (Record.Narrow).
	UnknownToPersonal() bool
	// Route is where a record goes, checked against its run's Mixed state first.
	Route(rec Record) Routed
	// Recheck starts one pass over enrollment's queue, as a sender makes before it sends
	// (Rechecker), against the routing state as it stands now.
	Recheck(enrollment string) Rechecker
	// Resolve is an enrollment's send policy for the tool that claimed a record: its
	// team's ingest and key, its content rules and the user it names. ErrNoKey while a
	// key is minted; another error while the key cannot be read now (a locked keychain).
	Resolve(enrollment, tool string) (Policy, error)
	// Selected reports whether an enrollment is still selected: an ended one's queue is
	// dropped. True while the routing state cannot be read, which sends nothing anyway.
	Selected(enrollment string) bool
	// Legacy is the destination of a part queued before the migration for project, in
	// repository repo: its cutover enrollment; false when it has none, and it is dropped.
	// True with no enrollment while the routing state cannot be read: it waits.
	Legacy(project string, repo config.Repository) (Queued, bool)
}

// Record is what the router is told about a record: its session, the placement in force
// when it was made, and how it arrived.
type Record struct {
	// Session is the session ID, or subagent ID, the record named; "" when none is known.
	Session string
	// Placement is the session's placement in force at At (claim.Claim.At), when Placed.
	Placement claim.Claim
	Placed    bool
	// At is the record's time; zero when it carries none.
	At time.Time
	// Narrow: the record arrived while an unknown record could not go to personal.
	Narrow bool
	// Claimed: an agent's hooks claim every session its key names, so the record waits
	// for its placement and never goes to personal unplaced.
	Claimed bool
}

// Outcome is what the router decided for a record.
type Outcome int

// Outcomes.
const (
	// Hold the record in memory, deciding again until its hold runs out.
	Hold Outcome = iota
	// Send it to Routed.Queued's enrollment.
	Send
	// Drop it now.
	Drop
)

// Why is why the router held or dropped a record, which names the drop in the relay's
// counters.
type Why int

// Reasons.
const (
	// WhyUnknown: no placement says where it ran, and it may not go to personal.
	WhyUnknown Why = iota
	// WhyMixed: its run crossed organizations; dropped at once.
	WhyMixed
	// WhyPending: the binding its team's matching entry needs is undecided.
	WhyPending
	// WhyBlocked: its team cannot send now: expired, signing in, suspended or refused.
	WhyBlocked
	// WhyNoTeam: no selected team collects where it ran, or no longer does; dropped.
	WhyNoTeam
	// WhyNoState: the routing state is missing, unreadable or not yet committed.
	WhyNoState
	// WhyDeselected: queued for a team since unticked or released.
	WhyDeselected
)

// reason is w as the relay's counters name it.
func (w Why) reason() string {
	switch w {
	case WhyMixed:
		return whyMixed
	case WhyPending:
		return whyPending
	case WhyBlocked:
		return whyBlocked
	case WhyNoTeam:
		return whyNoTeam
	case WhyNoState:
		return whyNoState
	case WhyDeselected:
		return whyDeselected
	}
	return whyUnknown
}

// Routed is the router's decision about one record.
type Routed struct {
	Outcome Outcome
	// Queued is the destination of a record sent, as the outbox keeps it.
	Queued Queued
	// Why names a hold or a drop.
	Why Why
	// Blocker names what kept the record from leaving: a pending binding, a blocked team or
	// a Mixed run. It is counted on this machine only (Snapshot.Blocked).
	Blocker string
	// MixedAt, set, is when the record's run became Mixed: the relay then drops that run's
	// queued entries from that time on.
	MixedAt time.Time
}

// Verdict is what becomes of a queued entry when it is about to be sent.
type Verdict int

// Verdicts.
const (
	// Wait: keep it queued; its team may recover.
	Wait Verdict = iota
	// SendNow: send it to its own team.
	SendNow
	// Discard: drop it. It is never sent to another team.
	Discard
)

// Rechecker judges an enrollment's queued entries in one pass over the queue: it reads
// the team's state once, and each session's run at most once.
type Rechecker interface {
	// TeamWaits reports whether every entry waits for the team itself, which cannot send
	// now but may recover, and why: no entry is judged then, and no new part changes it.
	TeamWaits() (Rechecked, bool)
	// Entry is what becomes of a queued entry now: sent only to the team it was routed
	// to, while that team still takes it; kept while it may recover; dropped otherwise.
	Entry(q Queued) Rechecked
}

// Rechecked is a queued entry's verdict, and for a drop why, and what blocked it.
type Rechecked struct {
	Verdict Verdict
	Why     Why
	Blocker string
}

// Queued is where a queued record goes, as the outbox keeps it with the record: fixed
// when it is first routed, and rechecked, never routed again, before it is sent.
type Queued struct {
	// Enrollment is the selected team it was routed to; Project that team's ID.
	Enrollment string `json:"enrollment"`
	Project    string `json:"project"`
	// Kind is the record's location kind (repository, outside_git or unknown), with the
	// Repository's origin for a repository, and Narrow for an unknown record that arrived
	// while personal could not take it.
	Kind       string `json:"kind"`
	Repository string `json:"repository,omitempty"`
	Narrow     bool   `json:"narrow,omitempty"`
	// Session is the session ID, or subagent ID, the record's run is known by; At the
	// record's time, which picks the run in the ledger.
	Session string    `json:"session,omitempty"`
	At      time.Time `json:"at,omitzero"`
	// Own: the location is the record's own, not its run's, so its run turning Mixed does
	// not drop it.
	Own bool `json:"own,omitempty"`
}

// Why a part cannot leave on a migrated machine; the same reason names the drop.
const (
	whyMixed      = "mixed_run"        // its run crossed organizations: dropped at once
	whyPending    = "pending_binding"  // the binding its team's entry needs is undecided
	whyBlocked    = "team_blocked"     // its team cannot send now (expired, signing in, suspended, refused)
	whyUnknown    = "unknown_location" // no placement says where it ran
	whyNoTeam     = "no_team"          // no selected team collects where it ran: dropped at once
	whyNoState    = "no_routing_state" // the routing state is missing, unreadable or not committed
	whyDeselected = "team_deselected"  // queued for a team since unticked or released
	whyLegacy     = "legacy_unmapped"  // queued before the migration for a team it does not map
)

// teams reports whether the relay routes by teams now.
func (r *Relay) teams() bool { return r.opts.Router != nil && r.opts.Router.Migrated() }

// takesUnplaced reports whether a part no session or process can place may leave: global
// mode's catch-all on a machine not migrated, the Unknown rule on one that is.
func (r *Relay) takesUnplaced() bool {
	if r.teams() {
		return r.opts.Router.UnknownToPersonal()
	}
	_, global := r.catchAll()
	return global
}

// decideTeams is decide on a migrated machine: the part's session, its placement in force
// at the part's time, and the router's decision, with the team's send policy when it is
// sent. A part naming no session goes by its exited process, as decide's; one no session
// places is unknown.
func (r *Relay) decideTeams(key string, p *part) verdict {
	rec := Record{At: p.at, Narrow: p.narrow, Claimed: p.claimed}
	var how attribution
	held := "" // why the part's session is unknown, kept over the router's unknown
	byProcess := func(pid int) {
		if s, why := r.exitedSession(pid); s != "" {
			rec.Session, held, how = s, "", attribution{how: semconv.TermaRelayAttributionProcess, session: s}
		} else if held == "" {
			held = why
		}
	}
	if pid, isProc := procOf(key); isProc {
		byProcess(pid)
	} else if rec.Session = r.sessionFor(key); rec.Session == "" {
		held = whyNoTrace
		if p.pid != 0 {
			byProcess(p.pid)
		}
	}
	if rec.Session != "" {
		// Unplaced, the part waits as decide's would: for its session's claim (a conversation
		// start as long as a trace), or for a placement covering its process.
		held = whyUnclaimed
		if c, ok := r.lookup(rec.Session); ok {
			held = whyProcess
			if c, ok = c.At(p.pid, p.at); ok {
				if c.Kind == "" && c.ProjectID == "" {
					// A legacy Mark: not collected, everywhere.
					return verdict{why: whyNotCollected, final: true}
				}
				rec.Placement, rec.Placed, held = c, true, ""
			}
		}
	}
	routed := r.opts.Router.Route(rec)
	v := verdict{why: routed.Why.reason(), blocker: routed.Blocker, mixedAt: routed.MixedAt, session: rec.Session}
	switch routed.Outcome {
	case Drop:
		v.final = true
	case Hold:
		if held != "" && routed.Why == WhyUnknown {
			v.why = held
		}
	case Send:
		pol, ok := r.resolveTeam(routed.Queued.Enrollment, rec.Placement.Tool)
		if !ok {
			v.why = whyNoKey
			return v
		}
		q := routed.Queued
		v.ok, v.why, v.pol, v.c, v.q, v.how = true, "", pol, rec.Placement, &q, how
		if !rec.Placed {
			// Placed by the Unknown rule, as global mode's catch-all placed what no claim did.
			v.how = attribution{how: semconv.TermaRelayAttributionCatchAll}
		}
	}
	return v
}

// deliverTeam applies the team's content policy to a part the router sent, stamps it with
// the team's project and the placement's checkout, and queues it for its enrollment.
// deliverMu is held.
func (r *Relay) deliverTeam(v verdict, p *part) {
	if !r.dress(v.pol, p, v.q.Project, v.c, v.how) {
		return
	}
	r.enqueueTeam(*v.q, v.c.Tool, p)
}

// resolveTeam is Router.Resolve, cached as resolve caches a claim's policy.
func (r *Relay) resolveTeam(enrollment, tool string) (Policy, bool) {
	now := r.opts.Now()
	key := strings.Join([]string{"\x01team", enrollment, tool}, "\x00")
	if ttl := r.opts.PolicyCacheTTL; ttl > 0 {
		r.cache.mu.Lock()
		e, hit := r.cache.policies[key]
		r.cache.mu.Unlock()
		if hit && now.Sub(e.at) < ttl {
			return e.pol, e.ok
		}
	}
	pol, err := r.opts.Router.Resolve(enrollment, tool)
	ok := err == nil && pol.Endpoint != "" && pol.Key != ""
	if r.opts.PolicyCacheTTL > 0 {
		r.cache.mu.Lock()
		r.cache.policies[key] = cachedPolicy{pol, ok, now}
		r.cache.mu.Unlock()
	}
	return pol, ok
}

// block counts what kept records from leaving, where a router named it.
func (r *Relay) block(blocker string, n int) {
	if blocker != "" {
		r.stats.block(blocker, n)
	}
}
