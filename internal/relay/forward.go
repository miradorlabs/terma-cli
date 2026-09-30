package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// Upstream. A part that may leave is written to its route's outbox on disk (outbox.go)
// before the export that carried it is answered, so what the relay accepted for a
// claimed session survives the relay's crash, a restart and a gateway outage. Only
// claimed parts, with the project's content policy already applied, ever reach disk:
// what waits for a claim stays in memory (route.go). Each route — a project and the
// tool whose key it uses — has its own sender: its parts leave oldest first, a
// transient failure is retried with backoff, and one refusing host never holds up
// another project.

const (
	sendTimeout   = 15 * time.Second
	maxMergeBytes = 4 << 20
	maxMergeFiles = 64
	minBackoff    = time.Second
	maxBackoff    = 2 * time.Minute
	// maxRetryAfter bounds how long a gateway's Retry-After can park a route.
	maxRetryAfter = 10 * time.Minute
	// A refused key is not fixed by asking again soon: `terma install` stores a new one.
	refusedBackoff    = 5 * time.Minute
	maxRefusedBackoff = time.Hour
	// keylessRetry is how often a route whose key is gone looks for one again.
	keylessRetry = time.Minute
	idleWait     = 30 * time.Second
)

// sender delivers one route's outbox.
type sender struct {
	r     *Relay
	route route
	wake  chan struct{}

	mu      sync.Mutex
	pending int  // files queued and not yet delivered or set aside
	keyless bool // the route's key is gone: it waits, and keeps no relay awake
}

// sender returns rt's sender, starting it on first use.
func (r *Relay) sender(rt route) *sender {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.senders[rt]
	if s == nil {
		s = &sender{r: r, route: rt, wake: make(chan struct{}, 1)}
		r.senders[rt] = s
		r.wg.Add(1)
		go s.loop()
	}
	return s
}

// enqueue writes a part that may leave to its route's outbox and wakes the sender. It
// runs under deliverMu, so a route's files are named in the order its parts arrived.
func (r *Relay) enqueue(c claim.Claim, p *part) {
	rt := routeOf(c)
	body, err := proto.Marshal(p.msg)
	if err != nil {
		r.stats.dropped(p.signal, "encode_failed", p.records)
		return
	}
	e := newEntry(r.opts.Now(), p.signal, p.records)
	if err := r.outbox.put(rt, e, body); err != nil {
		r.stats.dropped(p.signal, "outbox_write_failed", p.records)
		if r.opts.Logf != nil {
			r.opts.Logf("outbox %s: %v", rt, err)
		}
		return
	}
	s := r.sender(rt)
	s.mu.Lock()
	s.pending++
	s.mu.Unlock()
	s.poke()
}

func (s *sender) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// busy reports whether the sender has something to deliver: a keyless route does not
// count, since only a new key — the next claim's relay — can move it.
func (s *sender) busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending > 0 && !s.keyless
}

func (s *sender) setKeyless(v bool) {
	s.mu.Lock()
	s.keyless = v
	s.mu.Unlock()
}

func (s *sender) delivered(n int) {
	s.mu.Lock()
	s.pending = max(0, s.pending-n)
	s.mu.Unlock()
}

// loop delivers the outbox oldest first until the relay stops; a stopping relay keeps
// delivering, within its grace, until the outbox is empty. What is left stays on disk
// for the next relay.
func (s *sender) loop() {
	defer s.r.wg.Done()
	ctx := s.r.sendCtx
	backoff := time.Duration(0)
	// single sends one file per request after a merged request was refused, so one bad
	// file is set aside without the ones merged with it.
	single := false
	stopping := func() bool {
		select {
		case <-s.r.stopping:
			return true
		default:
			return false
		}
	}
	// sleep waits d, or for new work when nothing failed; false once the relay is done.
	sleep := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		wake := s.wake
		if backoff > 0 {
			wake = nil // new work does not cut a backoff short: the host is the same
		}
		stop := s.r.stopping
		if stopping() {
			stop = nil
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		case <-wake:
		case <-stop:
			if backoff > 0 {
				return false // a failing host is not retried on the way out
			}
		}
		return true
	}
	for ctx.Err() == nil {
		entries, err := s.r.outbox.list(s.route)
		if err != nil || len(entries) == 0 {
			if stopping() {
				return
			}
			if !sleep(idleWait) {
				return
			}
			continue
		}
		pol, ok := s.r.resolve(claim.Claim{ProjectID: s.route.project, Tool: s.route.toolLabel()})
		if !ok {
			s.setKeyless(true)
			if stopping() || !sleep(keylessRetry) {
				return
			}
			continue
		}
		s.setKeyless(false)
		if single {
			entries = entries[:1]
		}
		batch, body, err := s.take(entries)
		if err != nil {
			if !sleep(minBackoff) {
				return
			}
			continue
		}
		// The project's content policy as it stands now, not as it stood when the part
		// was queued: a project that turned prompts off since sends none of the prompts
		// still waiting (they stay on disk until delivered, filtered).
		body = s.r.withholdQueued(batch[0].signal, body, pol)
		if pol.Signals != nil && !contains(pol.Signals, string(batch[0].signal)) || body == nil {
			for _, e := range batch {
				s.r.stats.dropped(e.signal, "policy_signal_or_content", e.records)
			}
			s.r.outbox.remove(s.route, batch)
			s.delivered(len(batch))
			continue
		}
		out, wait, detail, rejected := s.send(ctx, pol, batch[0].signal, body)
		switch out {
		case sent:
			records := 0
			for _, e := range batch {
				records += e.records
			}
			s.r.stats.forwarded(batch[0].signal, records)
			if rejected > 0 {
				// Accepted, but the gateway dropped some of it: sending again would only
				// duplicate what it kept, so it is counted, never retried.
				s.r.stats.add("upstream_rejected."+string(batch[0].signal), int(rejected))
			}
			s.r.outbox.remove(s.route, batch)
			s.delivered(len(batch))
			single, backoff = false, 0
		case refused:
			if len(batch) > 1 {
				single = true
				continue
			}
			single = false
			s.r.stats.dropped(batch[0].signal, "upstream_"+detail, batch[0].records)
			s.r.outbox.bury(s.route, batch[0])
			s.delivered(1)
		case retry:
			if ctx.Err() != nil {
				return
			}
			s.r.stats.add("upstream_retries", 1)
			if s.r.opts.Logf != nil {
				s.r.opts.Logf("upstream %s: %s", s.route, detail)
			}
			backoff = nextBackoff(backoff, wait)
			if !sleep(jitter(backoff, wait)) {
				return
			}
		}
	}
}

// take reads the oldest files of one signal that fit one request and merges them.
func (s *sender) take(entries []entry) ([]entry, []byte, error) {
	first := entries[0]
	body, err := s.r.outbox.read(s.route, first)
	if err != nil {
		// A file that cannot be read never will be; it must not hold the rest back.
		s.r.stats.dropped(first.signal, "outbox_unreadable", first.records)
		s.r.outbox.bury(s.route, first)
		s.delivered(1)
		return nil, nil, err
	}
	batch := []entry{first}
	bodies := [][]byte{body}
	size := len(body)
	for _, e := range entries[1:] {
		if len(batch) >= maxMergeFiles || e.signal != first.signal {
			break
		}
		b, err := s.r.outbox.read(s.route, e)
		if err != nil || size+len(b) > maxMergeBytes {
			break
		}
		batch = append(batch, e)
		bodies = append(bodies, b)
		size += len(b)
	}
	if len(batch) == 1 {
		return batch, body, nil
	}
	merged, err := mergeBodies(first.signal, bodies)
	if err != nil {
		return batch[:1], body, nil
	}
	return batch, merged, nil
}

type outcome int

const (
	sent outcome = iota
	retry
	refused
)

// send delivers one request and classifies the answer. A refused key (401, 403) and a
// missing endpoint (404) are retried, slowly: they are configuration, fixed on this
// machine, not a fault in the records. Only a body the host judges bad is refused for
// good. A 2xx can still carry OTLP's partial success, returned as rejected.
func (s *sender) send(ctx context.Context, pol Policy, sig Signal, body []byte) (outcome, time.Duration, string, int64) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(pol.Endpoint, "/")+"/v1/"+string(sig), bytes.NewReader(body))
	if err != nil {
		return refused, 0, "bad_endpoint", 0
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+pol.Key)
	req.Header.Set("User-Agent", "terma-relay/"+s.r.opts.Version)
	resp, err := s.r.opts.HTTP.Do(req)
	if err != nil {
		return retry, 0, err.Error(), 0
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch code := resp.StatusCode; {
	case code/100 == 2:
		return sent, 0, "", partialSuccess(answer)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return retry, refusedBackoff, fmt.Sprintf("HTTP %d: the project's key was refused", code), 0
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code == http.StatusNotFound || code >= 500:
		return retry, retryAfter(resp.Header.Get("Retry-After")), fmt.Sprintf("HTTP %d", code), 0
	default:
		return refused, 0, "refused_" + strconv.Itoa(code), 0
	}
}

// partialSuccess reads the records a host rejected out of an accepted export from its
// protobuf response: field 1 of every Export*ServiceResponse is partial_success, whose
// field 1 is the rejected count. Decoded by hand, since the collector packages that
// define those messages pull gRPC into every hook.
func partialSuccess(body []byte) int64 {
	ps := protoField(body, 1, protowire.BytesType)
	if ps == nil {
		return 0
	}
	for b := ps; len(b) > 0; {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return 0
		}
		b = b[n:]
		if num == 1 && typ == protowire.VarintType {
			v, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return 0
			}
			return max(0, int64(v))
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return 0
		}
		b = b[m:]
	}
	return 0
}

// protoField returns the bytes of the first length-delimited field num in b.
func protoField(b []byte, num protowire.Number, typ protowire.Type) []byte {
	for len(b) > 0 {
		n, t, l := protowire.ConsumeTag(b)
		if l < 0 {
			return nil
		}
		b = b[l:]
		if n == num && t == typ {
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return nil
			}
			return v
		}
		m := protowire.ConsumeFieldValue(n, t, b)
		if m < 0 {
			return nil
		}
		b = b[m:]
	}
	return nil
}

func retryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}
	return min(time.Duration(secs)*time.Second, maxRetryAfter)
}

// nextBackoff doubles the previous wait within its bounds, starting from floor when the
// answer asked for one.
func nextBackoff(prev, floor time.Duration) time.Duration {
	next := max(minBackoff, prev*2, floor)
	limit := maxBackoff
	if floor >= refusedBackoff {
		limit = maxRefusedBackoff
	}
	return min(next, max(limit, floor))
}

// jitter spreads a wait over ±20%, never below floor (a host's Retry-After). Without
// it, every relay that lost the gateway in one outage retries on the same beat and
// meets the recovering gateway all at once.
func jitter(d, floor time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := int64(d) * 2 / 5
	return max(floor, d-time.Duration(spread/2)+time.Duration(rand.Int64N(spread+1)))
}

// recoverOutbox starts a sender for every route that has files from an earlier relay, so
// what it accepted and could not deliver leaves now.
func (r *Relay) recoverOutbox() {
	routes, _ := r.outbox.routes()
	for _, rt := range routes {
		entries, err := r.outbox.list(rt)
		if err != nil || len(entries) == 0 {
			continue
		}
		n := 0
		for _, e := range entries {
			n += e.records
		}
		r.stats.add("recovered_from_outbox", n)
		s := r.sender(rt)
		s.mu.Lock()
		s.pending += len(entries)
		s.mu.Unlock()
		s.poke()
	}
}

// withholdQueued applies pol's content policy to a queued body before it is sent. A
// body that no longer decodes cannot be checked against a stricter policy and is dropped.
func (r *Relay) withholdQueued(sig Signal, body []byte, pol Policy) []byte {
	if len(pol.ExcludePaths) > 0 {
		pol.IncludePrompts, pol.IncludeToolContent = false, false
	}
	if pol.IncludePrompts && pol.IncludeToolContent && len(pol.ExcludePaths) == 0 && !pol.RequireClaim {
		return body
	}
	var msg proto.Message
	switch sig {
	case Logs:
		msg = &logspb.LogsData{}
	case Traces:
		msg = &tracepb.TracesData{}
	default:
		msg = &metricspb.MetricsData{}
	}
	if proto.Unmarshal(body, msg) != nil {
		return nil
	}
	if pol.RequireClaim && hasCatchAll(&part{signal: sig, msg: msg}) {
		return nil
	}
	if pathExcluded(&part{signal: sig, msg: msg}, pol.ExcludePaths) {
		return nil
	}
	unclassified := map[string]int{}
	n := withhold(&part{signal: sig, msg: msg}, pol.IncludePrompts, pol.IncludeToolContent, unclassified)
	if n == 0 && len(unclassified) == 0 {
		return body
	}
	r.stats.add("withheld_at_send_records", n)
	for key, c := range unclassified {
		r.stats.unclassified(key, c)
	}
	out, err := proto.Marshal(msg)
	if err != nil {
		return nil
	}
	return out
}
