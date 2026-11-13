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
	// User is who parts sent to team, selected in cfg's profile, at endpoint name, by
	// Mirador user id; "" names no one.
	User func(cfg *config.Config, team routing.Team, endpoint string) string
	// Mint asks for the missing key of a team selected in cfg's profile; nil when none may
	// be minted.
	Mint func(cfg *config.Config, team routing.Team)
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Active is the profile hooks route by now, as `terma config use` last chose it; nil
	// keeps the relay's own.
	Active func() string
	// Profile is a profile's configuration: its sign-in environment, ingest and agents. Nil
	// takes the relay's own for every profile.
	Profile func(name string) (*config.Config, error)
}

// Teams is the relay's Router on a migrated machine: it routes each record by where it ran
// over the routing state of the profile its session's hooks used (routing.Router), which
// its placement names, the active profile's for a record with none; checks its run in the
// run ledger first; and gives each team's key from keys.v2.json alone. A profile switched
// while the relay runs never routes a session its hooks placed under the other one. A
// routing state that is missing, unreadable, or still being migrated sends nothing.
type Teams struct {
	cfg  *config.Config
	deps TeamsDeps

	mu        sync.Mutex
	migrated  bool
	checkedAt time.Time
	// active is the profile hooks route by now, read at activeAt.
	active   string
	activeAt time.Time
	// profiles are each profile's routing as last read, by name; owners the profile each
	// enrollment was last found in.
	profiles map[string]*profileRoute
	owners   map[string]string
	// ttl is how long the active profile and a routing state read are used: stateTTL.
	ttl time.Duration

	// A locked keychain is asked again only after keychainRetry, as Resolver does.
	keyMu       sync.Mutex
	lockedUntil time.Time
	lockedErr   error

	// users are who each organization's parts name, by organization and ingest, kept for
	// userTTL: asking reads the keychain, and the relay resolves far more often.
	userMu sync.Mutex
	users  map[string]cachedUser

	// ledgers are the run ledgers Route read, by session (Teams.ledger).
	ledgerMu   sync.Mutex
	ledgers    map[string]cachedLedger
	ledgerTTL  time.Duration
	readLedger func(stateDir, idOrAlias string) runs.Snapshot
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
	return &Teams{cfg: cfg, deps: deps, users: map[string]cachedUser{}, profiles: map[string]*profileRoute{}, owners: map[string]string{}, ttl: stateTTL,
		ledgers: map[string]cachedLedger{}, ledgerTTL: ledgerTTL, readLedger: runs.Read}
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

// UnknownToPersonal is the Unknown rule now (routing.UnknownToPersonal).
func (t *Teams) UnknownToPersonal() bool {
	pr := t.route("")
	return pr.ok && routing.UnknownToPersonal(pr.st, t.deps.Now())
}

// Route decides where a record goes, under the profile its placement names (the active
// one's for a record with none): dropped when its run was Mixed by its time, whatever its
// placement says, and before any catch-all; else wherever routing.Router.Route sends or
// holds its location.
func (t *Teams) Route(rec relay.Record) relay.Routed {
	profile := ""
	if rec.Placed {
		profile = rec.Placement.Profile
	}
	pr := t.route(profile)
	st, router := pr.st, pr.router
	if !pr.ok {
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
		tm := st.Team(d.EnrollmentID)
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
// state of the profile that selects it as it stands now: the team's state is judged once
// for the whole pass, and each session's run ledger read at most once.
func (t *Teams) Recheck(enrollment string) relay.Rechecker {
	pr, found, waits := t.owner(enrollment)
	return &recheck{stateDir: t.cfg.StateDir, enrollment: enrollment, st: pr.st, router: pr.router, ok: found || !waits,
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
	tm := p.st.Team(p.enrollment)
	if tm == nil {
		return relay.Rechecked{}, false
	}
	switch tm.StateAt(p.now) {
	case routing.Active, routing.Refused:
		return relay.Rechecked{}, false
	}
	return relay.Rechecked{Verdict: relay.Wait, Why: relay.WhyBlocked, Blocker: "team:" + p.enrollment}, true
}

// Entry judges a queued entry: it is dropped once its run was Mixed by its time (the run
// ledger), since telemetry is content (runs.Locational), then routing.Router.Recheck decides.
func (p *recheck) Entry(q relay.Queued) relay.Rechecked {
	st, router, ok := p.st, p.router, p.ok
	if !ok {
		return relay.Rechecked{Verdict: relay.Wait, Why: relay.WhyNoState}
	}
	if _, mixed := p.mixedAt(q.Session, q.At); mixed {
		return relay.Rechecked{Verdict: relay.Discard, Why: relay.WhyMixed, Blocker: mixedBlocker(q.Session)}
	}
	if router == nil {
		// No profile selects it any longer.
		return relay.Rechecked{Verdict: relay.Discard, Why: relay.WhyDeselected}
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

// Selected reports whether enrollment is still selected, in any profile; true while a
// state that might select it cannot be read, so a lost state drops no queue before a new
// one says so.
func (t *Teams) Selected(enrollment string) bool {
	_, found, waits := t.owner(enrollment)
	return found || waits
}

// Legacy maps a part queued before the migration for project to its cutover enrollment
// (routing.Migration.Legacy), by the location its claim named: it has no session run. While
// the routing state cannot be read, as between the migration marking the machine migrated
// and its commit, the part is mapped to no enrollment yet, and waits.
func (t *Teams) Legacy(project string, repo config.Repository) (relay.Queued, bool) {
	pr := t.route("")
	st := pr.st
	if !pr.ok {
		return relay.Queued{}, true
	}
	if st.Migration == nil {
		return relay.Queued{}, false
	}
	enrollment := st.Migration.Legacy[project]
	tm := st.Team(enrollment)
	if enrollment == "" || tm == nil {
		return relay.Queued{}, false
	}
	q := relay.Queued{Enrollment: enrollment, Project: tm.TeamID, Kind: string(routing.Unknown), Narrow: true}
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

// Resolve is enrollment's send policy, under the profile that selects it: the ingest its
// key was stored for, its key from keys.v2.json (never another team's, nor an agent's),
// its content rules, and the user parts name. A missing key is minted with the team's
// organization's sign-in, unless the team is Suspended or Refused, which is never given
// one.
func (t *Teams) Resolve(enrollment, tool string) (relay.Policy, error) {
	pr, found, _ := t.owner(enrollment)
	if !found {
		return relay.Policy{}, errNoTeam
	}
	tm, cfg := pr.st.Team(enrollment), pr.cfg
	if tm.AuthURL != cfg.AuthURL {
		return relay.Policy{}, errOtherEnvironment
	}
	key, hosts, err := t.key(enrollment)
	if err != nil {
		return relay.Policy{}, err
	}
	if key == "" {
		if s := tm.StateAt(t.deps.Now()); s != routing.Suspended && s != routing.Refused && t.deps.Mint != nil {
			t.deps.Mint(cfg, *tm)
		}
		return relay.Policy{}, relay.ErrNoKey
	}
	chosen := cfg.Harnesses
	file, err := config.LoadFile(t.cfg.Dir)
	if err != nil {
		return relay.Policy{}, err
	}
	if p := file.Profiles[cfg.ProfileName]; p != nil {
		chosen = p.Harnesses
	} else if cfg.ProfileName != "" {
		return relay.Policy{}, errors.New("profile removed; restart the relay")
	}
	endpoint := cfg.OTLPURL
	if !t.deps.OTLPPinned {
		endpoint = cmp.Or(hosts.OTLP, cfg.OTLPURL)
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
	pol.User = t.user(cfg, *tm, endpoint)
	return pol, nil
}

// user is who parts sent to tm, selected in cfg's profile, at endpoint name
// (TeamsDeps.User), asked at most once per userTTL.
func (t *Teams) user(cfg *config.Config, tm routing.Team, endpoint string) string {
	if t.deps.User == nil {
		return ""
	}
	key := cfg.ProfileName + "\x00" + tm.OrganizationID + "\x00" + tm.AuthURL + "\x00" + endpoint
	t.userMu.Lock()
	defer t.userMu.Unlock()
	if u, ok := t.users[key]; ok && time.Since(u.at) < userTTL {
		return u.id
	}
	id := t.deps.User(cfg, tm, endpoint)
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
