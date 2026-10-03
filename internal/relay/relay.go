// Package relay is the local OTLP relay: agents' exporters send to it on loopback, and it
// forwards a record only when a hook in an opted-in repository claimed its session and the
// record came from a process that claim names, to that project with its key and content
// policy. Everything else waits briefly in memory, since a first export can race the
// claiming hook, and is then dropped without touching disk.
package relay

import (
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

// ProjectAttr is the resource attribute the relay stamps on everything it forwards: the claimed project.
const ProjectAttr = "mirador.project.id"

// DefaultHold is how long a part that cannot leave yet waits, covering a first export that races its hook.
const DefaultHold = 2 * time.Minute

// DefaultTraceHold is how long a span waits for its trace to be named: as long as a turn can last.
const DefaultTraceHold = 30 * time.Minute

// maxBody bounds one export request, decompressed.
const maxBody = 16 << 20

// Policy is how a claimed session's telemetry leaves the machine: where, with which key, and what content.
type Policy struct {
	Endpoint           string
	Key                string
	IncludePrompts     bool
	IncludeToolContent bool
	// Signals nil allows every signal; empty allows none.
	Signals []string
	// Excludes reports whether an attribute, in protojson's shape, names an excluded
	// file; nil when nothing is excluded.
	Excludes func(value any) bool
	// ExcludedWorkspace drops everything: the claimed workspace is an excluded path.
	ExcludedWorkspace bool
	RequireClaim      bool
}

// ErrNoKey is Resolve's answer for a project this machine holds no key for; its parts wait, then drop.
var ErrNoKey = errors.New("no key for this team on this machine")

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
	// Lookup finds a session's claim; claim.Read when nil.
	Lookup func(sessionID string, now time.Time) (claim.Claim, bool)
	// Resolve turns a claim into a Policy, or ErrNoKey.
	Resolve func(c claim.Claim) (Policy, error)
	// CatchAll, in global mode, is where a part whose hold ran out unplaced goes instead of being dropped.
	CatchAll func() (claim.Claim, bool)
	// HeartbeatSend, set, turns the heartbeat on; HeartbeatEvery is its period.
	HeartbeatSend  func(ctx context.Context, beat *logspb.LogsData) error
	HeartbeatEvery time.Duration
	// ClaimCacheTTL and PolicyCacheTTL keep Lookup's and Resolve's answers that long (0: ask every time).
	ClaimCacheTTL  time.Duration
	PolicyCacheTTL time.Duration
	// PeerPID names the process behind a connection from its remote port; without it the session alone decides.
	PeerPID func(port int) (int, bool)
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
	// Logf, when set, is told why each part that could not leave was dropped (TERMA_RELAY_DEBUG=1).
	Logf func(format string, args ...any)
	// Warnf, when set, is told what an operator should see whatever the debug setting: a
	// delivery the backend refused, a retry, an outbox that cannot be written, a failed
	// heartbeat. Logf is told too.
	Warnf func(format string, args ...any)
}

// Relay is the local OTLP relay: Handler accepts exports and Run delivers or ages them out.
type Relay struct {
	opts  Options
	rules *rules
	stats *Stats
	cache lookupCache

	// deliverMu keeps a session's parts in arrival order; taken before mu, never after.
	deliverMu sync.Mutex

	mu         sync.Mutex
	held       map[string][]heldPart
	heldN      int
	heldBytes  int
	traces     map[string]traceSession
	procs      map[int]*procState // sender pid → the sessions it named, and whether it exited
	outbox     outbox
	senders    map[route]*sender
	lastSeen   time.Time
	wg         sync.WaitGroup
	sendCtx    context.Context
	cancelSend context.CancelFunc
	stopping   chan struct{}
}

// New returns a relay. Run must be started for anything to leave.
func New(opts Options) *Relay {
	if opts.Hold == 0 {
		opts.Hold = DefaultHold
	}
	if opts.TraceHold == 0 {
		opts.TraceHold = max(DefaultTraceHold, opts.Hold)
	}
	if opts.Lookup == nil {
		opts.Lookup = claim.Read
	}
	if opts.Resolve == nil {
		opts.Resolve = func(claim.Claim) (Policy, error) { return Policy{}, ErrNoKey }
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if opts.HeartbeatEvery == 0 {
		opts.HeartbeatEvery = DefaultHeartbeatEvery
	}
	if opts.Grace == 0 {
		opts.Grace = 5 * time.Second
	}
	sendCtx, cancel := context.WithCancel(context.Background())
	return &Relay{
		opts: opts, rules: compose(opts.Correlators, opts.Capturers), stats: newStats(),
		cache:  lookupCache{claims: map[string]cachedClaim{}, policies: map[string]cachedPolicy{}},
		held:   map[string][]heldPart{},
		traces: map[string]traceSession{}, procs: map[int]*procState{}, senders: map[route]*sender{}, outbox: outbox{dir: opts.Dir},
		lastSeen: opts.Now(), sendCtx: sendCtx, cancelSend: cancel, stopping: make(chan struct{}),
	}
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
		if err := r.heartbeat(req.Context(), reason); err != nil {
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
	r.lastSeen = r.opts.Now()
	r.mu.Unlock()
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
	for _, p := range parts {
		p.pid = pid
		p.at = earliest(p.msg)
		r.stats.received(s, p.records)
		if p.session != "" && !strings.HasPrefix(p.session, tracePrefix) {
			r.learnProcess(pid, p.session)
			p.start = r.rules.conversationStart(p)
		}
	}
	for _, p := range parts {
		if p.session == "" {
			// A part naming no session may still go by its process once it exits (decideExited).
			if pid == 0 {
				if _, global := r.catchAll(); !global {
					r.stats.dropped(s, "no_session_id", p.records)
					continue
				}
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

func readBody(req *http.Request) ([]byte, error) {
	var rd io.Reader = http.MaxBytesReader(nil, req.Body, maxBody)
	if strings.EqualFold(req.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(rd)
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		rd = io.LimitReader(zr, maxBody+1)
	}
	body, err := io.ReadAll(rd)
	if err == nil && len(body) > maxBody {
		err = fmt.Errorf("export larger than %d bytes", maxBody)
	}
	return body, err
}

func (r *Relay) decode(s Signal, body []byte, isJSON bool) (map[string]*part, error) {
	unmarshal := proto.Unmarshal
	if isJSON {
		fixed, err := hexIDsToBase64(body)
		if err != nil {
			return nil, err
		}
		body = fixed
		unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal
	}
	switch s {
	case Logs:
		var m logspb.LogsData
		if err := unmarshal(body, &m); err != nil {
			return nil, err
		}
		return r.rules.splitLogs(&m, r.learnTrace), nil
	case Traces:
		var m tracepb.TracesData
		if err := unmarshal(body, &m); err != nil {
			return nil, err
		}
		return r.rules.splitTraces(&m, r.learnTrace, r.traceOf), nil
	default:
		var m metricspb.MetricsData
		if err := unmarshal(body, &m); err != nil {
			return nil, err
		}
		return r.rules.splitMetrics(&m), nil
	}
}
