package daemon

import (
	"cmp"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

const (
	stateTTL = time.Second
	userTTL  = 5 * time.Minute
)

var (
	errNoTeam           = errors.New("team is no longer selected")
	errOtherEnvironment = errors.New("team was selected in another environment")
	errWrongAccount     = errors.New("team belongs to another account")
)

// TeamsDeps are the CLI hooks daemon.Teams uses to resolve endpoints, users, harnesses,
// and profile configurations.
type TeamsDeps struct {
	AgentName    func(tool string) string
	RelayTargets func(selected []string) []string
	Endpoint     func(cfg *config.Config, projectID string) string
	User         func(cfg *config.Config, team routing.Team, endpoint string) string
	Now          func() time.Time
	Active       func() string
	Profile      func(name string) (*config.Config, error)
}

// Teams implements relay.Router over account-verified routing.State files,
// using the profile's single user token (or keys.json on a server-key profile).
type Teams struct {
	cfg  *config.Config
	deps TeamsDeps

	mu       sync.Mutex
	active   string
	activeAt time.Time
	profiles map[string]*profileRoute
	owners   map[string]string
	ttl      time.Duration

	keyMu       sync.Mutex
	lockedUntil time.Time
	lockedErr   error

	userMu sync.Mutex
	users  map[string]cachedUser
}

type cachedUser struct {
	id string
	at time.Time
}

// NewTeams returns a multi-org relay.Router for cfg's profile.
func NewTeams(cfg *config.Config, deps TeamsDeps) *Teams {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Teams{
		cfg: cfg, deps: deps,
		profiles: map[string]*profileRoute{}, owners: map[string]string{}, ttl: stateTTL,
		users: map[string]cachedUser{},
	}
}

var _ relay.Router = (*Teams)(nil)

// UnknownToPersonal reports whether unplaced records currently route to Personal.
func (t *Teams) UnknownToPersonal() bool {
	pr := t.route("", false)
	return pr.ok && routing.UnknownToPersonal(pr.st, t.deps.Now())
}

// Route decides where rec goes under its placement's profile (or the relay's start profile
// when unplaced) and pins held destinations whenever routing assigns an enrollment.
func (t *Teams) Route(rec relay.Record) relay.Routed {
	profile := ""
	if rec.Placed {
		profile = rec.Placement.Profile
	}
	pr := t.route(profile, false)
	if !pr.ok || (rec.Account != "" && pr.st.Account != rec.Account) {
		return relay.Routed{Outcome: relay.Hold, Why: relay.WhyNoState}
	}
	if rec.Placed && (rec.Placement.Account == "" || rec.Placement.Account != pr.st.Account) {
		return relay.Routed{Outcome: relay.Hold, Why: relay.WhyNoState}
	}
	loc := location(rec)
	d := pr.router.Route(loc, t.deps.Now())
	queuedFor := func(tm routing.Team) relay.Queued {
		return relay.Queued{
			Profile: pr.st.Profile, Enrollment: tm.EnrollmentID, Project: tm.TeamID, Account: pr.st.Account,
			Kind: string(loc.Kind), Repository: loc.Repository, Narrow: loc.Narrow,
			Session: rec.Session, At: rec.At, EndAt: rec.EndAt,
		}
	}
	switch d.Outcome {
	case routing.Send:
		tm, ok := pr.st.Team(d.EnrollmentID)
		if !ok {
			return relay.Routed{Outcome: relay.Drop, Why: relay.WhyNoTeam}
		}
		return relay.Routed{Outcome: relay.Send, Queued: queuedFor(tm)}
	case routing.Hold:
		why, blocker := held(d)
		res := relay.Routed{Outcome: relay.Hold, Why: why, Blocker: blocker}
		if d.EnrollmentID != "" {
			if tm, ok := pr.st.Team(d.EnrollmentID); ok {
				res.Queued = queuedFor(tm)
			}
		}
		return res
	}
	return relay.Routed{Outcome: relay.Drop, Why: relay.WhyNoTeam}
}

// Recheck starts a recheck pass over enrollment's pinned/queued records for account.
func (t *Teams) Recheck(enrollment, account string) relay.Rechecker {
	pr, found, waits := t.owner(enrollment, account, true)
	return &recheck{
		enrollment: enrollment, st: pr.st, router: pr.router,
		ok: found || !waits, now: t.deps.Now(),
	}
}

type recheck struct {
	enrollment string
	st         routing.State
	router     *routing.Router
	ok         bool
	now        time.Time
}

func (p *recheck) StateID() string {
	if !p.ok {
		return ""
	}
	return p.st.StateID
}

func (p *recheck) Generation() int64 {
	if !p.ok {
		return 0
	}
	return p.st.Generation
}

func (p *recheck) TeamWaits() (relay.Rechecked, bool) {
	if !p.ok {
		return relay.Rechecked{Verdict: relay.Wait, Why: relay.WhyNoState}, true
	}
	tm, ok := p.st.Team(p.enrollment)
	if !ok {
		return relay.Rechecked{}, false
	}
	switch tm.StateAt(p.now) {
	case routing.Active, routing.Refused:
		return relay.Rechecked{}, false
	}
	return relay.Rechecked{Verdict: relay.Wait, Why: relay.WhyBlocked, Blocker: "team:" + p.enrollment}, true
}

func (p *recheck) Entry(q relay.Queued) relay.Rechecked {
	if !p.ok {
		return relay.Rechecked{Verdict: relay.Wait, Why: relay.WhyNoState}
	}
	if p.router == nil {
		return relay.Rechecked{Verdict: relay.Discard, Why: relay.WhyDeselected}
	}
	loc := routing.Location{Kind: routing.LocationKind(q.Kind), Repository: q.Repository, Narrow: q.Narrow}
	v, d := p.router.Recheck(routing.Destination{EnrollmentID: q.Enrollment, Location: loc}, p.now)
	switch v {
	case routing.SendNow:
		return relay.Rechecked{Verdict: relay.SendNow}
	case routing.Wait:
		why, blocker := held(d)
		return relay.Rechecked{Verdict: relay.Wait, Why: why, Blocker: blocker}
	}
	switch {
	case !p.st.Selected(q.Enrollment):
		return relay.Rechecked{Verdict: relay.Discard, Why: relay.WhyDeselected}
	case d.EnrollmentID == q.Enrollment && d.Reason == routing.ReasonBlocked:
		return relay.Rechecked{Verdict: relay.Discard, Why: relay.WhyBlocked, Blocker: "team:" + q.Enrollment}
	}
	return relay.Rechecked{Verdict: relay.Discard, Why: relay.WhyNoTeam}
}

// Selected reports whether enrollment is still selected for account, returning true while a
// potentially owning routing state cannot be read or belongs to another account.
func (t *Teams) Selected(enrollment, account string) bool {
	_, found, waits := t.owner(enrollment, account, true)
	return found || waits
}

// Resolve builds the relay.Policy for enrollment under account: user-token profiles leave
// Policy.Key empty so teamSender sends with SignIn.Token + X-Mirador-Project, while
// server-key profiles validate cfg.Team and cfg.ServerKeyAuthURL and populate Policy.Key
// from keys.json so no X-Mirador-Project header is sent.
func (t *Teams) Resolve(enrollment, tool, account string) (relay.Policy, error) {
	pr, found, _ := t.owner(enrollment, account, true)
	if !found {
		return relay.Policy{}, errNoTeam
	}
	tm, ok := pr.st.Team(enrollment)
	if !ok {
		return relay.Policy{}, errNoTeam
	}
	cfg := pr.cfg
	if tm.AuthURL != cfg.AuthURL {
		return relay.Policy{}, errOtherEnvironment
	}
	if account != "" && pr.st.Account != account {
		return relay.Policy{}, errWrongAccount
	}
	endpoint := cfg.OTLPURL
	if t.deps.Endpoint != nil {
		live := *cfg
		endpoint = t.deps.Endpoint(&live, tm.TeamID)
	}
	pol := relay.Policy{Endpoint: endpoint, Account: pr.st.Account, RequireClaim: !tm.Personal || !tm.Policy.Global()}
	if cfg.ServerKeySignIn {
		if pr.st.Account != sendas.NoPersonLane || tm.TeamID != cfg.Team || cfg.AuthURL != cfg.ServerKeyAuthURL {
			return relay.Policy{}, errWrongAccount
		}
		key, err := t.serverKey(cfg.Dir, tm.TeamID)
		if err != nil {
			return relay.Policy{}, err
		}
		if key == "" {
			return relay.Policy{}, relay.ErrNoKey
		}
		pol.Key = key
	} else {
		if pr.st.Account == "" || pr.st.Account == sendas.NoPersonLane {
			return relay.Policy{}, errWrongAccount
		}
		if u := t.user(cfg, pr.st.Account, tm, endpoint); u != "" {
			pol.User, pol.UserAccount = u, auth.AccountKey(cfg.AuthURL, u)
		}
	}
	chosen, err := profileHarnesses(t.cfg.Dir, cfg)
	if err != nil {
		return relay.Policy{}, err
	}
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
	return pol, nil
}

func profileHarnesses(dir string, cfg *config.Config) ([]string, error) {
	file, err := config.LoadFile(dir)
	if err != nil {
		return nil, err
	}
	if p := file.Profiles[cfg.ProfileName]; p != nil {
		return p.Harnesses, nil
	}
	if cfg.ProfileName != "" {
		return nil, errors.New("profile removed; restart the relay")
	}
	return cfg.Harnesses, nil
}

func (t *Teams) serverKey(dir, teamID string) (string, error) {
	t.keyMu.Lock()
	if t.deps.Now().Before(t.lockedUntil) {
		err := t.lockedErr
		t.keyMu.Unlock()
		return "", err
	}
	t.keyMu.Unlock()
	key, err := keystore.Get(dir, teamID)
	if err != nil && secret.IsUnavailable(err) {
		t.keyMu.Lock()
		t.lockedUntil, t.lockedErr = t.deps.Now().Add(keychainRetry), err
		t.keyMu.Unlock()
	}
	return key, err
}

func (t *Teams) user(cfg *config.Config, account string, tm routing.Team, endpoint string) string {
	if t.deps.User == nil {
		return ""
	}
	key := cfg.ProfileName + "\x00" + account + "\x00" + tm.OrganizationID + "\x00" + tm.AuthURL + "\x00" + endpoint
	now := t.deps.Now()
	t.userMu.Lock()
	defer t.userMu.Unlock()
	if u, ok := t.users[key]; ok && now.Sub(u.at) < userTTL {
		return u.id
	}
	id := t.deps.User(cfg, tm, endpoint)
	t.users[key] = cachedUser{id: id, at: now}
	return id
}

func held(d routing.Decision) (relay.Why, string) {
	switch d.Reason {
	case routing.ReasonBlocked:
		return relay.WhyBlocked, "team:" + d.EnrollmentID
	}
	return relay.WhyUnknown, ""
}

func location(rec relay.Record) routing.Location {
	if !rec.Placed {
		return routing.Location{Kind: routing.Unknown, Narrow: rec.Narrow || rec.Claimed}
	}
	c := rec.Placement
	origin := cmp.Or(c.Repository.Origin, c.Remote)
	switch c.Kind {
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
