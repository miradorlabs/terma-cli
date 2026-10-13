package relay

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// linkRelay is a relay over B, a Codex session whose project withholds content, sending to u from dir.
func linkRelay(t *testing.T, u *upstream) (r *Relay, srv *httptest.Server, f *fixture, dir string, stop func()) {
	t.Helper()
	f, dir = newFixture(), t.TempDir()
	r = newRelay(Options{Dir: dir, Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock, Grace: 2 * time.Second,
		Resolve: func(c claim.Claim) (Policy, error) {
			if p, ok := allPolicies(u)[c.ProjectID]; ok {
				return p, nil
			}
			return Policy{}, ErrNoKey
		}})
	srv, stop = startRelay(t, r)
	return r, srv, f, dir, stop
}

// toolResults are Codex tool_result records of session B, each a write_stdin poll unless named.
func toolResults(calls ...string) *logspb.LogsData {
	var recs []*logspb.LogRecord
	for _, call := range calls {
		tool, id, _ := strings.Cut(call, ":")
		if id == "" {
			tool, id = "write_stdin", call
		}
		recs = append(recs, &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
			kv("event.name", "codex.tool_result"), kv("conversation.id", "B"), kv("tool_name", tool),
			kv("call_id", id), kv("output", "Process running with session ID 3"),
		}})
	}
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}}}}}
}

// pollSpan is Codex's unified_exec.write_stdin span for the poll call, naming the exec it polls ("" for none).
func pollSpan(call, exec string) *tracepb.TracesData {
	attrs := []*commonpb.KeyValue{kv("conversation.id", "B"), kv("call_id", call), kv("interaction", "poll")}
	if exec != "" {
		attrs = append(attrs, kv("original_exec_call_id", exec))
	}
	return &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		{Name: "unified_exec.write_stdin", Attributes: attrs},
	}}}}}}
}

// origins is each forwarded record's call_id → its stamped origin ("" when unstamped).
func (u *upstream) origins(t *testing.T) map[string]string {
	t.Helper()
	got, _ := u.logs(t)
	out := map[string]string{}
	for _, lr := range got["Bearer key-p2"] {
		out[attr(lr.Attributes, "call_id")] = attr(lr.Attributes, semconv.TermaToolCallOriginIDKey)
	}
	return out
}

// A poll's record waits for its span and leaves stamped with the exec it polls, before the
// span, under a policy that withholds content: the span's own key is withheld, the stamp is not.
func TestLinkStampsAPollWhoseSpanFollows(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	r, srv, _, _, _ := linkRelay(t, u)
	postProto(t, srv, "/v1/logs", toolResults("exec-poll"))
	if c := r.Stats().Snapshot().Counters; c["linked_records"] != 0 || c["forwarded.logs"] != 0 {
		t.Fatalf("the poll left before its span: %v", c)
	}
	postProto(t, srv, "/v1/traces", pollSpan("exec-poll", "exec-orig"))
	// The span's export routes the poll before it answers.
	if c := r.Stats().Snapshot().Counters; c["linked_records"] != 1 {
		t.Fatalf("the span did not release its poll: %v", c)
	}
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 && c["forwarded.traces"] == 1 })
	if got := u.origins(t); got["exec-poll"] != "exec-orig" {
		t.Fatalf("origins = %v", got)
	}
}

// A span that came first stamps its poll as the poll arrives, without a wait, and the poll
// does not overtake the record exported before it.
func TestLinkStampsAPollWhoseSpanCameFirst(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	r, srv, _, _, _ := linkRelay(t, u)
	postProto(t, srv, "/v1/traces", pollSpan("exec-poll", "exec-orig"))
	postProto(t, srv, "/v1/logs", toolResults("exec_command:exec-cmd", "exec-poll"))
	if c := r.Stats().Snapshot().Counters; c["linked_records"] != 1 {
		t.Fatalf("the poll waited for a span already seen: %v", c)
	}
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 2 })
	if got := u.origins(t); got["exec-poll"] != "exec-orig" {
		t.Fatalf("origins = %v", got)
	}
	got, _ := u.logs(t)
	var order []string
	for _, lr := range got["Bearer key-p2"] {
		order = append(order, attr(lr.Attributes, "call_id"))
	}
	if !slices.Equal(order, []string{"exec-cmd", "exec-poll"}) {
		t.Fatalf("order = %v", order)
	}
}

// A poll whose span never comes leaves unstamped once the hold runs out, not before; one
// whose span names no exec (a process Codex no longer knew) leaves unstamped at once.
func TestLinkSendsAPollUnstampedWithoutItsSpan(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	r, srv, f, _, _ := linkRelay(t, u)
	postProto(t, srv, "/v1/logs", toolResults("exec-lost", "exec-unknown"))
	postProto(t, srv, "/v1/traces", pollSpan("exec-unknown", ""))
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
	f.advance(linkHold - time.Second)
	r.sweep()
	if c := r.Stats().Snapshot().Counters; c["unlinked_records"] != 1 {
		t.Fatalf("the poll left before its hold ran out: %v", c)
	}
	f.advance(time.Second)
	r.sweep()
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 2 && c["unlinked_records"] == 2 })
	if got := u.origins(t); len(got) != 2 || got["exec-lost"] != "" || got["exec-unknown"] != "" {
		t.Fatalf("origins = %v", got)
	}
}

// Only a poll waits: the records beside it leave as they arrive, without an origin an
// agent sent itself, since the stamp is the relay's.
func TestLinkHoldsNothingButPolls(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	r, srv, _, _, _ := linkRelay(t, u)
	m := toolResults("exec_command:exec-cmd", "exec-poll", "apply_patch:call-patch")
	m.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes = append(m.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes,
		kv(semconv.TermaToolCallOriginIDKey, "forged"))
	postProto(t, srv, "/v1/logs", m)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 2 })
	if got := u.origins(t); len(got) != 2 || got["exec-cmd"] != "" || got["call-patch"] != "" {
		t.Fatalf("origins = %v", got)
	}
	if c := r.Stats().Snapshot().Counters; c["received.logs"] != 3 || c["linked_records"]+c["unlinked_records"] != 0 {
		t.Fatalf("stats = %v", c)
	}
}

// The waits and the span values are bounded: a full wait sends its oldest poll on
// unstamped, and a full set of values forgets the oldest.
func TestLinkBounds(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	r, srv, _, _, _ := linkRelay(t, u)
	var calls []string
	for i := range maxLinkWaits + 1 {
		calls = append(calls, fmt.Sprintf("exec-%d", i))
	}
	postProto(t, srv, "/v1/logs", toolResults(calls...))
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 && c["unlinked_records"] == 1 })
	if got := u.origins(t); len(got) != 1 || got["exec-0"] != "" {
		t.Fatalf("evicted = %v; want the oldest poll", got)
	}
	r.mu.Lock()
	waiting := len(r.linkWaits)
	r.mu.Unlock()
	if waiting != maxLinkWaits {
		t.Fatalf("%d polls wait; want %d", waiting, maxLinkWaits)
	}

	spans := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{}}}}}
	for i := range maxLinkValues + 1 {
		spans.ResourceSpans[0].ScopeSpans[0].Spans = append(spans.ResourceSpans[0].ScopeSpans[0].Spans,
			pollSpan(fmt.Sprintf("span-%d", i), "exec-orig").ResourceSpans[0].ScopeSpans[0].Spans[0])
	}
	postProto(t, srv, "/v1/traces", spans)
	r.mu.Lock()
	values := len(r.linkValues)
	r.mu.Unlock()
	if values != maxLinkValues {
		t.Fatalf("%d span values kept; want %d", values, maxLinkValues)
	}
}

// A stopping relay sends a waiting poll on unstamped rather than drop it.
func TestLinkStopSendsWaitingPolls(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	r, srv, _, _, stop := linkRelay(t, u)
	postProto(t, srv, "/v1/logs", toolResults("exec-poll"))
	if _, idle := r.Idle(); idle || r.Quiesce(0) || !r.Holding() {
		t.Fatal("quiesced or not holding with a poll waiting for its span")
	}
	stop()
	if c := r.Stats().Snapshot().Counters; c["unlinked_records"] != 1 || c["forwarded.logs"] != 1 {
		t.Fatalf("stats = %v", c)
	}
	if got := u.origins(t); len(got) != 1 || got["exec-poll"] != "" {
		t.Fatalf("origins = %v", got)
	}
}

// The stamped poll reaches the outbox before the span that released it.
func TestLinkQueuesThePollBeforeItsSpan(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	u.status = http.StatusServiceUnavailable // so both stay in the outbox to be seen
	r, srv, _, dir, _ := linkRelay(t, u)
	postProto(t, srv, "/v1/logs", toolResults("exec-poll"))
	postProto(t, srv, "/v1/traces", pollSpan("exec-poll", "exec-orig"))
	if c := r.Stats().Snapshot().Counters; c["linked_records"] != 1 {
		t.Fatalf("stats = %v", c)
	}
	var names []string
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(d.Name(), pbSuffix) && !strings.HasPrefix(d.Name(), ".tmp-") {
			names = append(names, d.Name())
		}
		return nil
	})
	slices.Sort(names)
	if len(names) != 2 || !strings.Contains(names[0], "-logs-") || !strings.Contains(names[1], "-traces-") {
		t.Fatalf("outbox = %v; want the poll, then its span", names)
	}
}
