package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestCaptureWithholdsProviderErrorAndOtherAttributeChannels(t *testing.T) {
	t.Parallel()
	for _, flags := range [][2]bool{{false, false}, {false, true}, {true, false}, {true, true}} {
		span := &tracepb.Span{Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "PRIVATE_CONTENT"}, Links: []*tracepb.Span_Link{{Attributes: []*commonpb.KeyValue{kv("gen_ai.prompt", "PRIVATE_CONTENT")}}}}
		scope := &commonpb.InstrumentationScope{Attributes: []*commonpb.KeyValue{kv("gen_ai.prompt", "PRIVATE_CONTENT")}}
		p := &part{signal: Traces, msg: &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Scope: scope, Spans: []*tracepb.Span{span}}}}}}}
		testRules.withhold(p, flags[0], flags[1], map[string]int{})
		if !flags[0] || !flags[1] {
			if span.Status.Message != "" || span.Status.Code != tracepb.Status_STATUS_CODE_ERROR {
				t.Fatal("provider error text survived or status code changed")
			}
		}
		if !flags[0] && (len(scope.Attributes) != 0 || len(span.Links[0].Attributes) != 0) {
			t.Fatal("prompt escaped through scope/link attributes")
		}
	}
}

func TestQueuedExportsRespectSignalAndCoverageChanges(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		policy Policy
	}{
		{"signal disabled", Policy{Signals: []string{"metrics"}, IncludePrompts: true, IncludeToolContent: true}},
		{"left global coverage", Policy{RequireClaim: true, IncludePrompts: true, IncludeToolContent: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("disallowed queued export reached upstream")
				w.WriteHeader(200)
			}))
			defer host.Close()
			policy := test.policy
			policy.Endpoint = host.URL
			policy.Key = "key"
			r := newRelay(Options{Dir: t.TempDir(), Resolve: func(claim.Claim) (Policy, error) { return policy, nil }})
			m := logsOf("session", 1)
			m.ResourceLogs[0].Resource = &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv(semconv.TermaRelayAttributionKey, "catch-all")}}
			body, err := proto.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			rt := route{project: "team", tool: "codex", repo: noRepo}
			e := newEntry(time.Now(), Logs, 1)
			if err := r.outbox.put(rt, config.Repository{}, e, body); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { r.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			// The drop is counted before its file goes: wait for both.
			waitFor(t, func() bool {
				entries, err := r.outbox.list(rt)
				return err == nil && len(entries) == 0 && r.Stats().Snapshot().Counters["dropped.policy_signal_or_content.logs"] == 1
			})
		})
	}
}

// A part queued in a repository the team policy has since dropped is dropped; one still listed is sent.
func TestQueuedExportsRecheckTheRepository(t *testing.T) {
	t.Parallel()
	removed := config.Repository{Origin: "github.com/acme/removed"}
	kept := config.Repository{Origin: "github.com/acme/kept"}
	var got atomic.Value
	host := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		got.Store(string(b))
	}))
	defer host.Close()
	r := newRelay(Options{Dir: t.TempDir(), Resolve: func(c claim.Claim) (Policy, error) {
		if c.Repository == (config.Repository{}) {
			t.Error("the sender resolved a queued part without its repository")
		}
		return Policy{Endpoint: host.URL, Key: "key", Unadmitted: c.Repository != kept}, nil
	}})
	for _, repo := range []config.Repository{removed, kept} {
		body, err := proto.Marshal(logsOf("session-"+path.Base(repo.Origin), 1))
		if err != nil {
			t.Fatal(err)
		}
		c := claim.Claim{ProjectID: "team", Tool: "codex", Repository: repo}
		if err := r.outbox.put(routeOf(c), repo, newEntry(time.Now(), Logs, 1), body); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool {
		c := r.Stats().Snapshot().Counters
		return c["dropped.policy_repository.logs"] == 1 && c["forwarded.logs"] == 1
	})
	if body, _ := got.Load().(string); !strings.Contains(body, "session-kept") || strings.Contains(body, "session-removed") {
		t.Fatalf("upstream got %q, want only the kept repository's part", body)
	}
	if entries, _ := r.outbox.list(routeOf(claim.Claim{ProjectID: "team", Tool: "codex", Repository: removed})); len(entries) != 0 {
		t.Fatalf("the removed repository's part stayed queued: %v", entries)
	}
}

func TestQueuedCapturePolicyDropsCorruptBodies(t *testing.T) {
	t.Parallel()
	r := newRelay(Options{})
	if got, _ := r.withholdQueued(Traces, []byte{0xff}, Policy{}); got != nil {
		t.Fatal("uncheckable body was forwarded")
	}
}

// A queued batch the upstream asks to retry is counted once, when it finally leaves.
func TestQueuedWithholdingCountsOnceAcrossRetries(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer host.Close()
	policy := Policy{Endpoint: host.URL, Key: "key"}
	r := newRelay(Options{Dir: t.TempDir(), Resolve: func(claim.Claim) (Policy, error) { return policy, nil }})
	m := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		{Attributes: []*commonpb.KeyValue{kv("file_path", "src/secrets/app.env")}},
		{Attributes: []*commonpb.KeyValue{kv("file_path", "README.md")}},
	}}}}}}
	body, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.outbox.put(route{project: "team", tool: "claude", repo: noRepo}, config.Repository{}, newEntry(time.Now(), Traces, 2), body); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.traces"] > 0 })
	c := r.Stats().Snapshot().Counters
	if c["upstream_retries"] != 2 || c["forwarded.traces"] != 2 || c["withheld_at_send_records"] != 2 {
		t.Fatalf("2 retries, then 2 forwarded and 2 withheld wanted: %v", c)
	}
}
