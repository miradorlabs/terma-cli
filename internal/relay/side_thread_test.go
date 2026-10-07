package relay

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// testdata/codex_desktop_side_threads.json is Codex Desktop 0.155.0-alpha.16.3's own
// records from production, host and project removed: the start of a voice-chat thread its
// hooks claimed, then two side threads Desktop forked to title and describe threads, which
// fire no hook and write no rollout.
const (
	desktopThread     = "01a116c1-c3b9-7e01-b3aa-7a3111ff2050"
	desktopTitleFork  = "01a1172f-c001-7683-8608-5c8547edaba8"
	desktopDescribing = "01a116c2-b7aa-7ad1-ac9f-12e190854a6e"
)

func desktopSideThreadLogs(t *testing.T) *logspb.LogsData {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "codex_desktop_side_threads.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recs []struct {
		Time       time.Time         `json:"time"`
		Resource   map[string]string `json:"resource"`
		Attributes map[string]string `json:"attributes"`
	}
	if err := json.Unmarshal(b, &recs); err != nil {
		t.Fatal(err)
	}
	kvs := func(m map[string]string) []*commonpb.KeyValue {
		var out []*commonpb.KeyValue
		for k, v := range m {
			out = append(out, kv(k, v))
		}
		return out
	}
	logs := &logspb.LogsData{}
	for _, r := range recs {
		logs.ResourceLogs = append(logs.ResourceLogs, &logspb.ResourceLogs{
			Resource: &resourcepb.Resource{Attributes: kvs(r.Resource)},
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
				{TimeUnixNano: uint64(r.Time.UnixNano()), Attributes: kvs(r.Attributes)},
			}}},
		})
	}
	return logs
}

// In global mode a Codex thread goes by its hook's claim, and Desktop's title and
// description forks, which no hook claims, never leave: before, each listed as a one-turn session.
func TestRelayDropsCodexDesktopSideThreadsInGlobalMode(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	f.claims[desktopThread] = claim.Claim{ProjectID: "p-default", Tool: "codex"}
	pol := Policy{Endpoint: u.srv.URL, Key: "key-default", IncludePrompts: true, IncludeToolContent: true}
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		CatchAll: func() (claim.Claim, bool) { return claim.Claim{ProjectID: "p-default"}, true },
		Resolve:  func(claim.Claim) (Policy, error) { return pol, nil }})
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	body, _ := proto.Marshal(desktopSideThreadLogs(t))
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
	f.mu.Lock()
	// A conversation start waits as long as a trace for its claim.
	f.now = f.now.Add(DefaultTraceHold + time.Minute)
	f.mu.Unlock()
	waitForCounters(t, r, func(c map[string]int) bool { return c["dropped.unclaimed_expired.logs"] == 4 })
	if c := r.Stats().Snapshot().Counters; c["forwarded.logs"] != 1 || c["attributed_by_catch_all.logs"] != 0 {
		t.Fatalf("counters %v", c)
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	var sent strings.Builder
	for _, req := range u.requests {
		sent.Write(req.body)
	}
	if !strings.Contains(sent.String(), desktopThread) {
		t.Fatal("the claimed thread's start was not sent")
	}
	for _, s := range []string{desktopTitleFork, desktopDescribing, "You are in a fork of"} {
		if strings.Contains(sent.String(), s) {
			t.Fatalf("a side thread left the machine: %q", s)
		}
	}
}

// A fork's child span that names no session, exported after the span that named the fork's
// thread, inherits the fork's claimed key through its trace, so it waits and is dropped too.
func TestRelayDropsASideThreadsLaterChildSpanInGlobalMode(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	pol := Policy{Endpoint: u.srv.URL, Key: "key-default", IncludePrompts: true, IncludeToolContent: true}
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		CatchAll: func() (claim.Claim, bool) { return claim.Claim{ProjectID: "p-default"}, true },
		Resolve:  func(claim.Claim) (Policy, error) { return pol, nil }})
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	trace := make([]byte, 16)
	trace[0] = 7
	spans := func(sp *tracepb.Span) []byte {
		b, _ := proto.Marshal(&tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
			Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "codex-app-server")}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}}}})
		return b
	}
	post(t, srv, "/v1/traces", spans(&tracepb.Span{TraceId: trace, SpanId: []byte{1, 0, 0, 0, 0, 0, 0, 0}, Name: "session_loop",
		Attributes: []*commonpb.KeyValue{kv("thread_id", desktopTitleFork)}}), "application/x-protobuf", token, false)
	waitForCounters(t, r, func(c map[string]int) bool { return c["received.traces"] == 1 })
	post(t, srv, "/v1/traces", spans(&tracepb.Span{TraceId: trace, SpanId: []byte{2, 0, 0, 0, 0, 0, 0, 0}, Name: "handle_responses"}),
		"application/x-protobuf", token, false)
	waitForCounters(t, r, func(c map[string]int) bool { return c["received.traces"] == 2 })
	f.mu.Lock()
	f.now = f.now.Add(2 * time.Minute)
	f.mu.Unlock()
	waitForCounters(t, r, func(c map[string]int) bool { return c["dropped.unclaimed_expired.traces"] == 2 })
	if c := r.Stats().Snapshot().Counters; c["forwarded.traces"] != 0 {
		t.Fatalf("a side thread's span left: %v", c)
	}
}
