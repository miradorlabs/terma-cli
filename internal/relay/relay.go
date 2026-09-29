// Package relay is the local OTLP relay. The agents' global exporters send to it on
// loopback; it forwards a record only when a hook in an opted-in repository claimed
// the record's session (package claim) and the record came from a process that claim
// names, to that repository's project, with that project's key and under its content
// policy. Everything else waits briefly in memory — a first export can race the hook
// that claims it — and is then dropped, without leaving the machine or touching disk.
//
// The files: relay.go takes requests and decodes them, split.go divides an export by
// session, route.go decides and holds, forward.go sends upstream, content.go applies
// the content policy, conn.go names senders, stats.go counts every record's fate.
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
)

// ProjectAttr is the resource attribute the relay stamps on everything it forwards:
// the project the claim named. The shared gateway already reads it on Codex's and
// OpenCode's exports.
const ProjectAttr = "mirador.project.id"

// DefaultHold is how long a part that cannot leave yet — unclaimed, uncovered, keyless
// — waits for its reason to go away. It covers a first export racing the hook.
const DefaultHold = 2 * time.Minute

// DefaultTraceHold is how long a span waits for its trace to be named: as long as a
// turn can last, since a turn's child spans can precede the span naming the session.
const DefaultTraceHold = 30 * time.Minute

// maxBody bounds one export request, decompressed.
const maxBody = 16 << 20

// Policy is how a claimed session's telemetry leaves the machine: where, with which
// key, and what content the project's routing record lets through.
type Policy struct {
	Endpoint           string
	Key                string
	IncludePrompts     bool
	IncludeToolContent bool
}

// ErrNoKey is Resolve's answer for a project this machine holds no key for: the
// developer has not opted in here, so the session's parts wait, then are dropped.
var ErrNoKey = errors.New("no key for this project on this machine")

// Options configure a relay.
type Options struct {
	// Token is what the agents' exporters present as `Authorization: Bearer <token>`.
	Token string
	// Hold and TraceHold: DefaultHold and DefaultTraceHold (at least Hold) when zero.
	Hold      time.Duration
	TraceHold time.Duration
	// Lookup finds a session's claim; claim.Read when nil.
	Lookup func(sessionID string, now time.Time) (claim.Claim, bool)
	// Resolve turns a claim into a Policy, or ErrNoKey.
	Resolve func(c claim.Claim) (Policy, error)
	// ClaimCacheTTL and PolicyCacheTTL keep Lookup's and Resolve's answers that long
	// (0: ask every time). Production uses about a second and a few seconds.
	ClaimCacheTTL  time.Duration
	PolicyCacheTTL time.Duration
	// PeerPID names the process behind a connection from its remote port
	// (procinfo.FindSender). Without it the session alone decides.
	PeerPID func(port int) (int, bool)
	// HTTP sends upstream; a no-redirect client with a 15-second timeout when nil.
	HTTP *http.Client
	// Grace is how long a stopping relay keeps delivering what it accepted (5 s).
	Grace time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Version is terma's, sent as the User-Agent.
	Version string
	// Logf, when set, is told why each part that could not leave was dropped: its key,
	// its sender and the claim's processes. `terma relay run` sets it with
	// TERMA_RELAY_DEBUG=1.
	Logf func(format string, args ...any)
}

// Relay is the local OTLP relay. Handler serves the agents; Run delivers what was
// accepted and ages out what never may be.
type Relay struct {
	opts  Options
	stats *Stats
	cache lookupCache

	// deliverMu orders every hand-off to a destination and every hold, so a session's
	// parts leave in arrival order. Taken before mu, never after.
	deliverMu sync.Mutex

	mu         sync.Mutex
	held       map[string][]heldPart
	heldN      int
	heldBytes  int
	traces     map[string]traceSession
	procs      map[int]map[string]time.Time // sender pid → sessions it exported, and when
	origins    map[int]map[string]bool      // sender pid → every client it served (Codex's originator)
	internal   map[string]time.Time         // sessions whose start says Codex made them for itself
	dests      map[string]*destination
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
	if opts.Grace == 0 {
		opts.Grace = 5 * time.Second
	}
	sendCtx, cancel := context.WithCancel(context.Background())
	return &Relay{
		opts: opts, stats: newStats(),
		cache:  lookupCache{claims: map[string]cachedClaim{}, policies: map[string]cachedPolicy{}},
		held:   map[string][]heldPart{},
		traces: map[string]traceSession{}, procs: map[int]map[string]time.Time{}, origins: map[int]map[string]bool{}, internal: map[string]time.Time{}, dests: map[string]*destination{},
		lastSeen: opts.Now(), sendCtx: sendCtx, cancelSend: cancel, stopping: make(chan struct{}),
	}
}

// Stats is the relay's running account.
func (r *Relay) Stats() *Stats { return r.stats }

// Handler serves OTLP over HTTP (protobuf or JSON, optionally gzipped) on
// /v1/{logs,metrics,traces}, and the stats on GET /stats.
func (r *Relay) Handler() http.Handler {
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
		r.stats.received(s, p.records)
		if p.session != "" && !strings.HasPrefix(p.session, tracePrefix) {
			r.learnProcess(pid, p.session, originatorOf(p))
			if internalStart(p) {
				r.mu.Lock()
				r.internal[p.session] = r.opts.Now()
				r.mu.Unlock()
			}
		}
	}
	for _, p := range parts {
		if p.session == "" {
			// Nothing in the record names a session — Codex's metrics. Its process
			// might: see decideProcess.
			if pid == 0 {
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
		return splitLogs(&m, r.learnTrace), nil
	case Traces:
		var m tracepb.TracesData
		if err := unmarshal(body, &m); err != nil {
			return nil, err
		}
		return splitTraces(&m, r.learnTrace, r.traceOf), nil
	default:
		var m metricspb.MetricsData
		if err := unmarshal(body, &m); err != nil {
			return nil, err
		}
		return splitMetrics(&m), nil
	}
}
