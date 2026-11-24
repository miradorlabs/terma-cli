package relay

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Router places records on a routed machine using routing.State and the run ledger.
type Router interface {
	Active() bool
	UnknownToPersonal() bool
	Route(rec Record) Routed
	Recheck(enrollment, account string) Rechecker
	Resolve(enrollment, tool, account string) (Policy, error)
	Selected(enrollment, account string) bool
}

// Rechecker re-evaluates destination-pinned parts under a single routing state snapshot.
type Rechecker interface {
	Generation() int64
	TeamWaits() (Rechecked, bool)
	Entry(q Queued) Rechecked
}

type (
	// Outcome classifies Router.Route's placement decision for a telemetry part.
	Outcome int
	// Why names the reason a routed part is held or dropped.
	Why string
	// Verdict classifies a Rechecker's decision for a destination-pinned or queued part.
	Verdict int
	// Record is the telemetry interval and placement metadata supplied to Router.Route.
	Record struct {
		Session, Account        string
		Placement               claim.Claim
		At, EndAt               time.Time
		Placed, Narrow, Claimed bool
	}
	// Routed is the decision returned by Router.Route for a telemetry part.
	Routed struct {
		Outcome Outcome
		Queued  Queued
		Why     Why
		Blocker string
		MixedAt time.Time
	}
	// Rechecked is the verdict returned by a Rechecker for a pinned or queued entry.
	Rechecked struct {
		Verdict Verdict
		Why     Why
		Blocker string
		MixedAt time.Time
	}
	// Queued is a routed record's pinned destination in pinnedHold and .teams/ outbox headers.
	Queued struct {
		Profile    string    `json:"profile,omitempty"`
		Enrollment string    `json:"enrollment"`
		Project    string    `json:"project"`
		Account    string    `json:"account,omitempty"`
		Kind       string    `json:"kind"`
		Repository string    `json:"repository,omitempty"`
		Narrow     bool      `json:"narrow,omitempty"`
		HoldKey    string    `json:"hold_key,omitempty"`
		Session    string    `json:"session,omitempty"`
		Run        string    `json:"run,omitempty"`
		At         time.Time `json:"at,omitzero"`
		EndAt      time.Time `json:"end_at,omitzero"`
	}
	teamState struct {
		pinnedHold  map[string][]heldPart
		teamSenders map[teamRoute]*sender
		mixedSeen   map[string]time.Time
		nudging     map[string]bool
	}
)

// Routing outcomes, recheck verdicts, and hold/drop reasons.
const (
	Hold Outcome = iota
	Send
	Drop
	Wait Verdict = iota
	SendNow
	Discard
	WhyUnknown    Why = "unknown_location"
	WhyMixed      Why = "mixed_run"
	WhyPending    Why = "pending_binding"
	WhyBlocked    Why = "team_blocked"
	WhyNoTeam     Why = "no_team"
	WhyNoState    Why = "no_routing_state"
	WhyDeselected Why = "team_deselected"
)

func (w Why) reason() string { return cmp.Or(string(w), string(WhyUnknown)) }

// EffectiveEnd returns rec.EndAt when present, falling back to rec.At.
func (rec Record) EffectiveEnd() time.Time { return nonZeroTime(rec.EndAt, rec.At) }

// EffectiveEnd returns q.EndAt when present, falling back to q.At.
func (q Queued) EffectiveEnd() time.Time   { return nonZeroTime(q.EndAt, q.At) }
func nonZeroTime(a, b time.Time) time.Time { return map[bool]time.Time{true: a, false: b}[!a.IsZero()] }
func (r *Relay) initTeams() {
	r.pinnedHold, r.teamSenders, r.mixedSeen, r.nudging = map[string][]heldPart{}, map[teamRoute]*sender{}, map[string]time.Time{}, map[string]bool{}
}
func (r *Relay) teams() bool { return r.opts.Router != nil && r.opts.Router.Active() }
func (r *Relay) takesUnplaced() bool {
	if r.teams() {
		return r.opts.Router.UnknownToPersonal()
	}
	_, global := r.catchAll()
	return global
}
func (r *Relay) decideTeams(key string, p *part) verdict {
	rec, how, held := Record{At: p.at, EndAt: p.endAt, Account: p.account, Narrow: p.narrow, Claimed: p.claimed}, attribution{}, ""
	if pid, ok := procOf(key); ok {
		rec.Session, held = r.exitedSession(pid)
	} else if rec.Session = r.sessionFor(key); rec.Session == "" {
		if p.pid != 0 {
			rec.Session, held = r.exitedSession(p.pid)
		}
		held = cmp.Or(held, whyNoTrace)
	}
	if rec.Session != "" {
		if strings.HasPrefix(key, procPrefix) || r.sessionFor(key) == "" {
			how = attribution{how: semconv.TermaRelayAttributionProcess, session: rec.Session}
		}
		held = whyUnclaimed
		if raw, ok := r.lookup(rec.Session); ok {
			held = whyProcess
			if c, ok := raw.At(p.pid, p.at); ok {
				if !p.endAt.IsZero() && !p.endAt.Equal(p.at) {
					if endC, ok := raw.At(p.pid, p.endAt); ok && endC.Kind == claim.KindMixed {
						c = endC
					}
				}
				if c.Kind == "" && c.ProjectID == "" {
					return verdict{why: whyNotCollected, final: true}
				}
				rec.Placement, rec.Placed, held = c, true, ""
			}
		}
	}
	routed := r.opts.Router.Route(rec)
	v, q := verdict{why: routed.Why.reason(), blocker: routed.Blocker, mixedAt: routed.MixedAt, session: rec.Session, c: rec.Placement}, routed.Queued
	if q.Enrollment != "" {
		q.Account, q.Session, q.Run = cmp.Or(q.Account, p.account), cmp.Or(q.Session, rec.Session), cmp.Or(q.Run, rec.Placement.Run)
		q.At, q.EndAt = nonZeroTime(q.At, p.at), nonZeroTime(q.EndAt, p.endAt)
	}
	if !rec.Placed {
		how = attribution{how: semconv.TermaRelayAttributionCatchAll}
	}
	switch routed.Outcome {
	case Drop:
		v.final = true
	case Hold:
		if q.Enrollment != "" {
			v.pin, v.how = &q, how
		} else if held != "" && routed.Why == WhyUnknown {
			v.why = held
		}
	case Send:
		if pol, ok := r.resolveTeam(q.Enrollment, rec.Placement.Tool, p.account, false); ok && r.mayLeaveAccount(p.account, pol.Key != "") && pol.resolvedFor(p.account) {
			v.ok, v.why, v.pol, v.q, v.how = true, "", pol, &q, how
		} else {
			v.why, v.pin, v.how = whyNoKey, &q, how
		}
	}
	return v
}
func (r *Relay) resolveTeam(enrollment, tool, account string, fresh bool) (Policy, bool) {
	now, key := r.opts.Now(), "\x01team\x00"+account+"\x00"+enrollment+"\x00"+tool
	if ttl := r.opts.PolicyCacheTTL; ttl > 0 && !fresh {
		r.cache.mu.Lock()
		e, hit := r.cache.policies[key]
		r.cache.mu.Unlock()
		if hit && now.Sub(e.at) < ttl && (!e.ok || e.pol.resolvedFor(account)) {
			return e.pol, e.ok
		}
	}
	pol, err := r.opts.Router.Resolve(enrollment, tool, account)
	ok := err == nil && pol.Endpoint != "" && (pol.Key != "" || r.opts.SignIn != nil) && pol.resolvedFor(account)
	if r.opts.PolicyCacheTTL > 0 {
		r.cache.mu.Lock()
		r.cache.policies[key] = cachedPolicy{pol, ok, now}
		r.cache.mu.Unlock()
	}
	return pol, ok
}
func (r *Relay) invalidateTeamPolicy(enrollments ...string) {
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	for k := range r.cache.policies {
		if rest, ok := strings.CutPrefix(k, "\x01team\x00"); ok && (len(enrollments) == 0 || slices.Contains(enrollments, strings.Split(rest, "\x00")[1])) {
			delete(r.cache.policies, k)
		}
	}
}
func (r *Relay) allTeamSenders() []*sender {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Collect(maps.Values(r.teamSenders))
}

// OnStateChange invalidates cached team policies and immediately wakes matching teamSenders
// past refusedBackoff, resetting backoff to 0 on recovery or Refused purge.
func (r *Relay) OnStateChange(enrollments ...string) {
	r.invalidateTeamPolicy(enrollments...)
	for _, s := range r.allTeamSenders() {
		if len(enrollments) > 0 && !slices.Contains(enrollments, s.team.enrollment) {
			continue
		}
		s.mu.Lock()
		s.blocked = false
		s.mu.Unlock()
		select {
		case s.stateChange <- struct{}{}:
		default:
		}
		s.poke()
	}
}
func (r *Relay) dropStats(sig Signal, why, blocker string, n int) {
	r.stats.dropped(sig, why, n)
	if blocker != "" {
		r.stats.block(blocker, n)
	}
}
