package daemon

import (
	"cmp"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// stateTTL is how long a loaded routing state is used before it is read again: a
// selection, a binding or a refresh reaches the relay within it.
const stateTTL = time.Second

// TeamsDeps is what the team router asks of the CLI.
type TeamsDeps struct {
	// AgentName names the agent behind a tool label.
	AgentName func(tool string) string
	// RelayTargets names the agents among the developer's choices that send through the relay.
	RelayTargets func(selected []string) []string
	// OTLPPinned: the ingest host is pinned (a flag or TERMA_OTLP_URL), whatever host a
	// team's key was stored with.
	OTLPPinned bool
	// User is who parts sent to team at endpoint name, by Mirador user id; "" names no one.
	User func(team routing.Team, endpoint string) string
	// Mint asks for a selected team's missing key; nil when none may be minted.
	Mint func(team routing.Team)
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Teams is the relay's Router on a migrated machine: it routes each record by where it ran
// over the profile's routing state (routing.Router), checks its run in the run ledger
// first, and gives each team's key from keys.v2.json alone. A routing state that is
// missing, unreadable, or still being migrated sends nothing.
type Teams struct {
	cfg  *config.Config
	deps TeamsDeps

	mu        sync.Mutex
	migrated  bool
	checkedAt time.Time
	loadedAt  time.Time
	st        routing.State
	ok        bool
	router    *routing.Router

	// A locked keychain is asked again only after keychainRetry, as Resolver does.
	keyMu       sync.Mutex
	lockedUntil time.Time
	lockedErr   error

	// users are who each organization's parts name, by organization and ingest, kept for
	// userTTL: asking reads the keychain, and the relay resolves far more often.
	userMu sync.Mutex
	users  map[string]cachedUser
}

type cachedUser struct {
	id string
	at time.Time
}

// userTTL is how long a team's user stands: a sign-in as another user of the same
// organization reaches the relay within it.
const userTTL = 5 * time.Minute

// NewTeams is the team router for cfg's profile.
func NewTeams(cfg *config.Config, deps TeamsDeps) *Teams {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Teams{cfg: cfg, deps: deps, users: map[string]cachedUser{}}
}

var _ relay.Router = (*Teams)(nil)

// Migrated reports whether the machine is migrated (config.Migrated), asked at most once
// a second until it is, and never again after.
func (t *Teams) Migrated() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.migrated {
		return true
	}
	if now := time.Now(); t.checkedAt.IsZero() || now.Sub(t.checkedAt) >= stateTTL {
		t.checkedAt = now
		t.migrated = config.Migrated(t.cfg.Dir)
	}
	return t.migrated
}

// state is the profile's committed routing state and its compiled router, reread at most
// once a second; the router is compiled again only for a new generation.
func (t *Teams) state() (routing.State, *routing.Router, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if !t.loadedAt.IsZero() && now.Sub(t.loadedAt) < stateTTL {
		return t.st, t.router, t.ok
	}
	t.loadedAt = now
	// A crash in the middle of migrating collects nothing until the next start commits.
	st, found, err := routing.LoadReady(t.cfg.StateDir, t.cfg.ProfileName)
	switch {
	case err != nil || !found:
		t.ok = false
	default:
		if !t.ok || t.router == nil || !routing.SameGeneration(st, t.st) {
			t.router = routing.NewRouter(st)
		}
		t.st, t.ok = st, true
	}
	return t.st, t.router, t.ok
}

// team is enrollment's team in st, or nil.
func team(st routing.State, enrollment string) *routing.Team {
	for i := range st.Teams {
		if st.Teams[i].EnrollmentID == enrollment {
			return &st.Teams[i]
		}
	}
	return nil
}

// UnknownToPersonal is the Unknown rule now (routing.UnknownToPersonal).
func (t *Teams) UnknownToPersonal() bool {
	st, _, ok := t.state()
	return ok && routing.UnknownToPersonal(st, t.deps.Now())
}

// Route decides where a record goes: dropped when its run was Mixed by its time, whatever
// its placement says, and before any catch-all; else wherever routing.Router.Route sends
// or holds its location.
func (t *Teams) Route(rec relay.Record) relay.Routed {
	st, router, ok := t.state()
	if !ok {
		return relay.Routed{Outcome: relay.Hold, Why: relay.WhyNoState}
	}
	if from, mixed := t.mixedAt(rec.Session, rec.At); mixed || rec.Placed && rec.Placement.Kind == claim.KindMixed {
		if !mixed {
			from = rec.At
		}
		return relay.Routed{Outcome: relay.Drop, Why: relay.WhyMixed, Blocker: mixedBlocker(rec.Session), MixedAt: from}
	}
	loc := location(rec)
	d := router.Route(loc, t.deps.Now())
	switch d.Outcome {
	case routing.Send:
		tm := team(st, d.EnrollmentID)
		if tm == nil {
			return relay.Routed{Outcome: relay.Drop, Why: relay.WhyNoTeam}
		}
		return relay.Routed{Outcome: relay.Send, Queued: relay.Queued{Enrollment: tm.EnrollmentID, Project: tm.TeamID,
			Kind: string(loc.Kind), Repository: loc.Repository, Narrow: loc.Narrow, Session: rec.Session, At: rec.At}}
	case routing.Hold:
		why, blocker := held(d)
		return relay.Routed{Outcome: relay.Hold, Why: why, Blocker: blocker}
	}
	if d.Reason == routing.ReasonMixed {
		return relay.Routed{Outcome: relay.Drop, Why: relay.WhyMixed, Blocker: mixedBlocker(rec.Session)}
	}
	return relay.Routed{Outcome: relay.Drop, Why: relay.WhyNoTeam}
}

// Recheck starts a pass over enrollment's queue (relay.Rechecker), against the routing
// state as it stands now: the team's state is judged once for the whole pass, and each
// session's run ledger read at most once.
func (t *Teams) Recheck(enrollment string) relay.Rechecker {
	st, router, ok := t.state()
	return &recheck{stateDir: t.cfg.StateDir, enrollment: enrollment, st: st, router: router, ok: ok,
		now: t.deps.Now(), runs: map[string]runs.Snapshot{}}
}

// recheck is one pass over an enrollment's queue.
type recheck struct {
	stateDir, enrollment string
	st                   routing.State
	router               *routing.Router
	ok                   bool
	now                  time.Time
	// runs are the sessions' ledgers, each read once in the pass.
	runs map[string]runs.Snapshot
}

// TeamWaits reports whether every entry waits for its team: the routing state cannot be
// read, or the team cannot send now but may recover (expired, signing in, suspended).
// Nothing queued for it may leave then, so no entry is judged until it can: one its team
// would no longer take waits with the rest, and is dropped once the team can send again.
func (p *recheck) TeamWaits() (relay.Rechecked, bool) {
	if !p.ok {
		return relay.Rechecked{Verdict: relay.Wait, Why: relay.WhyNoState}, true
	}
	tm := team(p.st, p.enrollment)
	if tm == nil {
		return relay.Rechecked{}, false
	}
	switch tm.StateAt(p.now) {
	case routing.Active, routing.Refused:
		return relay.Rechecked{}, false
	}
	return relay.Rechecked{Verdict: relay.Wait, Why: relay.WhyBlocked, Blocker: "team:" + p.enrollment}, true
}

// Entry judges a queued entry: one attributed through its run is dropped once the run was
// Mixed by its time (the run ledger), then routing.Router.Recheck decides.
func (p *recheck) Entry(q relay.Queued) relay.Rechecked {
	st, router, ok := p.st, p.router, p.ok
	if !ok {
		return relay.Rechecked{Verdict: relay.Wait, Why: relay.WhyNoState}
	}
	if !q.Own {
		if _, mixed := p.mixedAt(q.Session, q.At); mixed {
			return relay.Rechecked{Verdict: relay.Discard, Why: relay.WhyMixed, Blocker: mixedBlocker(q.Session)}
		}
	}
	loc := routing.Location{Kind: routing.LocationKind(q.Kind), Repository: q.Repository, Narrow: q.Narrow}
	v, d := router.Recheck(routing.Destination{EnrollmentID: q.Enrollment, Location: loc}, p.now)
	switch v {
	case routing.SendNow:
		return relay.Rechecked{Verdict: relay.SendNow}
	case routing.Wait:
		why, blocker := held(d)
		return relay.Rechecked{Verdict: relay.Wait, Why: why, Blocker: blocker}
	}
	switch {
	case !st.Selected(q.Enrollment):
		return relay.Rechecked{Verdict: relay.Discard, Why: relay.WhyDeselected}
	case d.EnrollmentID == q.Enrollment && d.Reason == routing.ReasonBlocked:
		return relay.Rechecked{Verdict: relay.Discard, Why: relay.WhyBlocked, Blocker: "team:" + q.Enrollment}
	}
	return relay.Rechecked{Verdict: relay.Discard, Why: relay.WhyNoTeam}
}

// Selected reports whether enrollment is still selected; true while the state cannot be
// read, so a lost state drops no queue before a new one says so.
func (t *Teams) Selected(enrollment string) bool {
	st, _, ok := t.state()
	return !ok || st.Selected(enrollment)
}

// Legacy maps a part queued before the migration for project to its cutover enrollment
// (routing.Migration.Legacy), as a record with a location of its own: it has no run.
func (t *Teams) Legacy(project string, repo config.Repository) (relay.Queued, bool) {
	st, _, ok := t.state()
	if !ok || st.Migration == nil {
		return relay.Queued{}, false
	}
	enrollment := st.Migration.Legacy[project]
	tm := team(st, enrollment)
	if enrollment == "" || tm == nil {
		return relay.Queued{}, false
	}
	q := relay.Queued{Enrollment: enrollment, Project: tm.TeamID, Kind: string(routing.Unknown), Narrow: true, Own: true}
	if repo.Origin != "" {
		q.Kind, q.Repository, q.Narrow = string(routing.Repository), repo.Origin, false
	}
	return q, true
}

// errNoTeam is Resolve for an enrollment not selected, or a routing state not readable.
var errNoTeam = errors.New("no such selected team")

// errOtherEnvironment is Resolve for a team selected in another environment than the
// profile now points at: its key belongs there, so nothing is sent until it is unticked.
var errOtherEnvironment = errors.New("the team was selected in another environment than this profile's")

// Resolve is enrollment's send policy: the ingest its key was stored for, its key from
// keys.v2.json (never another team's, nor an agent's), its content rules, and the user
// parts name. A missing key is minted with the team's organization's sign-in, unless the
// team is Suspended or Refused, which is never given one.
func (t *Teams) Resolve(enrollment, tool string) (relay.Policy, error) {
	st, _, ok := t.state()
	tm := team(st, enrollment)
	if !ok || tm == nil {
		return relay.Policy{}, errNoTeam
	}
	if tm.AuthURL != t.cfg.AuthURL {
		return relay.Policy{}, errOtherEnvironment
	}
	key, hosts, err := t.key(enrollment)
	if err != nil {
		return relay.Policy{}, err
	}
	if key == "" {
		if s := tm.StateAt(t.deps.Now()); s != routing.Suspended && s != routing.Refused && t.deps.Mint != nil {
			t.deps.Mint(*tm)
		}
		return relay.Policy{}, relay.ErrNoKey
	}
	chosen := t.cfg.Harnesses
	file, err := config.LoadFile(t.cfg.Dir)
	if err != nil {
		return relay.Policy{}, err
	}
	if p := file.Profiles[t.cfg.ProfileName]; p != nil {
		chosen = p.Harnesses
	} else if t.cfg.ProfileName != "" {
		return relay.Policy{}, errors.New("profile removed; restart the relay")
	}
	endpoint := t.cfg.OTLPURL
	if !t.deps.OTLPPinned {
		endpoint = cmp.Or(hosts.OTLP, t.cfg.OTLPURL)
	}
	pol := relay.Policy{Endpoint: endpoint, Key: key, RequireClaim: !tm.Personal || !tm.Policy.Global()}
	pol.IncludePrompts, pol.IncludeToolContent = tm.Policy.Content()
	harness := ""
	if tool != "" && t.deps.AgentName != nil {
		harness = t.deps.AgentName(tool)
	}
	targets := chosen
	if t.deps.RelayTargets != nil {
		targets = t.deps.RelayTargets(chosen)
	}
	if tm.Policy.CollectsNothing || !tm.Policy.Global() && harness != "" && !slices.Contains(targets, harness) {
		pol.IncludePrompts, pol.IncludeToolContent = false, false
		pol.Signals = []string{}
	}
	pol.User = t.user(*tm, endpoint)
	return pol, nil
}

// user is who parts sent to tm at endpoint name (TeamsDeps.User), asked at most once per
// userTTL.
func (t *Teams) user(tm routing.Team, endpoint string) string {
	if t.deps.User == nil {
		return ""
	}
	key := tm.OrganizationID + "\x00" + tm.AuthURL + "\x00" + endpoint
	t.userMu.Lock()
	defer t.userMu.Unlock()
	if u, ok := t.users[key]; ok && time.Since(u.at) < userTTL {
		return u.id
	}
	id := t.deps.User(tm, endpoint)
	t.users[key] = cachedUser{id: id, at: time.Now()}
	return id
}

// key is enrollment's key and its hosts; a locked keychain is asked again only after a while.
func (t *Teams) key(enrollment string) (string, keystore.Hosts, error) {
	t.keyMu.Lock()
	if time.Now().Before(t.lockedUntil) {
		err := t.lockedErr
		t.keyMu.Unlock()
		return "", keystore.Hosts{}, err
	}
	t.keyMu.Unlock()
	key, hosts, err := keystore.GetTeam(t.cfg.Dir, enrollment)
	if err != nil && secret.IsUnavailable(err) {
		t.keyMu.Lock()
		t.lockedUntil, t.lockedErr = time.Now().Add(keychainRetry), err
		t.keyMu.Unlock()
	}
	return key, hosts, err
}

// mixedAt reports whether session's run in force at at was Mixed by then (runs.MixedAt),
// and from when: a ledger that cannot be read is Mixed from the start.
func (t *Teams) mixedAt(session string, at time.Time) (time.Time, bool) {
	if session == "" {
		return time.Time{}, false
	}
	return mixedFrom(runs.Read(t.cfg.StateDir, session), at)
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

// held is why routing held a record, and what blocked it.
func held(d routing.Decision) (relay.Why, string) {
	switch d.Reason {
	case routing.ReasonPending:
		return relay.WhyPending, "pending:" + d.Authority.Key
	case routing.ReasonBlocked:
		return relay.WhyBlocked, "team:" + d.EnrollmentID
	}
	return relay.WhyUnknown, ""
}

// location is where a record ran, as routing reads it: its placement's, or unknown. An
// unplaced record whose agent's hooks claim every session waits for its placement, so it
// is never personal's unplaced. A placement from before the migration names its project's
// repository, where it has one.
func location(rec relay.Record) routing.Location {
	if !rec.Placed {
		return routing.Location{Kind: routing.Unknown, Narrow: rec.Narrow || rec.Claimed}
	}
	c := rec.Placement
	origin := cmp.Or(c.Repository.Origin, c.Remote)
	switch c.Kind {
	case claim.KindMixed:
		return routing.Location{Kind: routing.Mixed}
	case claim.KindOutsideGit:
		return routing.Location{Kind: routing.OutsideGit}
	case claim.KindRepository:
		return routing.Location{Kind: routing.Repository, Repository: origin}
	}
	if origin != "" {
		return routing.Location{Kind: routing.Repository, Repository: origin}
	}
	return routing.Location{Kind: routing.Unknown, Narrow: true}
}
