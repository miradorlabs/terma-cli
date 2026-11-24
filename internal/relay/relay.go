// Package relay is the local OTLP relay: agents' exporters send to it on loopback, and it
// forwards a record only when a hook in a collected repository claimed its session and the
// record came from a process that claim names, to that project with its key and content
// policy. A session a hook marked not collected is dropped as it arrives. Everything else
// waits briefly in memory, since a first export can race the claiming hook, and is then
// dropped without touching disk; a stopping relay drops it too.
package relay

import (
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// DefaultHold is how long a part that cannot leave yet waits, covering a first export that races its hook.
const DefaultHold = 2 * time.Minute

// DefaultTraceHold is how long a span waits for its trace to be named: as long as a turn can last.
const DefaultTraceHold = 30 * time.Minute

// maxBody bounds one export request, decompressed.
const maxBody = 16 << 20

// Policy is how a claimed session's telemetry leaves the machine: where, with which
// credential, and what content.
type Policy struct {
	Endpoint string
	// Key is the team's server key a profile was set up with; "" sends with the person's
	// sign-in (Options.SignIn), naming the team in the request's X-Mirador-Project.
	Key                string
	IncludePrompts     bool
	IncludeToolContent bool
	// Signals nil allows every signal; empty allows none.
	Signals      []string
	RequireClaim bool
	// Unadmitted is a claim whose repository the team policy no longer lists: its records drop.
	Unadmitted bool
	// User is the Mirador user every part sent under this policy names, by id; "" names no one.
	User string
	// UserAccount is User's account key (auth.AccountKey): a part bound to another account
	// names no one, so the user it names is never another than the sign-in it leaves with.
	UserAccount string
	// Account is the account key of the sign-in the profile's team was chosen under when
	// this was resolved (config.Profile.Account); "" when unknown. A person's part in
	// another lane is never sent under it.
	Account string
}

// resolvedFor reports whether a part in lane may leave under pol: a server key's policy
// names its own credential; a person's leaves only under the account it was resolved for.
func (pol Policy) resolvedFor(lane string) bool {
	return pol.Key != "" || pol.Account == "" || !auth.ValidAccountKey(lane) || pol.Account == lane
}

// ErrNoKey is Resolve's answer for a project this machine holds no key for; its parts wait, then drop.
var ErrNoKey = errors.New("no key for this team on this machine")

// SignIn is the person's sign-in the relay sends with (sendas.SignIn).
type SignIn = sendas.SignIn

// ProjectHeader names the team a part sent with a user token goes to.
const ProjectHeader = sendas.ProjectHeader

// Options configure a relay.
type Options struct {
	// Correlators and Capturers say how each agent's records name their session and carry content.
	Correlators []shape.Correlator
	Capturers   []shape.Capturer
	// Token is what the agents' exporters present as `Authorization: Bearer <token>`.
	Token string
	// Hold and TraceHold: DefaultHold and DefaultTraceHold (at least Hold) when zero.
	Hold      time.Duration
	TraceHold time.Duration
	// Lookup finds a session's claim; none when nil.
	Lookup func(sessionID string, now time.Time) (claim.Claim, bool)
	// Resolve turns a claim into a Policy, or ErrNoKey.
	Resolve func(c claim.Claim) (Policy, error)
	// SignIn is who parts without a server key are sent as; none leave without it. A part
	// is bound to the lane of the sign-in when it arrives: a person's account, which it
	// leaves under only while that sign-in is the one in place, so a backlog never goes out
	// in another person's name or to another environment (it ages out of the outbox
	// meanwhile); or no person's, under a server key, which only a server key sends. One
	// that arrives signed out, or while the sign-in can't be read, is lost, and counted.
	// Nil is a server key's relay.
	SignIn SignIn
	// CatchAll, in global mode, is where a part whose hold ran out unplaced goes instead of being dropped.
	CatchAll func() (claim.Claim, bool)
	// Router routes records to selected teams when active (Router.Active()).
	Router Router
	// NudgeRefresh triggers a coalesced, non-blocking refresh on ingest 403 ORG_NOT_ACCESSIBLE.
	NudgeRefresh func(enrollment string)
	// HeartbeatSend, set, turns the heartbeat on; HeartbeatEvery is its period.
	HeartbeatSend  func(ctx context.Context, beat *logspb.LogsData) error
	HeartbeatEvery time.Duration
	// ClaimCacheTTL and PolicyCacheTTL keep Lookup's and Resolve's answers that long (0: ask every time).
	ClaimCacheTTL  time.Duration
	PolicyCacheTTL time.Duration
	// PeerPID names the process behind a connection from its two ends, the client's and the
	// relay's; without it the session alone decides.
	PeerPID func(client, server netip.AddrPort) (int, bool)
	// ProcessAlive reports whether a sender still runs; sessionless parts go by process once it exits.
	ProcessAlive func(pid int) bool
	// HTTP sends upstream; a no-redirect client with a 15-second timeout when nil.
	HTTP *http.Client
	// Dir is the on-disk outbox, so a restart loses nothing; required for anything to leave.
	Dir string
	// Grace is how long a stopping relay keeps delivering (5 s); the rest waits in the outbox.
	Grace time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Version is terma's, sent as the User-Agent.
	Version string
	// Launch is what started the relay, on the heartbeat's resource: the service manager,
	// a hook on demand, or a developer. A string; the daemon owns the vocabulary.
	Launch string
	// InstallMethod is how this terma was installed, on the heartbeat's resource as
	// terma.install.method; "" sends none.
	InstallMethod string
	// Installation is, asked at every heartbeat, this installation's stable id, on its
	// resource as terma.installation.id; nil or "" sends none.
	Installation func() string
	// HookLosses are, asked at every heartbeat, what hooks lost since it was last asked, by
	// reason (spool.TakeNewLosses): counted as dropped.<reason>.hooks, so the beat carries
	// the hooks' losses with the relay's.
	HookLosses func() map[string]int
	// UpdateAuto, set, is asked at every heartbeat whether this terma installs new releases
	// by itself, sent on its record as terma.update.auto: the developer can turn automatic
	// updates on or off while the relay runs.
	UpdateAuto func() string
	// Logf, when set, is told why each part that could not leave was dropped (TERMA_RELAY_DEBUG=1).
	Logf func(format string, args ...any)
	// Warnf, when set, is told what an operator should see whatever the debug setting: a
	// delivery the backend refused, a retry, an outbox that cannot be written, a failed
	// heartbeat. Logf is told too.
	Warnf func(format string, args ...any)
}

// Relay is the local OTLP relay: Handler accepts exports and Run delivers or ages them out.
type Relay struct {
	opts Options
	// hookLossesMu serializes Options.HookLosses.
	hookLossesMu sync.Mutex
	rules        *rules
	stats        *Stats
	cache        lookupCache

	// deliverMu keeps a session's parts in arrival order; taken before mu, never after.
	deliverMu sync.Mutex

	mu    sync.Mutex
	held  map[string][]heldPart
	heldN int
	// policyLost counts the parts waiting for their team's policy that a full hold refused
	// or evicted since the last sweep.
	policyLost int
	heldBytes  int
	traces     map[string]traceSession
	procs      map[int]*procState // sender pid → the sessions it named, and whether it exited
	outbox     outbox
	senders    map[route]*sender
	lastSeen   time.Time
	teamState
	// inflight counts the exports being read and routed; refusing, set by Quiesce, answers
	// each new one 503 so its exporter sends it again, to the next relay.
	inflight   int
	refusing   bool
	wg         sync.WaitGroup
	sendCtx    context.Context
	cancelSend context.CancelFunc
	stopping   chan struct{}
	// lossMu orders the writes of what the relay lost (recordLosses).
	lossMu sync.Mutex
	// unreadableRetry is how long an export waits for a sign-in that can't be read, and
	// unreadableAt, under mu, when the last such wait failed; lanes the lane read last.
	unreadableRetry time.Duration
	unreadableAt    time.Time
	lanes           laneState
}

// New returns a relay. Run must be started for anything to leave.
func New(opts Options) *Relay {
	opts.Hold = cmp.Or(opts.Hold, DefaultHold)
	opts.TraceHold = cmp.Or(opts.TraceHold, max(DefaultTraceHold, opts.Hold))
	if opts.Lookup == nil {
		opts.Lookup = func(string, time.Time) (claim.Claim, bool) { return claim.Claim{}, false }
	}
	if opts.Resolve == nil {
		opts.Resolve = func(claim.Claim) (Policy, error) { return Policy{}, ErrNoKey }
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{
			// A transport of its own: closing http.DefaultTransport's idle connections, as
			// httptest.Server.Close does, would otherwise break a delivery in flight.
			Transport:     http.DefaultTransport.(*http.Transport).Clone(),
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	opts.HeartbeatEvery = cmp.Or(opts.HeartbeatEvery, DefaultHeartbeatEvery)
	opts.Grace = cmp.Or(opts.Grace, 5*time.Second)
	sendCtx, cancel := context.WithCancel(context.Background())
	r := &Relay{
		opts:            opts,
		rules:           compose(opts.Correlators, opts.Capturers),
		stats:           newStats(),
		cache:           lookupCache{claims: map[string]cachedClaim{}, policies: map[string]cachedPolicy{}},
		held:            map[string][]heldPart{},
		traces:          map[string]traceSession{},
		procs:           map[int]*procState{},
		senders:         map[route]*sender{},
		outbox:          outbox{dir: opts.Dir},
		lastSeen:        opts.Now(),
		sendCtx:         sendCtx,
		cancelSend:      cancel,
		stopping:        make(chan struct{}),
		unreadableRetry: unreadableRetry,
		teamState:       newTeamState(),
	}
	r.seedLane()
	return r
}

// Stats is the relay's running account.
func (r *Relay) Stats() *Stats { return r.stats }

func (r *Relay) warnf(format string, args ...any) {
	if r.opts.Warnf != nil {
		r.opts.Warnf(format, args...)
	}
	if r.opts.Logf != nil {
		r.opts.Logf(format, args...)
	}
}

// Handler serves OTLP over HTTP on /v1/{logs,metrics,traces} and the stats on GET /stats.
//
// The token may also lead the path (/<token>/v1/logs) for an exporter whose config file has
// no headers setting: an environment header would hand the token to every tool the agent runs.
func (r *Relay) Handler() http.Handler {
	mux := r.routes()
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if rest, ok := r.pathToken(req.URL.Path); ok {
			req = req.Clone(req.Context())
			req.URL.Path = rest
			req.URL.RawPath = ""
			req.Header.Set("Authorization", "Bearer "+r.opts.Token)
		}
		mux.ServeHTTP(w, req)
	})
}

func (r *Relay) pathToken(path string) (string, bool) {
	if r.opts.Token == "" {
		return "", false
	}
	first, rest, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if !ok || subtle.ConstantTimeCompare([]byte(first), []byte(r.opts.Token)) != 1 {
		return "", false
	}
	return "/" + rest, true
}

func (r *Relay) routes() *http.ServeMux {
	mux := http.NewServeMux()
	for _, s := range []Signal{Logs, Metrics, Traces} {
		mux.HandleFunc("/v1/"+string(s), func(w http.ResponseWriter, req *http.Request) { r.export(w, req, s) })
	}
	mux.HandleFunc("/stats", func(w http.ResponseWriter, req *http.Request) {
		if !r.authorized(req) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.stats.Snapshot())
	})
	// `terma setup` asks for a beat as it finishes, proving the relay, credential and endpoint work.
	mux.HandleFunc("/heartbeat", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if !r.authorized(req) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		reason := req.URL.Query().Get("reason")
		if reason == "" || len(reason) > 32 {
			reason = "request"
		}
		w.Header().Set("Content-Type", "application/json")
		if err := r.heartbeat(req.Context(), reason, ""); err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_, _ = w.Write([]byte("{}"))
	})
	return mux
}

func (r *Relay) authorized(req *http.Request) bool {
	got, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	return ok && r.opts.Token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(r.opts.Token)) == 1
}

func (r *Relay) export(w http.ResponseWriter, req *http.Request, s Signal) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !r.authorized(req) {
		r.stats.add("refused_unauthorized", 1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.mu.Lock()
	refusing := r.refusing
	if !refusing {
		r.inflight++
		r.lastSeen = r.opts.Now()
	}
	r.mu.Unlock()
	if refusing {
		r.stats.add("refused_restarting", 1)
		w.Header().Set("Retry-After", "5")
		http.Error(w, "the relay is restarting; send this again", http.StatusServiceUnavailable)
		return
	}
	defer func() {
		r.mu.Lock()
		r.inflight--
		r.mu.Unlock()
	}()
	// The sign-in the export arrived under, before its body is read: a switch while it
	// is read or decoded does not hand what was collected under one account to another.
	lane, lost := r.arrivalLane(req.Context())
	arrived := r.opts.Now()
	body, err := readBody(req)
	if err != nil {
		r.stats.add("refused_unreadable", 1)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	isJSON := strings.Contains(req.Header.Get("Content-Type"), "json")
	parts, err := r.decode(s, body, isJSON)
	if err != nil {
		r.stats.add("refused_undecodable", 1)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pid := r.senderPID(req)
	narrow := (r.teams() || r.opts.CatchAll != nil) && !r.takesUnplaced()
	for _, p := range parts {
		p.pid, p.narrow, p.account, p.lost, p.arrived = pid, narrow, lane, lost, arrived
		p.at = earliest(p.msg)
		p.startAt, p.endAt = intervalOf(p.msg)
		r.stats.received(s, p.records)
		if p.session != "" && !strings.HasPrefix(p.session, tracePrefix) {
			r.learnProcess(pid, p.session)
			p.start = r.rules.conversationStart(p)
		}
	}
	for _, p := range parts {
		if p.session == "" {
			// A part naming no session may still go by its process once it exits (decideExited).
			if pid == 0 && !r.takesUnplaced() {
				r.stats.dropped(s, "no_session_id", p.records)
				continue
			}
			p.session = procKey(pid)
		}
		r.route(p)
	}
	// An empty success response is valid OTLP in either encoding.
	if isJSON {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}
