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
	StateID() string
	Generation() int64
	TeamWaits() (Rechecked, bool)
	Entry(q Queued) Rechecked
}

type (
	// Outcome classifies Router.Route's placement decision.
	Outcome int
	// Why names the reason a routed part is held or dropped.
	Why string
	// Verdict classifies a Rechecker's decision for a pinned or queued part.
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
		pinnedHold      map[string][]heldPart
		teamSenders     map[teamRoute]*sender
		teamRecovered   map[teamRoute]bool
		teamDiscoverErr bool
		mixedSeen       map[string]time.Time
		nudging         map[string]bool
	}
)

func newTeamState() teamState {
	return teamState{
		pinnedHold:    map[string][]heldPart{},
		teamSenders:   map[teamRoute]*sender{},
		teamRecovered: map[teamRoute]bool{},
		mixedSeen:     map[string]time.Time{},
		nudging:       map[string]bool{},
	}
}

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

// EffectiveEnd returns EndAt when non-zero, falling back to At.
func (rec Record) EffectiveEnd() time.Time { return nonZeroTime(rec.EndAt, rec.At) }

// EffectiveEnd returns EndAt when non-zero, falling back to At.
func (q Queued) EffectiveEnd() time.Time { return nonZeroTime(q.EndAt, q.At) }
func nonZeroTime(a, b time.Time) time.Time {
	if !a.IsZero() {
		return a
	}
	return b
}
func (r *Relay) teams() bool { return r.opts.Router != nil && r.opts.Router.Active() }
func (r *Relay) takesUnplaced() bool {
	if r.teams() {
		return r.opts.Router.UnknownToPersonal()
	}
	_, global := r.catchAll()
	return global
}
func (r *Relay) sessionOf(key string, pid int) (session, held string) {
	if ppid, ok := procOf(key); ok {
		return r.exitedSession(ppid)
	}
	if session = r.sessionFor(key); session != "" {
		return session, ""
	}
	if pid != 0 {
		session, held = r.exitedSession(pid)
	}
	return session, cmp.Or(held, whyNoTrace)
}
func placementKey(pl claim.Claim) string {
	return strings.Join([]string{string(pl.Kind), pl.ProjectID, pl.Run, pl.Profile, pl.Account, pl.Repository.Origin, pl.Root, pl.Cwd, pl.Tool}, "\x00")
}
func crossedPlacement(raw claim.Claim, pid int, c claim.Claim, start, end time.Time) string {
	if end.IsZero() || end.Equal(start) {
		return ""
	}
	want := placementKey(c)
	for i := -1; i < len(raw.Placements); i++ {
		at := end
		if i >= 0 {
			if at = raw.Placements[i].Since; !at.After(start) || !at.Before(end) {
				continue
			}
		}
		if pl, ok := raw.At(pid, at); !ok || placementKey(pl) != want {
			if ok && pl.Kind == "" && pl.ProjectID == "" {
				return whyNotCollected
			}
			return string(WhyMixed)
		}
	}
	return ""
}
func (r *Relay) decideTeams(key string, p *part) verdict {
	if ppid, ok := procOf(key); ok && r.collectsNone(ppid, p.startAt) && (p.endAt.IsZero() || p.endAt.Equal(p.startAt) || r.collectsNone(ppid, p.endAt)) {
		return verdict{why: whyNotCollected, final: true}
	}
	session, held := r.sessionOf(key, p.pid)
	rec := Record{
		Session: session,
		Account: p.account,
		At:      p.startAt,
		EndAt:   p.endAt,
		Narrow:  p.narrow,
		Claimed: p.claimed,
	}
	var how attribution
	if rec.Session != "" {
		if strings.HasPrefix(key, procPrefix) || r.sessionFor(key) == "" {
			how = attribution{how: semconv.TermaRelayAttributionProcess, session: rec.Session}
		}
		held = whyUnclaimed
		if raw, ok := r.lookup(rec.Session); ok {
			held = whyProcess
			if c, ok := raw.At(p.pid, p.startAt); ok {
				if c.Kind == "" && c.ProjectID == "" {
					return verdict{why: whyNotCollected, final: true}
				}
				if why := crossedPlacement(raw, p.pid, c, p.startAt, p.endAt); why != "" {
					v := verdict{why: why, final: true}
					if why == string(WhyMixed) {
						v.blocker = "mixed:" + rec.Session
					}
					return v
				}
				rec.Placement = c
				rec.Placed = true
				held = ""
			}
		}
	}
	routed := r.opts.Router.Route(rec)
	v := verdict{
		why:     routed.Why.reason(),
		blocker: routed.Blocker,
		mixedAt: routed.MixedAt,
		session: rec.Session,
		c:       rec.Placement,
	}
	q := routed.Queued
	if q.Enrollment != "" {
		q.Account = cmp.Or(q.Account, p.account)
		q.Session = cmp.Or(q.Session, rec.Session)
		q.Run = cmp.Or(q.Run, rec.Placement.Run)
		q.At = nonZeroTime(q.At, p.startAt)
		q.EndAt = nonZeroTime(q.EndAt, p.endAt)
	}
	if !rec.Placed {
		how = attribution{how: semconv.TermaRelayAttributionCatchAll}
	}
	switch routed.Outcome {
	case Drop:
		v.final = true
	case Hold:
		if q.Enrollment != "" {
			v.pin = &q
			v.how = how
		} else if held != "" && routed.Why == WhyUnknown {
			v.why = held
		}
	case Send:
		if pol, ok := r.teamReady(q.Enrollment, rec.Placement.Tool, p.account, false); ok {
			v.ok = true
			v.why = ""
			v.pol = pol
			v.q = &q
			v.how = how
		} else {
			v.why = whyNoKey
			v.pin = &q
			v.how = how
		}
	}
	return v
}
func (r *Relay) teamReady(enrollment, tool, account string, fresh bool) (Policy, bool) {
	if !r.laneActive(account) {
		return Policy{}, false
	}
	now, key, ttl := r.opts.Now(), "\x01team\x00"+account+"\x00"+enrollment+"\x00"+tool, r.opts.PolicyCacheTTL
	if ttl > 0 && !fresh {
		r.cache.mu.Lock()
		e, hit := r.cache.policies[key]
		r.cache.mu.Unlock()
		if hit && now.Sub(e.at) < ttl && (!e.ok || e.pol.resolvedFor(account)) {
			return e.pol, e.ok && r.mayLeaveAccount(account, e.pol.Key != "")
		}
	}
	pol, err := r.opts.Router.Resolve(enrollment, tool, account)
	ok := err == nil && pol.Endpoint != "" && (pol.Key != "" || r.opts.SignIn != nil) && pol.resolvedFor(account)
	if ttl > 0 {
		r.cache.mu.Lock()
		r.cache.policies[key] = cachedPolicy{pol, ok, now}
		r.cache.mu.Unlock()
	}
	return pol, ok && r.mayLeaveAccount(account, pol.Key != "")
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

// OnStateChange invalidates cached team policies and immediately wakes matching teamSenders.
func (r *Relay) OnStateChange(enrollments ...string) {
	r.invalidateTeamPolicy(enrollments...)
	for _, s := range r.allTeamSenders() {
		if len(enrollments) == 0 || slices.Contains(enrollments, s.team.enrollment) {
			s.setKeyless(false)
			select {
			case s.stateChange <- struct{}{}:
			default:
			}
			s.poke()
		}
	}
}
func (r *Relay) dropStats(sig Signal, why, blocker string, n int) {
	r.stats.dropped(sig, why, n)
	if blocker != "" {
		r.stats.block(blocker, n)
	}
}
