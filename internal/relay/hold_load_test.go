package relay

import (
	"bytes"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// The held-store load replays what the store was weighed against, one simulated second at
// a time: that second's exports, then the sweep and the store's flush the relay's ticker
// runs. TERMA_HELD_LOAD=1 runs it and logs what each load cost.

type loadRun struct {
	t       *testing.T
	r       *Relay
	f       *fixture
	h       http.Handler
	latency []time.Duration
	exports int
}

func (l *loadRun) post(path string, m proto.Message) {
	body, err := proto.Marshal(m)
	if err != nil {
		l.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()
	start := time.Now()
	l.h.ServeHTTP(w, req)
	l.latency = append(l.latency, time.Since(start))
	l.exports++
	if w.Code != http.StatusOK {
		l.t.Fatalf("%s = %d", path, w.Code)
	}
}

func loadAttrs(pairs ...string) []*commonpb.KeyValue {
	var out []*commonpb.KeyValue
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, kv(pairs[i], pairs[i+1]))
	}
	return out
}

// codexSpans are a Codex startup's session-less spans: fs and auth work, each of a turn's trace.
func codexSpans(sec, n int, traces ...[]byte) *tracepb.TracesData {
	var spans []*tracepb.Span
	for i := range n {
		id := make([]byte, 8)
		copy(id, fmt.Sprintf("%04d%04d", sec, i))
		start := uint64(1_800_000_000_000_000_000 + sec*1_000_000_000 + i*1000)
		spans = append(spans, &tracepb.Span{TraceId: traces[i%len(traces)], SpanId: id, Name: []string{"fs.get_metadata", "fs.read_file", "auth.refresh"}[i%3],
			StartTimeUnixNano: start, EndTimeUnixNano: start + 250_000, Kind: tracepb.Span_SPAN_KIND_INTERNAL,
			Attributes: loadAttrs("code.filepath", "codex-rs/core/src/exec.rs", "code.lineno", "412", "code.namespace", "codex_core::fs",
				"thread.id", "7", "thread.name", "tokio-runtime-worker", "busy_ns", "182340", "idle_ns", "9120",
				"path", fmt.Sprintf("/Users/dev/work/app/src/module_%d/file_%d.rs", i%40, i))})
	}
	return &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: loadAttrs("service.name", "codex_cli_rs", "service.version", "0.48.0", "host.name", "laptop")},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}}
}

// agentLogs are an agent's events for session: API requests, with a prompt every tenth export.
func agentLogs(key, session string, n int, prompt bool) *logspb.LogsData {
	var recs []*logspb.LogRecord
	for i := range n {
		recs = append(recs, &logspb.LogRecord{TimeUnixNano: uint64(1_800_000_000_000_000_000 + i), Attributes: loadAttrs(key, session,
			"event.name", "claude_code.api_request", "model", "claude-opus-5-5", "cost_usd", "0.0123", "duration_ms", "2310",
			"input_tokens", "18234", "output_tokens", "612", "cache_read_tokens", "17000", "prompt.id", "5f2e9f2b-444e-46fd-aab7",
			"user.id", "8d1c0a7e6b", "organization.id", "0e5b2b0d", "terminal.type", "iTerm.app")})
	}
	if prompt {
		recs = append(recs, &logspb.LogRecord{Attributes: loadAttrs(key, session, "event.name", "claude_code.user_prompt",
			"prompt", strings.Repeat("Refactor the relay hold so restarts keep records. ", 40), "prompt_length", "2000")})
	}
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: loadAttrs("service.name", "claude-code", "service.version", "2.1.280", "host.name", "laptop")},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}}}}}
}

type loadScenario struct {
	name    string
	seconds int
	step    func(l *loadRun, sec int)
}

var loadScenarios = []loadScenario{
	// (a) ~2,000 session-less spans and their naming logs over 20 s, then the claim.
	{"codex-startup-burst", 30, func(l *loadRun, sec int) {
		if sec < 20 {
			a, b := []byte(fmt.Sprintf("trace-%010d-a", sec)), []byte(fmt.Sprintf("trace-%010d-b", sec))
			l.post("/v1/traces", codexSpans(sec, 100, a, b))
			for _, tr := range [][]byte{a, b} {
				lg := agentLogs("conversation.id", "N", 1, false)
				lg.ResourceLogs[0].ScopeLogs[0].LogRecords[0].TraceId = tr
				l.post("/v1/logs", lg)
			}
		}
		if sec == 20 {
			l.f.claim("N", claim.Claim{ProjectID: "p1", Tool: "codex"})
		}
	}},
	// (b) a claimed Claude session: three exports a second for a minute.
	{"claude-steady-claimed", 60, func(l *loadRun, sec int) {
		for i := range 3 {
			l.post("/v1/logs", agentLogs("session.id", "A", 3, i == 0 && sec%10 == 0))
		}
	}},
	// (c) an unclaimed personal session, the same load, held until it expires.
	{"personal-unclaimed-expiry", 60 + 150, func(l *loadRun, sec int) {
		if sec < 60 {
			for i := range 3 {
				l.post("/v1/logs", agentLogs("session.id", "P", 3, i == 0 && sec%10 == 0))
			}
		}
	}},
}

type loadResult struct {
	exports                  int
	p50, p99                 time.Duration
	cpu                      time.Duration
	allocs, allocBytes       uint64
	heldWrites, heldRemoves  int
	heldBytes                int
	heldLeft, heldLeftBytes  int
	outboxFiles, outboxBytes int
	forwarded, dropped       int
}

func dirFiles(dir string, keep func(string) bool) (n, size int) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".tmp-") || !keep(p) {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n++
			size += int(info.Size())
		}
		return nil
	})
	return n, size
}

func runLoad(t *testing.T, sc loadScenario) loadResult {
	u := newUpstream(t)
	u.status = http.StatusServiceUnavailable // parts stay in the outbox, to be counted
	f := newFixture()
	f.claims["A"] = claim.Claim{ProjectID: "p1", Tool: "claude-code"}
	dir := t.TempDir()
	r := newRelay(Options{Dir: dir, Token: token, Lookup: f.lookup, Now: f.clock,
		Resolve: func(c claim.Claim) (Policy, error) { return allPolicies(u)[c.ProjectID], nil }})
	t.Cleanup(r.cancelSend)
	l := &loadRun{t: t, r: r, f: f, h: r.Handler()}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	cpu := cpuTime()
	for sec := range sc.seconds {
		sc.step(l, sec)
		r.sweep()
		heldTick(r)
		f.advance(time.Second)
	}
	res := loadResult{cpu: cpuTime() - cpu, exports: l.exports}
	runtime.ReadMemStats(&after)
	res.allocs, res.allocBytes = after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc
	slices.Sort(l.latency)
	if n := len(l.latency); n > 0 {
		res.p50, res.p99 = l.latency[n/2], l.latency[min(n-1, n*99/100)]
	}
	c := r.Stats().Snapshot().Counters
	res.heldWrites, res.heldRemoves, res.heldBytes = c["held_store_files_written"], c["held_store_files_removed"], c["held_store_bytes_written"]
	held := func(p string) bool { return strings.Contains(p, string(filepath.Separator)+".held") }
	res.heldLeft, res.heldLeftBytes = dirFiles(dir, held)
	res.outboxFiles, res.outboxBytes = dirFiles(dir, func(p string) bool { return !held(p) })
	res.forwarded, res.dropped = c["released_after_hold"], sum(c, "dropped.")
	return res
}

func TestHeldStoreLoad(t *testing.T) {
	if os.Getenv("TERMA_HELD_LOAD") == "" {
		t.Skip("TERMA_HELD_LOAD=1 measures the held store under load")
	}
	for _, sc := range loadScenarios {
		t.Run(sc.name, func(t *testing.T) {
			r := runLoad(t, sc)
			t.Logf("exports=%d p50=%v p99=%v cpu=%v allocs/export=%d bytes/export=%d held_store: writes=%d removes=%d bytes=%d left=%d/%dB outbox: files=%d bytes=%d released_records=%d dropped_records=%d",
				r.exports, r.p50, r.p99, r.cpu, r.allocs/uint64(r.exports), r.allocBytes/uint64(r.exports),
				r.heldWrites, r.heldRemoves, r.heldBytes, r.heldLeft, r.heldLeftBytes, r.outboxFiles, r.outboxBytes, r.forwarded, r.dropped)
		})
	}
}

// BenchmarkRelayHeldExport is one export of an unclaimed session, which the relay holds.
func BenchmarkRelayHeldExport(b *testing.B) {
	f := newFixture()
	r := newRelay(Options{Dir: b.TempDir(), Token: token, Lookup: f.lookup})
	runRelay(b, r)
	body, _ := proto.Marshal(agentLogs("session.id", "P", 3, false))
	h := r.Handler()
	var lat []time.Duration
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/x-protobuf")
		start := time.Now()
		h.ServeHTTP(httptest.NewRecorder(), req)
		lat = append(lat, time.Since(start))
	}
	slices.Sort(lat)
	b.ReportMetric(float64(lat[len(lat)/2].Nanoseconds()), "p50-ns")
	b.ReportMetric(float64(lat[len(lat)*99/100].Nanoseconds()), "p99-ns")
	c := r.Stats().Snapshot().Counters
	b.ReportMetric(float64(c["held_store_bytes_written"])/float64(b.N), "store-B/op")
	b.ReportMetric(float64(len(body)), "export-B")
}
