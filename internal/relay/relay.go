package relay

import (
	"bytes"
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

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// ProjectAttr is the resource attribute the relay stamps on everything it forwards:
// the project the claim named. The shared gateway already reads it on Codex's and
// OpenCode's exports.
const ProjectAttr = "mirador.project.id"

// DefaultHold is how long a record whose session nobody has claimed yet is kept in
// memory before it is dropped. It covers the first export batch racing the hook that
// claims the session; the spike measures the real race.
const DefaultHold = 2 * time.Minute

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
// developer never opted in here, so the session is dropped, not held.
var ErrNoKey = errors.New("no key for this project on this machine")

// Options configure a relay.
type Options struct {
	// Token is what the agents' exporters present as `Authorization: Bearer <token>`.
	Token string
	// Hold is how long unclaimed records wait for a claim (DefaultHold when zero).
	Hold time.Duration
	// Lookup finds a session's claim; claim.Read when nil.
	Lookup func(sessionID string, now time.Time) (claim.Claim, bool)
	// Resolve turns a claim into a Policy, or ErrNoKey.
	Resolve func(c claim.Claim) (Policy, error)
	// HTTP sends upstream; a no-redirect client with a 15-second timeout when nil.
	HTTP *http.Client
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Version is terma's, sent as the User-Agent.
	Version string
}

// Relay is the local OTLP relay. Handler serves the agents; Run delivers what was
// accepted and ages out what was never claimed.
type Relay struct {
	opts  Options
	stats *Stats

	mu       sync.Mutex
	held     map[string][]heldPart
	heldN    int
	lastSeen time.Time
	dests    map[string]*destination
	traces   map[string]traceSession
	wg       sync.WaitGroup
	ctx      context.Context
}

// traceSession is the session a trace was seen to belong to, and when.
type traceSession struct {
	session string
	at      time.Time
}

// traceTTL is how long the relay remembers which session a trace belongs to. A turn's
// spans arrive within seconds of each other; an hour is generous.
const traceTTL = time.Hour

type heldPart struct {
	p  *part
	at time.Time
}

// maxHeld bounds how many records wait for claims at once. Past it a new unclaimed
// record is dropped on arrival, counted as unclaimed_overflow.
const maxHeld = 50000

// New returns a relay. Run must be started before requests are served.
func New(opts Options) *Relay {
	if opts.Hold == 0 {
		opts.Hold = DefaultHold
	}
	if opts.Lookup == nil {
		opts.Lookup = claim.Read
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
	return &Relay{opts: opts, stats: newStats(), held: map[string][]heldPart{}, dests: map[string]*destination{}, traces: map[string]traceSession{}, lastSeen: opts.Now(), ctx: context.Background()}
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
	r.touch()
	body, err := readBody(req)
	if err != nil {
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
	for _, p := range parts {
		r.stats.received(s, p.records)
		if p.session == "" {
			r.stats.dropped(s, "no_session_id", p.records)
			continue
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
		unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal
	}
	switch s {
	case Logs:
		var m logspb.LogsData
		if err := unmarshal(body, &m); err != nil {
			return nil, err
		}
		return splitLogs(&m), nil
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

func (r *Relay) learnTrace(traceID, session string) {
	r.mu.Lock()
	r.traces[traceID] = traceSession{session, r.opts.Now()}
	r.mu.Unlock()
}

func (r *Relay) traceOf(traceID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.traces[traceID].session
}

// sessionFor resolves a held key to a session: itself, or for a trace key, the
// session its trace has since been seen to belong to ("" while unknown).
func (r *Relay) sessionFor(key string) string {
	if id, ok := strings.CutPrefix(key, tracePrefix); ok {
		return r.traceOf(id)
	}
	return key
}

func (r *Relay) touch() {
	r.mu.Lock()
	r.lastSeen = r.opts.Now()
	r.mu.Unlock()
}

// route forwards a session's part if its session is claimed, and holds it otherwise.
func (r *Relay) route(p *part) {
	if s := r.sessionFor(p.session); s != "" {
		if c, ok := r.opts.Lookup(s, r.opts.Now()); ok {
			r.deliver(c, p)
			return
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.heldN+p.records > maxHeld {
		r.stats.dropped(p.signal, "unclaimed_overflow", p.records)
		return
	}
	r.held[p.session] = append(r.held[p.session], heldPart{p, r.opts.Now()})
	r.heldN += p.records
	r.stats.add("held_parts", 1)
}

func (r *Relay) deliver(c claim.Claim, p *part) {
	pol, err := r.opts.Resolve(c)
	if err != nil || pol.Endpoint == "" || pol.Key == "" {
		r.stats.dropped(p.signal, "no_key", p.records)
		return
	}
	if n := withhold(p, pol.IncludePrompts, pol.IncludeToolContent); n > 0 {
		r.stats.add("withheld_content_records", n)
	}
	stamp(p, c.ProjectID)
	r.destination(pol).enqueue(p)
}

// stamp names the claimed project on every resource in the part, replacing any value
// already there: the claim decides.
func stamp(p *part, projectID string) {
	set := func(res *resourcepb.Resource) {
		for _, kv := range res.Attributes {
			if kv.GetKey() == ProjectAttr {
				kv.Value = strValue(projectID)
				return
			}
		}
		res.Attributes = append(res.Attributes, &commonpb.KeyValue{Key: ProjectAttr, Value: strValue(projectID)})
	}
	switch m := p.msg.(type) {
	case *logspb.LogsData:
		for _, rl := range m.GetResourceLogs() {
			set(rl.Resource)
		}
	case *tracepb.TracesData:
		for _, rs := range m.GetResourceSpans() {
			set(rs.Resource)
		}
	case *metricspb.MetricsData:
		for _, rm := range m.GetResourceMetrics() {
			set(rm.Resource)
		}
	}
}

func strValue(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

// Run releases held records as their sessions are claimed, drops those that outlive
// the hold, and delivers until ctx is done. It returns when ctx is cancelled and every
// destination has stopped; what was still queued is counted as lost.
func (r *Relay) Run(ctx context.Context) {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			for s, parts := range r.held {
				for _, h := range parts {
					reason := "unclaimed_at_exit"
					if strings.HasPrefix(s, tracePrefix) {
						reason = "no_session_trace_at_exit"
					}
					r.stats.dropped(h.p.signal, reason, h.p.records)
				}
				delete(r.held, s)
			}
			r.heldN = 0
			r.mu.Unlock()
			r.wg.Wait()
			return
		case <-tick.C:
			r.sweep()
		}
	}
}

func (r *Relay) sweep() {
	now := r.opts.Now()
	r.mu.Lock()
	sessions := make([]string, 0, len(r.held))
	for s := range r.held {
		sessions = append(sessions, s)
	}
	r.mu.Unlock()
	r.mu.Lock()
	for id, t := range r.traces {
		if now.Sub(t.at) >= traceTTL {
			delete(r.traces, id)
		}
	}
	r.mu.Unlock()
	for _, s := range sessions {
		var c claim.Claim
		claimed := false
		if session := r.sessionFor(s); session != "" {
			c, claimed = r.opts.Lookup(session, now)
		}
		r.mu.Lock()
		parts := r.held[s]
		var keep []heldPart
		var release []*part
		for _, h := range parts {
			switch {
			case claimed:
				release = append(release, h.p)
			case now.Sub(h.at) >= r.opts.Hold && strings.HasPrefix(s, tracePrefix):
				// No span ever named this trace's session: process-level work, not an
				// unclaimed session.
				r.stats.dropped(h.p.signal, "no_session_trace", h.p.records)
			case now.Sub(h.at) >= r.opts.Hold:
				r.stats.dropped(h.p.signal, "unclaimed_expired", h.p.records)
			default:
				keep = append(keep, h)
			}
			if claimed || now.Sub(h.at) >= r.opts.Hold {
				r.heldN -= h.p.records
			}
		}
		if len(keep) == 0 {
			delete(r.held, s)
		} else {
			r.held[s] = keep
		}
		r.mu.Unlock()
		for _, p := range release {
			r.stats.add("released_after_hold", p.records)
			r.deliver(c, p)
		}
	}
}

// Idle reports how long the relay has had no export, if it holds and queues nothing.
func (r *Relay) Idle() (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.heldN > 0 {
		return 0, false
	}
	for _, d := range r.dests {
		if d.busy() {
			return 0, false
		}
	}
	return r.opts.Now().Sub(r.lastSeen), true
}

// --- upstream ------------------------------------------------------------------------

// destination is one project's upstream: a queue drained by its own goroutine, which
// retries with backoff so one refusing host never holds up another project.
type destination struct {
	r      *Relay
	pol    Policy
	queue  chan *part
	mu     sync.Mutex
	active bool
}

const destQueue = 512

func (r *Relay) destination(pol Policy) *destination {
	key := pol.Endpoint + "\x00" + pol.Key
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.dests[key]
	if d == nil {
		d = &destination{r: r, pol: pol, queue: make(chan *part, destQueue)}
		r.dests[key] = d
		r.wg.Add(1)
		go d.drain(r.ctx)
	}
	return d
}

func (d *destination) enqueue(p *part) {
	select {
	case d.queue <- p:
	default:
		d.r.stats.dropped(p.signal, "upstream_queue_full", p.records)
	}
}

func (d *destination) busy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active || len(d.queue) > 0
}

func (d *destination) drain(ctx context.Context) {
	defer d.r.wg.Done()
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case p := <-d.queue:
					d.r.stats.dropped(p.signal, "upstream_lost_at_exit", p.records)
				default:
					return
				}
			}
		case p := <-d.queue:
			d.mu.Lock()
			d.active = true
			d.mu.Unlock()
			d.send(ctx, p)
			d.mu.Lock()
			d.active = false
			d.mu.Unlock()
		}
	}
}

// send delivers one part, retrying a transient failure (network, 429, 5xx) with
// backoff from one second to a minute. A refusal (any other 4xx) is final: the host
// will not take it however often it is asked.
func (d *destination) send(ctx context.Context, p *part) {
	body, err := proto.Marshal(p.msg)
	if err != nil {
		d.r.stats.dropped(p.signal, "encode_failed", p.records)
		return
	}
	backoff := time.Second
	for {
		status, err := d.post(ctx, p.signal, body)
		switch {
		case err == nil && status/100 == 2:
			d.r.stats.forwarded(p.signal, p.records)
			return
		case err == nil && status != http.StatusTooManyRequests && status/100 == 4:
			d.r.stats.dropped(p.signal, fmt.Sprintf("upstream_refused_%d", status), p.records)
			return
		}
		d.r.stats.add("upstream_retries", 1)
		select {
		case <-ctx.Done():
			d.r.stats.dropped(p.signal, "upstream_lost_at_exit", p.records)
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (d *destination) post(ctx context.Context, s Signal, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(d.pol.Endpoint, "/")+"/v1/"+string(s), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+d.pol.Key)
	req.Header.Set("User-Agent", "terma-relay/"+d.r.opts.Version)
	resp, err := d.r.opts.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
