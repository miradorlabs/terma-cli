package relay

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// startRelay runs r with a server that names every sender by PeerPID; stop ends both.
func startRelay(t *testing.T, r *Relay) (srv *httptest.Server, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	srv = httptest.NewUnstartedServer(r.Handler())
	srv.Config.ConnContext = r.ConnContext
	srv.Start()
	var stopped atomic.Bool
	stop = func() {
		if stopped.Swap(true) {
			return
		}
		srv.Close()
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return srv, stop
}

func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *fixture) claim(session string, c claim.Claim) {
	f.mu.Lock()
	f.claims[session] = c
	f.mu.Unlock()
}

// sessionlessSpans are a turn's child spans, which name no session: Codex's fs and auth spans.
func sessionlessSpans(trace []byte, n int) *tracepb.TracesData {
	var spans []*tracepb.Span
	for range n {
		spans = append(spans, &tracepb.Span{Name: "fs.read_file", TraceId: trace})
	}
	return &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}}
}

func namingLog(trace []byte, session string) *logspb.LogsData {
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
		{TraceId: trace, Attributes: []*commonpb.KeyValue{kv("conversation.id", session), kv("event.name", "codex.api_request")}},
	}}}}}}
}

func postProto(t *testing.T, srv *httptest.Server, path string, m proto.Message) {
	t.Helper()
	body, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if code := post(t, srv, path, body, "application/x-protobuf", token, false); code != 200 {
		t.Fatalf("%s = %d", path, code)
	}
}

// globalSwitch is a machine whose organization can switch to "Every repository": its
// catch-all and its default project's need for a claim follow the switch, as the daemon's do.
type globalSwitch struct {
	on atomic.Bool
	u  *upstream
}

func (g *globalSwitch) catchAll() (claim.Claim, bool) {
	return claim.Claim{ProjectID: "p-default"}, g.on.Load()
}

func (g *globalSwitch) resolve(c claim.Claim) (Policy, error) {
	switch c.ProjectID {
	case "p-default":
		return Policy{Endpoint: g.u.srv.URL, Key: "key-default", IncludePrompts: true, IncludeToolContent: true, RequireClaim: !g.on.Load()}, nil
	case "p1":
		return Policy{Endpoint: g.u.srv.URL, Key: "key-p1", IncludePrompts: true, IncludeToolContent: true, RequireClaim: true}, nil
	}
	return Policy{}, ErrNoKey
}

// Switching to "Every repository" collects from then on, never what was held while a
// session was not collected: a session in a repository without terma, claimed by global
// mode's hook once it is on, sends none of what it exported before. A session an installed
// repository claimed for its own project still goes.
func TestRelaySwitchToGlobalSendsNothingHeldFromBefore(t *testing.T) {
	t.Parallel()
	g := &globalSwitch{u: newUpstream(t)}
	f := newFixture()
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: 2 * time.Minute, Lookup: f.lookup, Now: f.clock,
		CatchAll: g.catchAll, Resolve: g.resolve})
	srv, _ := startRelay(t, r)
	trace := []byte("fedcba9876543210")
	postProto(t, srv, "/v1/logs", logsOf("U", 2))               // a repository without terma
	postProto(t, srv, "/v1/traces", sessionlessSpans(trace, 2)) // and its turn's child spans
	postProto(t, srv, "/v1/logs", logsOf("I", 1))               // an installed repository, claimed late
	r.sweep()
	if c := r.Stats().Snapshot().Counters; c["held_parts"] != 3 || c["forwarded.logs"] != 0 {
		t.Fatalf("before the switch: %v", c)
	}

	f.advance(30 * time.Second)
	g.on.Store(true)
	f.claim("U", claim.Claim{ProjectID: "p-default", Tool: "codex"}) // global mode's hook
	f.claim("I", claim.Claim{ProjectID: "p1", Tool: "codex"})
	postProto(t, srv, "/v1/logs", namingLog(trace, "U"))
	r.sweep()
	// The naming log arrived after the switch, so it goes; the earlier records never do,
	// not at once, not when their hold runs out.
	waitFor(t, func() bool {
		c := r.Stats().Snapshot().Counters
		return c["forwarded.logs"] == 2
	})
	f.advance(31 * time.Minute)
	r.sweep()
	time.Sleep(50 * time.Millisecond)
	c := r.Stats().Snapshot().Counters
	if c["dropped.policy_widened.logs"] != 2 || c["dropped.policy_widened.traces"] != 2 || c["forwarded.traces"] != 0 || c["forwarded.logs"] != 2 {
		t.Fatalf("after the switch: %v", c)
	}
	byAuth, _ := g.u.logs(t)
	if len(byAuth["Bearer key-p1"]) != 1 || len(byAuth["Bearer key-default"]) != 1 {
		t.Fatalf("delivered by key: p1 %d, default %d", len(byAuth["Bearer key-p1"]), len(byAuth["Bearer key-default"]))
	}
}
