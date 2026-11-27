package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

func makeTraceBatch(sessionID string, start time.Time, texts ...string) *tracepb.TracesData {
	out := &tracepb.TracesData{}
	for i, text := range texts {
		t0 := start.Add(time.Duration(i) * time.Second)
		t1 := t0.Add(500 * time.Millisecond)
		out.ResourceSpans = append(out.ResourceSpans, &tracepb.ResourceSpans{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				{Key: semconv.ServiceNameKey, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "oh-my-pi"}}},
				{Key: semconv.TermaSessionHistoricalKey, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "@oh-my-pi/pi-agent-core"},
				Spans: []*tracepb.Span{{
					TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, byte(i + 1)},
					SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, byte(i + 1)},
					Name:              "chat test-model",
					StartTimeUnixNano: uint64(t0.UnixNano()),
					EndTimeUnixNano:   uint64(t1.UnixNano()),
					Attributes: []*commonpb.KeyValue{
						{Key: "gen_ai.operation.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "chat"}}},
						{Key: "gen_ai.conversation.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: sessionID}}},
						{Key: "gen_ai.input.messages", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: text}}},
						{Key: semconv.TermaSessionHistoricalKey, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
					},
				}},
			}},
		})
	}
	return out
}

func TestChunkWriterTracesTargetLargeAndHardLimit(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)

	// Many small spans split at TargetChunkBytes with Path=/v1/traces.
	var texts []string
	for i := range 1600 {
		texts = append(texts, fmt.Sprintf("%04d-%s", i, strings.Repeat("x", 1024)))
	}
	var chunks []Chunk
	w := NewChunkWriter(MaxRequestBytes, func(ch Chunk) error { chunks = append(chunks, ch); return nil })
	if err := w.AddTraces(makeTraceBatch("s-trace", t0, texts...)); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 3 {
		t.Fatalf("got %d trace chunks for ~1.6 MiB, want several", len(chunks))
	}
	var got []string
	for i, ch := range chunks {
		if ch.Path != otlpTracesPath {
			t.Errorf("chunk %d path = %q, want %q", i, ch.Path, otlpTracesPath)
		}
		if len(ch.Body) > TargetChunkBytes {
			t.Errorf("chunk %d size = %d > TargetChunkBytes (%d)", i, len(ch.Body), TargetChunkBytes)
		}
		var msg tracepb.TracesData
		if err := proto.Unmarshal(ch.Body, &msg); err != nil {
			t.Fatalf("unmarshal chunk %d: %v", i, err)
		}
		n := 0
		for _, rs := range msg.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					got = append(got, spanAttrString(sp, "gen_ai.input.messages"))
					n++
				}
			}
		}
		if n != ch.Records {
			t.Errorf("chunk %d Records=%d, actual spans=%d", i, ch.Records, n)
		}
	}
	if !slices.Equal(got, texts) {
		t.Fatalf("trace spans lost or reordered: got %d, want %d", len(got), len(texts))
	}

	// Single span > TargetChunkBytes and <= MaxRequestBytes goes alone.
	big := strings.Repeat("b", 1<<20)
	chunks = nil
	w = NewChunkWriter(0, func(ch Chunk) error { chunks = append(chunks, ch); return nil })
	if err := w.AddTraces(makeTraceBatch("s-big", t0, "before", big, "after")); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3 (before, big alone, after)", len(chunks))
	}
	if len(chunks[1].Body) <= TargetChunkBytes || len(chunks[1].Body) > MaxRequestBytes {
		t.Errorf("large trace chunk size = %d", len(chunks[1].Body))
	}

	// Single span over MaxRequestBytes fails with *PermanentError.
	w = NewChunkWriter(0, func(Chunk) error { return nil })
	err := w.AddTraces(makeTraceBatch("s-huge", t0, strings.Repeat("z", MaxRequestBytes+1)))
	if _, ok := errors.AsType[*PermanentError](err); !ok {
		t.Fatalf("oversized span error = %v, want *PermanentError", err)
	}
}

func TestFilterMixedTracesCutoffResumeCapAndLookup(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)

	// Resume cap drops newer spans without counting them as Mixed drops.
	msg := makeTraceBatch("s-1", t0, "early")
	later := makeTraceBatch("s-1", t0.Add(2*time.Hour), "later")
	msg.ResourceSpans = append(msg.ResourceSpans, later.ResourceSpans...)
	kept, dropped := FilterMixedTraces(msg, runs.Snapshot{}, nil, time.Time{}, false, t0.Add(time.Hour))
	if kept != 1 || dropped != 0 {
		t.Errorf("resume cap: kept=%d dropped=%d, want 1 and 0", kept, dropped)
	}

	// Span straddling MixedAt (start before, end after) is dropped and counted.
	straddle := makeTraceBatch("s-1", t0, "straddle")
	straddle.ResourceSpans[0].ScopeSpans[0].Spans[0].EndTimeUnixNano = uint64(t0.Add(10 * time.Second).UnixNano())
	kept, dropped = FilterMixedTraces(straddle, runs.Snapshot{}, nil, t0.Add(5*time.Second), true, time.Time{})
	if kept != 0 || dropped != 1 {
		t.Errorf("straddling MixedAt: kept=%d dropped=%d, want 0 and 1", kept, dropped)
	}

	// Subagent span checked via lookup(gen_ai.conversation.id) against its own run ledger.
	stateDir := t.TempDir()
	if err := runs.MarkMixed(stateDir, "sub-mixed", t0.Add(-time.Minute), runs.Why{Reason: "cross_org"}); err != nil {
		t.Fatal(err)
	}
	sub := makeTraceBatch("sub-mixed", t0, "from-sub")
	kept, dropped = FilterMixedTraces(sub, runs.Snapshot{}, func(id string) runs.Snapshot {
		return runs.Read(stateDir, id)
	}, time.Time{}, false, time.Time{})
	if kept != 0 || dropped != 1 {
		t.Errorf("subagent lookup Mixed: kept=%d dropped=%d, want 0 and 1", kept, dropped)
	}
}

func TestTransportPostsTracesPathAndParsesPartialSuccess(t *testing.T) {
	t.Parallel()
	var gotPath, gotCT, gotProject string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotProject = r.Header.Get("X-Mirador-Project")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encodePartialSuccess(2, "bad span"))
	}))
	defer srv.Close()

	tr := &Transport{
		Version:     "test",
		HTTP:        srv.Client(),
		SignIn:      &fakeSignIn{account: "acc1", tokens: []string{"tok1"}},
		BytesPerSec: 1 << 30,
	}
	err := tr.NewRoutePacer().Send(context.Background(), SendRequest{
		Endpoint: srv.URL,
		Path:     otlpTracesPath,
		TeamID:   "team-a",
		Account:  "acc1",
		Body:     []byte{0x0a, 0x00},
	})
	if gotPath != otlpTracesPath {
		t.Errorf("path = %q, want %q", gotPath, otlpTracesPath)
	}
	if gotCT != "application/x-protobuf" || gotProject != "team-a" {
		t.Errorf("headers ct=%q project=%q", gotCT, gotProject)
	}
	if _, ok := errors.AsType[*PermanentError](err); !ok || !strings.Contains(err.Error(), "bad span") {
		t.Fatalf("partial success error = %v, want *PermanentError mentioning bad span", err)
	}
}

func TestOmpHistoryUploadEndToEndPolicyHoldingsMixedAndOutboxIsolation(t *testing.T) {
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	repoB := initTestGitRepo(t, "https://github.com/org-b/repo-b.git")
	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-48 * time.Hour)

	projDir := filepath.Join(agentDir, "sessions", "--repo-a--")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoA, "app.go"), []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoB, "secret.go"), []byte("package b\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	writeSession := func(rel, content string) string {
		t.Helper()
		full := filepath.Join(projDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(full, oldTime, oldTime)
		return full
	}

	// 1. Clean session with confirmed edit + subagent.
	cleanParent := writeSession("2026-11-23T10-00-00-000Z_omp-clean.jsonl", strings.Join([]string{
		`{"type":"title","title":"SECRET-TITLE","updatedAt":"2026-11-23T10:00:01Z"}`,
		`{"type":"session","version":3,"id":"omp-clean","timestamp":"2026-11-23T10:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"session_init","id":"i1","timestamp":"2026-11-23T10:00:00.100Z","runtime":{"version":"19.0.1"},"systemPrompt":"SECRET-SYS"}`,
		`{"type":"message","id":"u1","timestamp":"2026-11-23T10:00:01Z","message":{"role":"user","content":"SECRET-PROMPT","timestamp":1795428001000}}`,
		`{"type":"message","id":"a1","timestamp":"2026-11-23T10:00:02Z","message":{"role":"assistant","provider":"anthropic","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":500,"timestamp":1795428002000,"usage":{"input":10,"output":20,"cacheRead":30,"cacheWrite":5},"content":[{"type":"text","text":"SECRET-REPLY"},{"type":"toolCall","id":"edit_1","name":"write","arguments":{"path":` + mustJSONTest(filepath.Join(repoA, "app.go")) + `,"content":"SECRET-CONTENT"}}]}}`,
		`{"type":"message","id":"tr1","timestamp":"2026-11-23T10:00:03Z","message":{"role":"toolResult","toolCallId":"edit_1","toolName":"write","content":[{"type":"text","text":"SECRET-RESULT"}],"isError":false,"timestamp":1795428003000}}`,
	}, "\n")+"\n")
	writeSession(filepath.Join("2026-11-23T10-00-00-000Z_omp-clean", "0-explore.jsonl"), strings.Join([]string{
		`{"type":"session","version":3,"id":"omp-clean-sub","timestamp":"2026-11-23T10:00:04Z","cwd":` + mustJSONTest(repoA) + `,"parentSession":` + mustJSONTest(cleanParent) + `}`,
		`{"type":"session_init","id":"si1","timestamp":"2026-11-23T10:00:04.100Z","agent":"explore","runtime":{"version":"19.0.1"}}`,
		`{"type":"message","id":"su1","timestamp":"2026-11-23T10:00:04.200Z","message":{"role":"user","content":"SECRET-SUB-PROMPT","timestamp":1795428004200}}`,
		`{"type":"message","id":"sa1","timestamp":"2026-11-23T10:00:04.500Z","message":{"role":"assistant","provider":"anthropic","model":"claude-sonnet-4-6","stopReason":"stop","duration":200,"timestamp":1795428004500,"usage":{"input":5,"output":8},"content":[{"type":"text","text":"SECRET-SUB-REPLY"}]}}`,
	}, "\n")+"\n")

	// 2. Held session (already on server, must be skipped along with its subagent).
	heldParent := writeSession("2026-11-23T09-00-00-000Z_omp-held.jsonl",
		`{"type":"session","version":3,"id":"omp-held","timestamp":"2026-11-23T09:00:00Z","cwd":`+mustJSONTest(repoA)+`}`+"\n"+
			`{"type":"message","id":"hu1","timestamp":"2026-11-23T09:00:01Z","message":{"role":"user","content":"already held","timestamp":1795424401000}}`+"\n"+
			`{"type":"message","id":"ha1","timestamp":"2026-11-23T09:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795424402000,"usage":{"input":1,"output":1},"content":[{"type":"text","text":"ok"}]}}`+"\n",
	)
	writeSession(filepath.Join("2026-11-23T09-00-00-000Z_omp-held", "0-worker.jsonl"),
		`{"type":"session","version":3,"id":"omp-held-sub","timestamp":"2026-11-23T09:00:03Z","cwd":`+mustJSONTest(repoA)+`,"parentSession":`+mustJSONTest(heldParent)+`}`+"\n"+
			`{"type":"message","id":"hsa1","timestamp":"2026-11-23T09:00:04Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795424404000,"usage":{"input":1,"output":1},"content":[{"type":"text","text":"sub"}]}}`+"\n",
	)

	// 3. Cross-org Mixed session: starts in repoA, reads repoB at 10:00:02 -> content at/after 10:00:02 dropped.
	writeSession("2026-11-23T08-00-00-000Z_omp-mixed.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"omp-mixed","timestamp":"2026-11-23T08:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"mu1","timestamp":"2026-11-23T08:00:01Z","message":{"role":"user","content":"before cross","timestamp":1795420801000}}`,
		`{"type":"message","id":"ma1","timestamp":"2026-11-23T08:00:01.500Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","duration":200,"timestamp":1795420801500,"usage":{"input":3,"output":4},"content":[{"type":"text","text":"early ok"}]}}`,
		`{"type":"message","id":"mu2","timestamp":"2026-11-23T08:00:02Z","message":{"role":"user","content":"read org-b","timestamp":1795420802000}}`,
		`{"type":"message","id":"ma2","timestamp":"2026-11-23T08:00:03Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":200,"timestamp":1795420803000,"usage":{"input":3,"output":4},"content":[{"type":"toolCall","id":"cross_read","name":"read","arguments":{"path":` + mustJSONTest(filepath.Join(repoB, "secret.go")) + `}}]}}`,
	}, "\n")+"\n")

	// 4. Orphan subagent (deleted parent) must fail closed (withheld as Mixed).
	writeSession(filepath.Join("2026-11-23T07-00-00-000Z_omp-missing-parent", "0-orphan.jsonl"),
		`{"type":"session","version":3,"id":"omp-orphan","timestamp":"2026-11-23T07:00:01Z","cwd":`+mustJSONTest(repoA)+`,"parentSession":`+mustJSONTest(filepath.Join(projDir, "2026-11-23T07-00-00-000Z_omp-missing-parent.jsonl"))+`}`+"\n"+
			`{"type":"message","id":"oa1","timestamp":"2026-11-23T07:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795417202000,"usage":{"input":1,"output":1},"content":[{"type":"text","text":"orphan"}]}}`+"\n",
	)

	// Build two-org routing state: org-a collects repo-a (restrictive policy), org-b collects repo-b.
	stA := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", false, false)
	stB := singleTeamRoutingState(now, "acc1", "org-b", "team-b", "github.com/org-b/repo-b", false, false)
	st := stA
	st.Teams = append(st.Teams, stB.Teams...)

	reg := builtin.Agents("")
	var ompSrc agents.HistorySource
	for _, s := range reg.With[agents.HistorySource]() {
		if s.SourceSystem() == "omp" {
			ompSrc = s
			break
		}
	}
	if ompSrc == nil {
		t.Fatal("builtin registry did not expose omp HistorySource")
	}
	filter := relay.NewContentFilter(reg.With[shape.Capturer]())

	held := NewHeldSet()
	held.Add("omp", "omp-held")

	type capturedReq struct {
		path string
		body []byte
	}
	var mu sync.Mutex
	var reqs []capturedReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, capturedReq{path: r.URL.Path, body: b})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	run := func(dryRun bool, stateDir string, holdings HoldingsChecker) Summary {
		t.Helper()
		sum, err := Upload(context.Background(), Options{
			StateDir: stateDir,
			Profile:  "default",
			Account:  "acc1",
			AuthURL:  "https://auth.example.com",
			Version:  "0.4.0-test",
			State:    st,
			Sources:  []agents.HistorySource{ompSrc},
			Filter:   filter,
			Holdings: holdings,
			Transport: &Transport{
				Version:     "0.4.0-test",
				HTTP:        srv.Client(),
				SignIn:      &fakeSignIn{account: "acc1", tokens: []string{"tok1"}},
				BytesPerSec: 1 << 30,
			},
			EndpointFor: func(string) string { return srv.URL },
			DryRun:      dryRun,
			Now:         func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}

	// Dry run makes zero network calls (including holdings), so verify its planned byte count
	// matches a full upload with empty holdings, then run with omp-held marked held.
	dry := run(true, t.TempDir(), stubHoldings{})
	run(false, t.TempDir(), stubHoldings{})
	var allBytes int64
	for _, r := range reqs {
		allBytes += int64(len(r.body))
	}
	var plannedBytes int64
	for _, p := range dry.Planned {
		plannedBytes += p.EstimatedBytes
	}
	if plannedBytes != allBytes {
		t.Errorf("dry-run planned %d bytes != full upload %d bytes", plannedBytes, allBytes)
	}

	reqs = nil
	stateDir := t.TempDir()
	sum := run(false, stateDir, stubHoldings{heldByTeam: map[string]HeldSet{"team-a": held}})

	var traceSpans []*tracepb.Span
	var traceResources []*resourcepb.Resource
	var logRecords []*logspb.LogRecord
	for _, r := range reqs {
		if strings.Contains(string(r.body), "SECRET-") {
			t.Fatalf("restrictive policy leaked SECRET content on %s", r.path)
		}
		switch r.path {
		case otlpTracesPath:
			var td tracepb.TracesData
			if err := proto.Unmarshal(r.body, &td); err != nil {
				t.Fatalf("unmarshal traces: %v", err)
			}
			for _, rs := range td.GetResourceSpans() {
				traceResources = append(traceResources, rs.GetResource())
				for _, ss := range rs.GetScopeSpans() {
					traceSpans = append(traceSpans, ss.GetSpans()...)
				}
			}
		case otlpLogsPath:
			var ld logspb.LogsData
			if err := proto.Unmarshal(r.body, &ld); err != nil {
				t.Fatalf("unmarshal logs: %v", err)
			}
			for _, rl := range ld.GetResourceLogs() {
				for _, sl := range rl.GetScopeLogs() {
					logRecords = append(logRecords, sl.GetLogRecords()...)
				}
			}
		default:
			t.Errorf("unexpected ingest path %q", r.path)
		}
	}

	if sum.SkippedSessions[SkipAlreadyHeld] != 1 {
		t.Errorf("SkippedSessions[already_held] = %d, want 1 (omp-held)", sum.SkippedSessions[SkipAlreadyHeld])
	}
	if sum.SentSessions != 2 {
		t.Errorf("SentSessions = %d, want 2 (omp-clean + early part of omp-mixed)", sum.SentSessions)
	}
	if sum.SkippedSessions[SkipMixed] == 0 || sum.WithheldRecords == 0 {
		t.Errorf("expected Mixed skips and withheld records for omp-mixed and omp-orphan, got %+v", sum)
	}

	// Verify spans: omp-clean (chat + tool) + omp-clean-sub (chat) + omp-mixed early (chat) = 4 spans;
	// omp-held, omp-held-sub, omp-mixed cross-org turn, and omp-orphan must NOT appear.
	var spanConvIDs []string
	for _, sp := range traceSpans {
		spanConvIDs = append(spanConvIDs, spanAttrString(sp, "gen_ai.conversation.id"))
		if !hasHistoricalBool(sp.GetAttributes()) {
			t.Errorf("span missing terma.session.historical=true")
		}
	}
	for _, forbidden := range []string{"omp-held", "omp-held-sub", "omp-orphan"} {
		if slices.Contains(spanConvIDs, forbidden) {
			t.Errorf("forbidden session %q appeared in uploaded traces: %v", forbidden, spanConvIDs)
		}
	}
	if len(traceSpans) != 4 {
		t.Errorf("uploaded %d trace spans (%v), want 4", len(traceSpans), spanConvIDs)
	}
	foundSubParent := false
	for _, res := range traceResources {
		if !hasHistoricalBool(res.GetAttributes()) {
			t.Errorf("trace resource missing terma.session.historical=true")
		}
		for _, kv := range res.GetAttributes() {
			if kv.GetKey() == semconv.TermaSessionParentIDKey && kv.GetValue().GetStringValue() == "omp-clean" {
				foundSubParent = true
			}
		}
	}
	if !foundSubParent {
		t.Errorf("subagent trace resource missing %s=omp-clean", semconv.TermaSessionParentIDKey)
	}

	// Verify supplemental logs: terma.session.title withheld (prompts=false),
	// terma.session.start (subagent) and terma.files.touched (normalized) delivered.
	var gotStart, gotTouch bool
	wantCheckoutSum := sha256.Sum256([]byte(repoA))
	wantCheckoutID := hex.EncodeToString(wantCheckoutSum[:16])
	for _, lr := range logRecords {
		switch lr.GetEventName() {
		case semconv.TermaSessionTitleEvent:
			t.Errorf("terma.session.title should be withheld when prompts=false")
		case semconv.TermaSessionStartEvent:
			gotStart = true
			if logRecordAttrString(lr, semconv.TermaSessionParentIDKey) != "omp-clean" {
				t.Errorf("session.start parent = %q, want omp-clean", logRecordAttrString(lr, semconv.TermaSessionParentIDKey))
			}
		case semconv.TermaFilesTouchedEvent:
			gotTouch = true
			if got := logRecordAttrString(lr, semconv.TermaCheckoutIDKey); got != wantCheckoutID {
				t.Errorf("files.touched checkout.id = %q, want %q", got, wantCheckoutID)
			}
			if got := logRecordAttrString(lr, semconv.VCSRepositoryURLFullKey); got != "https://github.com/org-a/repo-a" {
				t.Errorf("files.touched repo url = %q", got)
			}
			var paths []string
			for _, kv := range lr.GetAttributes() {
				if kv.GetKey() == semconv.TermaFilesPathsKey {
					for _, v := range kv.GetValue().GetArrayValue().GetValues() {
						paths = append(paths, v.GetStringValue())
					}
				}
			}
			if !slices.Equal(paths, []string{"app.go"}) {
				t.Errorf("files.touched paths = %v, want [app.go]", paths)
			}
		}
	}
	if !gotStart || !gotTouch {
		t.Errorf("supplemental logs missing expected records: gotStart=%v gotTouch=%v", gotStart, gotTouch)
	}

	// Outbox & spool isolation: history upload never touches relay/outbox-v2/ or spool/.
	for _, rel := range []string{filepath.Join("relay", "outbox-v2"), "spool"} {
		if _, err := os.Stat(filepath.Join(stateDir, rel)); !os.IsNotExist(err) {
			t.Errorf("history upload created %s (must stay isolated from live queues)", rel)
		}
	}
}

func hasHistoricalBool(attrs []*commonpb.KeyValue) bool {
	for _, kv := range attrs {
		if kv.GetKey() == semconv.TermaSessionHistoricalKey {
			b, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_BoolValue)
			return ok && b.BoolValue
		}
	}
	return false
}

func mustJSONTest(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestOmpCouncilEndToEndSecurityAndWorktreeAttribution(t *testing.T) {
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	repoB := initTestGitRepo(t, "https://github.com/org-b/repo-b.git")
	// Commit a file in repoA and create a sibling linked worktree + a nested repoB checkout.
	if err := os.WriteFile(filepath.Join(repoA, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit(repoA, "add", "main.go")
	runGit(repoA, "commit", "-q", "-m", "init")
	wtDir := filepath.Join(t.TempDir(), "wt-a")
	runGit(repoA, "worktree", "add", "-q", "-b", "feat", wtDir)
	nestedB := filepath.Join(repoA, "nested-b")
	if err := os.MkdirAll(nestedB, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(nestedB, "init", "-q")
	runGit(nestedB, "remote", "add", "origin", "https://github.com/org-b/nested-b.git")
	_ = os.WriteFile(filepath.Join(nestedB, "inner.go"), []byte("package inner\n"), 0o600)
	_ = os.WriteFile(filepath.Join(repoB, "secret.txt"), []byte("TOP-SECRET-ORGB\n"), 0o600)

	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-48 * time.Hour)
	projDir := filepath.Join(agentDir, "sessions", "--repo-a--")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSess := func(rel, content string) string {
		t.Helper()
		full := filepath.Join(projDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(full, oldTime, oldTime)
		return full
	}

	// Case 1: Neon blocker #1 + Astra blocker #3: assistant msg with long duration=15000ms
	// (no completedAt) reads repoB at 10:00:10Z -> toolResult at 10:00:11Z, reply at 10:00:13Z,
	// AND rewritten header.title from post-crossing rename MUST all be withheld.
	writeSess("2026-11-23T10-00-00-000Z_s-dur.jsonl", strings.Join([]string{
		`{"type":"title","title":"LEAKED-ORGB-TITLE","updatedAt":"2026-11-23T10:00:14Z"}`,
		`{"type":"session","version":3,"id":"s-dur","timestamp":"2026-11-23T10:00:00Z","cwd":` + mustJSONTest(repoA) + `,"title":"LEAKED-ORGB-TITLE"}`,
		`{"type":"message","id":"u0","timestamp":"2026-11-23T10:00:01Z","message":{"role":"user","content":"early safe","timestamp":1795428001000}}`,
		`{"type":"message","id":"a0","timestamp":"2026-11-23T10:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","duration":500,"timestamp":1795428002000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"safe reply"}]}}`,
		`{"type":"message","id":"u1","timestamp":"2026-11-23T10:00:09Z","message":{"role":"user","content":"read b","timestamp":1795428009000}}`,
		`{"type":"message","id":"a1","timestamp":"2026-11-23T10:00:10Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":15000,"timestamp":1795428010000,"usage":{"input":5,"output":5},"content":[{"type":"toolCall","id":"c_b","name":"bash","arguments":{"command":` + mustJSONTest("cat "+filepath.Join(repoB, "secret.txt")) + `}}]}}`,
		`{"type":"message","id":"tr1","timestamp":"2026-11-23T10:00:11Z","message":{"role":"toolResult","toolCallId":"c_b","toolName":"bash","content":[{"type":"text","text":"LEAKED-ORGB-TOOL-OUTPUT"}],"details":{"cwd":` + mustJSONTest(repoA) + `},"isError":false,"timestamp":1795428011000}}`,
		`{"type":"message","id":"a2","timestamp":"2026-11-23T10:00:13Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","duration":1000,"timestamp":1795428013000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-ORGB-REPLY"}]}}`,
		`{"type":"title_change","id":"tc1","timestamp":"2026-11-23T10:00:14Z","title":"LEAKED-ORGB-TITLE"}`,
	}, "\n")+"\n")

	// Case 2: Astra blocker #1: moved session with previousSessionFiles fails closed completely.
	writeSess("2026-11-23T09-00-00-000Z_s-moved.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-moved","timestamp":"2026-11-23T09:00:00Z","cwd":` + mustJSONTest(repoA) + `,"previousSessionFiles":["/old/org-b/s-moved.jsonl"]}`,
		`{"type":"message","id":"mu1","timestamp":"2026-11-23T09:00:01Z","message":{"role":"user","content":"LEAKED-MOVED-PROMPT","timestamp":1795424401000}}`,
		`{"type":"message","id":"ma1","timestamp":"2026-11-23T09:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795424402000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-MOVED-REPLY"}]}}`,
	}, "\n")+"\n")

	// Case 3: Astra blocker #4: parent in repoA spawns child in repoB at 08:00:02Z, child's
	// first chat isn't until 08:00:10Z -> child's terma.session.start (08:00:02Z) AND parent
	// activity at 08:00:05Z must BOTH be cut at 08:00:02Z.
	crossParent := writeSess("2026-11-23T08-00-00-000Z_s-cross-sub.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-cross-sub","timestamp":"2026-11-23T08:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"pu1","timestamp":"2026-11-23T08:00:05Z","message":{"role":"user","content":"LEAKED-BETWEEN","timestamp":1795420805000}}`,
		`{"type":"message","id":"pa1","timestamp":"2026-11-23T08:00:06Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795420806000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-BETWEEN-REPLY"}]}}`,
	}, "\n")+"\n")
	writeSess(filepath.Join("2026-11-23T08-00-00-000Z_s-cross-sub", "0-b.jsonl"), strings.Join([]string{
		`{"type":"session","version":3,"id":"s-cross-child","timestamp":"2026-11-23T08:00:02Z","cwd":` + mustJSONTest(repoB) + `,"parentSession":` + mustJSONTest(crossParent) + `}`,
		`{"type":"message","id":"ca1","timestamp":"2026-11-23T08:00:10Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795420810000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-CHILD"}]}}`,
	}, "\n")+"\n")

	// Case 4: Astra should #2 (fork with copied stale subagent) + Astra should #4 (linked
	// worktree edit attributed to worktree checkout ID, nested repoB edit dropped).
	writeSess("2026-11-23T07-00-00-000Z_s-wt.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-wt","timestamp":"2026-11-23T07:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"wu1","timestamp":"2026-11-23T07:00:01Z","message":{"role":"user","content":"edit wt","timestamp":1795417201000}}`,
		`{"type":"message","id":"wa1","timestamp":"2026-11-23T07:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","timestamp":1795417202000,"usage":{"input":5,"output":5},"content":[{"type":"toolCall","id":"wt_edit","name":"write","arguments":{"path":` + mustJSONTest(filepath.Join(wtDir, "main.go")) + `,"content":"package main\n"}}]}}`,
		`{"type":"message","id":"wtr1","timestamp":"2026-11-23T07:00:03Z","message":{"role":"toolResult","toolCallId":"wt_edit","toolName":"write","content":[{"type":"text","text":"ok"}],"isError":false,"timestamp":1795417203000}}`,
	}, "\n")+"\n")
	// Copied stale subagent pointing at an old parent path must NOT cause SkipActive!
	writeSess(filepath.Join("2026-11-23T07-00-00-000Z_s-wt", "0-copied.jsonl"),
		`{"type":"session","version":3,"id":"s-wt-oldsub","timestamp":"2026-11-22T07:00:00Z","cwd":`+mustJSONTest(repoA)+`,"parentSession":"/old/parent.jsonl"}`+"\n",
	)

	// Verify normalizeFilesTouched drops nested repoB checkout while keeping linked worktree.
	stA := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	stB := singleTeamRoutingState(now, "acc1", "org-b", "team-b", "github.com/org-b/repo-b", true, true)
	st := stA
	st.Teams = append(st.Teams, stB.Teams...)

	reg := builtin.Agents("")
	var ompSrc agents.HistorySource
	for _, s := range reg.With[agents.HistorySource]() {
		if s.SourceSystem() == "omp" {
			ompSrc = s
			break
		}
	}
	var mu sync.Mutex
	var wirePayloads [][]byte
	var logRecords []*logspb.LogRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		wirePayloads = append(wirePayloads, b)
		if r.URL.Path == otlpLogsPath {
			var ld logspb.LogsData
			if proto.Unmarshal(b, &ld) == nil {
				for _, rl := range ld.GetResourceLogs() {
					for _, sl := range rl.GetScopeLogs() {
						logRecords = append(logRecords, sl.GetLogRecords()...)
					}
				}
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sum, err := Upload(context.Background(), Options{
		StateDir: t.TempDir(), Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "0.4.0-test",
		State: st, Sources: []agents.HistorySource{ompSrc}, Filter: relay.NewContentFilter(reg.With[shape.Capturer]()),
		Holdings: stubHoldings{},
		Transport: &Transport{
			Version: "0.4.0-test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok1"}}, BytesPerSec: 1 << 30,
		},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range wirePayloads {
		if strings.Contains(string(b), "LEAKED-") {
			t.Fatalf("cross-org leaked data found in uploaded payload: %q", string(b))
		}
	}
	if sum.SkippedSessions[SkipActive] != 0 {
		t.Errorf("copied stale subagent caused SkipActive=%d, want 0", sum.SkippedSessions[SkipActive])
	}
	// s-wt must upload with terma.files.touched stamped to wtDir's CheckoutID (not repoA's).
	wantWTID := hookrun.CheckoutID(wtDir)
	foundWTTouch := false
	for _, lr := range logRecords {
		if lr.GetEventName() == semconv.TermaSessionStartEvent && logRecordAttrString(lr, semconv.SessionIDKey) == "s-cross-child" {
			t.Errorf("cross-org child terma.session.start should have been withheld at child header time")
		}
		if lr.GetEventName() == semconv.TermaFilesTouchedEvent && logRecordAttrString(lr, semconv.GenAIToolCallIDKey) == "wt_edit" {
			foundWTTouch = true
			if got := logRecordAttrString(lr, semconv.TermaCheckoutIDKey); got != wantWTID {
				t.Errorf("linked worktree touch checkout.id = %q, want %q (wtDir)", got, wantWTID)
			}
		}
	}
	if !foundWTTouch {
		t.Errorf("missing linked worktree terma.files.touched record")
	}

	// Direct check: normalizeFilesTouched drops nested checkout of another repo.
	nestedLR := &logspb.LogRecord{
		EventName: semconv.TermaFilesTouchedEvent,
		Attributes: []*commonpb.KeyValue{
			{Key: semconv.TermaFilesPathsKey, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{
				Values: []*commonpb.AnyValue{{Value: &commonpb.AnyValue_StringValue{StringValue: filepath.Join(nestedB, "inner.go")}}},
			}}}},
		},
	}
	if got := normalizeFilesTouched(nestedLR, relay.CheckoutInfo{Root: repoA, Cwd: repoA, Origin: "github.com/org-a/repo-a"}); len(got) != 0 {
		t.Errorf("nested checkout of another repo should be dropped, got %d records", len(got))
	}
}

func TestOmpCouncilR2TildeFileMentionForkFromAndUndatedTitle(t *testing.T) {
	// Put repoB inside a temporary HOME so ~/repo-b resolves to an org-B checkout.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	repoB := filepath.Join(fakeHome, "repo-b")
	if err := os.MkdirAll(repoB, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit(repoB, "init", "-q")
	runGit(repoB, "remote", "add", "origin", "https://github.com/org-b/repo-b.git")
	_ = os.WriteFile(filepath.Join(repoB, "plan.md"), []byte("SECRET-B\n"), 0o600)

	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-48 * time.Hour)
	projDir := filepath.Join(agentDir, "sessions", "--r2--")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSess := func(rel, content string) string {
		t.Helper()
		full := filepath.Join(projDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(full, oldTime, oldTime)
		return full
	}

	// 1. bash with cwd: "~/repo-b" must trigger cross-org Mixed cutoff end-to-end.
	writeSess("2026-11-23T10-00-00-000Z_s-tilde.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-tilde","timestamp":"2026-11-23T10:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"tu1","timestamp":"2026-11-23T10:00:01Z","message":{"role":"user","content":"check b","timestamp":1795428001000}}`,
		`{"type":"message","id":"ta1","timestamp":"2026-11-23T10:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":200,"timestamp":1795428001800,"usage":{"input":5,"output":5},"content":[{"type":"toolCall","id":"tb1","name":"bash","arguments":{"cwd":"~/repo-b","command":"git status"}}]}}`,
		`{"type":"message","id":"ta2","timestamp":"2026-11-23T10:00:03Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","duration":100,"timestamp":1795428002900,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-TILDE"}]}}`,
	}, "\n")+"\n")

	// 2. @file mention (role: "fileMention") referencing repoB must trigger Mixed cutoff.
	writeSess("2026-11-23T09-00-00-000Z_s-mention.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-mention","timestamp":"2026-11-23T09:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"fm1","timestamp":"2026-11-23T09:00:01Z","message":{"role":"fileMention","files":[{"path":` + mustJSONTest(filepath.Join(repoB, "plan.md")) + `}],"timestamp":1795424401000}}`,
		`{"type":"message","id":"mu1","timestamp":"2026-11-23T09:00:02Z","message":{"role":"user","content":"summarize","timestamp":1795424402000}}`,
		`{"type":"message","id":"ma1","timestamp":"2026-11-23T09:00:03Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795424403000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-MENTION"}]}}`,
	}, "\n")+"\n")

	// 3. Cross-org forkFrom: source session in repoB forked into repoA must be withheld.
	writeSess("2026-11-23T08-00-00-000Z_src-b.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"src-b","timestamp":"2026-11-23T08:00:00Z","cwd":` + mustJSONTest(repoB) + `}`,
		`{"type":"message","id":"bu1","timestamp":"2026-11-23T08:00:01Z","message":{"role":"user","content":"b work","timestamp":1795420801000}}`,
		`{"type":"message","id":"ba1","timestamp":"2026-11-23T08:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795420802000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"b done"}]}}`,
	}, "\n")+"\n")
	writeSess("2026-11-23T08-05-00-000Z_fork-into-a.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"fork-into-a","timestamp":"2026-11-23T08:05:00Z","cwd":` + mustJSONTest(repoA) + `,"parentSession":"src-b","title":"LEAKED-FORK-TITLE"}`,
		`{"type":"message","id":"fu1","timestamp":"2026-11-23T08:05:01Z","message":{"role":"user","content":"continue in a","timestamp":1795421101000}}`,
		`{"type":"message","id":"fa1","timestamp":"2026-11-23T08:05:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795421102000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-FORK-REPLY"}]}}`,
	}, "\n")+"\n")

	// 4. Undated fallback header.title on parent when child crosses AFTER parent's last entry.
	parentUndated := writeSess("2026-11-23T07-00-00-000Z_s-undated.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-undated","timestamp":"2026-11-23T07:00:00Z","cwd":` + mustJSONTest(repoA) + `,"title":"LEAKED-UNDATED-TITLE"}`,
		`{"type":"message","id":"uu1","timestamp":"2026-11-23T07:00:01Z","message":{"role":"user","content":"early","timestamp":1795417201000}}`,
		`{"type":"message","id":"ua1","timestamp":"2026-11-23T07:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795417202000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"safe early"}]}}`,
	}, "\n")+"\n")
	writeSess(filepath.Join("2026-11-23T07-00-00-000Z_s-undated", "0-later.jsonl"),
		`{"type":"session","version":3,"id":"s-undated-sub","timestamp":"2026-11-23T07:05:00Z","cwd":`+mustJSONTest(repoB)+`,"parentSession":`+mustJSONTest(parentUndated)+`}`+"\n",
	)

	// 5. Assistant span starting before a crossing (10:00:00) and finishing after (10:00:10)
	// must straddle MixedAt (10:00:05) and be dropped.
	straddleParent := writeSess("2026-11-23T06-00-00-000Z_s-straddle.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-straddle","timestamp":"2026-11-23T06:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"su1","timestamp":"2026-11-23T06:00:01Z","message":{"role":"user","content":"run","timestamp":1795413601000}}`,
		// Starts at 06:00:01Z, finishes at 06:00:11Z (duration=10000ms), while child crosses at 06:00:05Z.
		`{"type":"message","id":"sa1","timestamp":"2026-11-23T06:00:11Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","duration":10000,"timestamp":1795413601000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-STRADDLE"}]}}`,
	}, "\n")+"\n")
	writeSess(filepath.Join("2026-11-23T06-00-00-000Z_s-straddle", "0-mid.jsonl"),
		`{"type":"session","version":3,"id":"s-straddle-sub","timestamp":"2026-11-23T06:00:05Z","cwd":`+mustJSONTest(repoB)+`,"parentSession":`+mustJSONTest(straddleParent)+`}`+"\n",
	)

	stA := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	stB := singleTeamRoutingState(now, "acc1", "org-b", "team-b", "github.com/org-b/repo-b", true, true)
	st := stA
	st.Teams = append(st.Teams, stB.Teams...)

	reg := builtin.Agents("")
	var ompSrc agents.HistorySource
	for _, s := range reg.With[agents.HistorySource]() {
		if s.SourceSystem() == "omp" {
			ompSrc = s
			break
		}
	}
	var mu sync.Mutex
	var payloads [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		payloads = append(payloads, b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err := Upload(context.Background(), Options{
		StateDir: t.TempDir(), Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "0.4.0-test",
		State: st, Sources: []agents.HistorySource{ompSrc}, Filter: relay.NewContentFilter(reg.With[shape.Capturer]()),
		Holdings: stubHoldings{},
		Transport: &Transport{
			Version: "0.4.0-test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok1"}}, BytesPerSec: 1 << 30,
		},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range payloads {
		if strings.Contains(string(b), "LEAKED-") {
			t.Fatalf("Round 2 security regression: leaked data in uploaded payload: %q", string(b))
		}
	}
}

func TestOmpCouncilR3CommaGrepSinceAncestryOneWayForkAndUnparseableParent(t *testing.T) {
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	repoB := initTestGitRepo(t, "https://github.com/org-b/repo-b.git")
	_ = os.WriteFile(filepath.Join(repoA, "a.go"), []byte("package a\n"), 0o600)
	_ = os.WriteFile(filepath.Join(repoB, "b.go"), []byte("package b\n"), 0o600)

	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	since := now.Add(-7 * 24 * time.Hour)
	projDir := filepath.Join(agentDir, "sessions", "--r3--")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSess := func(name, content string, mtime time.Time) {
		t.Helper()
		full := filepath.Join(projDir, name)
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(full, mtime, mtime)
	}
	inWindow := now.Add(-2 * 24 * time.Hour)
	outWindow := now.Add(-14 * 24 * time.Hour)

	// 1. Comma-separated grep path (".,<repoB>") from repoA must trigger cross-org Mixed cutoff.
	writeSess("2026-11-23T10-00-00-000Z_s-comma.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-comma","timestamp":"2026-11-23T10:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"cu1","timestamp":"2026-11-23T10:00:01Z","message":{"role":"user","content":"search","timestamp":1795428001000}}`,
		`{"type":"message","id":"ca1","timestamp":"2026-11-23T10:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":100,"timestamp":1795428001900,"usage":{"input":5,"output":5},"content":[{"type":"toolCall","id":"cg1","name":"grep","arguments":{"pattern":"package","path":` + mustJSONTest(".,"+repoB) + `}}]}}`,
		`{"type":"message","id":"ctr1","timestamp":"2026-11-23T10:00:03Z","message":{"role":"toolResult","toolCallId":"cg1","toolName":"grep","content":[{"type":"text","text":"LEAKED-COMMA-GREP"}],"isError":false,"timestamp":1795428003000}}`,
	}, "\n")+"\n", inWindow)

	// 2. 2-hop ancestry across --since: gp-b (Org B, old) -> mid-a (Org A, old/OutsideSince) -> leaf-a (Org A, in window).
	writeSess("2026-11-10T10-00-00-000Z_gp-b.jsonl",
		`{"type":"session","version":3,"id":"gp-b","timestamp":"2026-11-10T10:00:00Z","cwd":`+mustJSONTest(repoB)+`}`+"\n"+
			`{"type":"message","id":"ga1","timestamp":"2026-11-10T10:00:01Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1794304801000,"usage":{"input":1,"output":1},"content":[{"type":"text","text":"b"}]}}`+"\n",
		outWindow)
	writeSess("2026-11-11T10-00-00-000Z_mid-a.jsonl",
		`{"type":"session","version":3,"id":"mid-a","timestamp":"2026-11-11T10:00:00Z","cwd":`+mustJSONTest(repoA)+`,"parentSession":"gp-b"}`+"\n",
		outWindow)
	writeSess("2026-11-23T09-00-00-000Z_leaf-a.jsonl",
		`{"type":"session","version":3,"id":"leaf-a","timestamp":"2026-11-23T09:00:00Z","cwd":`+mustJSONTest(repoA)+`,"continuationOf":"mid-a"}`+"\n"+
			`{"type":"message","id":"la1","timestamp":"2026-11-23T09:00:01Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795424401000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-2HOP-SINCE"}]}}`+"\n",
		inWindow)

	// 3. One-way fork: clean parent orig-a (Org A) forked into cross-fork-b (Org B) at 08:05,
	// then orig-a continues with a clean post-fork reply at 08:10 -> orig-a MUST still upload,
	// while cross-fork-b MUST be withheld.
	writeSess("2026-11-23T08-00-00-000Z_orig-a.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"orig-a","timestamp":"2026-11-23T08:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"ou1","timestamp":"2026-11-23T08:00:01Z","message":{"role":"user","content":"before fork","timestamp":1795420801000}}`,
		`{"type":"message","id":"oa1","timestamp":"2026-11-23T08:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795420802000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"CLEAN-BEFORE"}]}}`,
		`{"type":"message","id":"ou2","timestamp":"2026-11-23T08:10:00Z","message":{"role":"user","content":"after fork","timestamp":1795421400000}}`,
		`{"type":"message","id":"oa2","timestamp":"2026-11-23T08:10:01Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795421401000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"CLEAN-POST-FORK-PARENT"}]}}`,
	}, "\n")+"\n", inWindow)
	writeSess("2026-11-23T08-05-00-000Z_cross-fork-b.jsonl",
		`{"type":"session","version":3,"id":"cross-fork-b","timestamp":"2026-11-23T08:05:00Z","cwd":`+mustJSONTest(repoB)+`,"parentSession":"orig-a"}`+"\n"+
			`{"type":"message","id":"fb1","timestamp":"2026-11-23T08:05:01Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795421101000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-CROSS-FORK"}]}}`+"\n",
		inWindow)

	// 4. Unparseable non-empty parentSession path must fail closed (not fail open as rootless).
	writeSess("2026-11-23T07-00-00-000Z_bad-parent.jsonl",
		`{"type":"session","version":3,"id":"bad-parent","timestamp":"2026-11-23T07:00:00Z","cwd":`+mustJSONTest(repoA)+`,"parentSession":"/nonexistent/custom-name.jsonl"}`+"\n"+
			`{"type":"message","id":"bp1","timestamp":"2026-11-23T07:00:01Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795417201000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-UNPARSEABLE-PARENT"}]}}`+"\n",
		inWindow)

	stA := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	stB := singleTeamRoutingState(now, "acc1", "org-b", "team-b", "github.com/org-b/repo-b", true, true)
	st := stA
	st.Teams = append(st.Teams, stB.Teams...)

	reg := builtin.Agents("")
	var ompSrc agents.HistorySource
	for _, s := range reg.With[agents.HistorySource]() {
		if s.SourceSystem() == "omp" {
			ompSrc = s
			break
		}
	}
	var mu sync.Mutex
	var payloads [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		payloads = append(payloads, b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err := Upload(context.Background(), Options{
		StateDir: t.TempDir(), Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "0.4.0-test",
		State: st, Sources: []agents.HistorySource{ompSrc}, Filter: relay.NewContentFilter(reg.With[shape.Capturer]()),
		Holdings: stubHoldings{},
		Transport: &Transport{
			Version: "0.4.0-test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok1"}}, BytesPerSec: 1 << 30,
		},
		EndpointFor: func(string) string { return srv.URL },
		Since:       since,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	foundCleanPostFork := false
	for _, b := range payloads {
		s := string(b)
		if strings.Contains(s, "LEAKED-") {
			t.Fatalf("Round 3 security regression: leaked data in uploaded payload: %q", s)
		}
		if strings.Contains(s, "CLEAN-POST-FORK-PARENT") {
			foundCleanPostFork = true
		}
	}
	if !foundCleanPostFork {
		t.Errorf("clean post-fork reply on original parent orig-a was wrongly withheld by cross-org fork")
	}
}

func TestOmpCouncilR4FileURLSelectorsMissingIntermediateAndPostForkParent(t *testing.T) {
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	repoB := initTestGitRepo(t, "https://github.com/org-b/repo-b.git")
	_ = os.WriteFile(filepath.Join(repoA, "local.txt"), []byte("local a\nline 2\n"), 0o600)
	_ = os.WriteFile(filepath.Join(repoB, "b.txt"), []byte("secret b\nline 2\n"), 0o600)

	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-48 * time.Hour)
	projDir := filepath.Join(agentDir, "sessions", "--r4--")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSess := func(rel, content string) string {
		t.Helper()
		full := filepath.Join(projDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(full, oldTime, oldTime)
		return full
	}

	// 1. Local file:// URL in grep must decode and trigger Mixed cutoff.
	bFileURL := "file://" + filepath.ToSlash(filepath.Join(repoB, "b.txt"))
	writeSess("2026-11-23T10-00-00-000Z_s-fileurl.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-fileurl","timestamp":"2026-11-23T10:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"fu1","timestamp":"2026-11-23T10:00:01Z","message":{"role":"user","content":"grep file url","timestamp":1795428001000}}`,
		`{"type":"message","id":"fa1","timestamp":"2026-11-23T10:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":100,"timestamp":1795428001900,"usage":{"input":5,"output":5},"content":[{"type":"toolCall","id":"fg1","name":"grep","arguments":{"pattern":"secret","path":` + mustJSONTest(bFileURL) + `}}]}}`,
		`{"type":"message","id":"ftr1","timestamp":"2026-11-23T10:00:03Z","message":{"role":"toolResult","toolCallId":"fg1","toolName":"grep","content":[{"type":"text","text":"LEAKED-FILE-URL-MATCH"}],"isError":false,"timestamp":1795428003000}}`,
		`{"type":"message","id":"fa2","timestamp":"2026-11-23T10:00:04Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795428004000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-FILE-URL-REPLY"}]}}`,
	}, "\n")+"\n")

	// 2. Whitespace-separated grep paths with line selectors ("local.txt:1-2 <repoB>/b.txt:1-2") must split and withhold.
	wsPath := "local.txt:1-2 " + filepath.Join(repoB, "b.txt") + ":1-2"
	writeSess("2026-11-23T09-00-00-000Z_s-ws-sel.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-ws-sel","timestamp":"2026-11-23T09:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"wu1","timestamp":"2026-11-23T09:00:01Z","message":{"role":"user","content":"grep ws sel","timestamp":1795424401000}}`,
		`{"type":"message","id":"wa1","timestamp":"2026-11-23T09:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":100,"timestamp":1795424401900,"usage":{"input":5,"output":5},"content":[{"type":"toolCall","id":"wg1","name":"grep","arguments":{"pattern":"line","path":` + mustJSONTest(wsPath) + `}}]}}`,
		`{"type":"message","id":"wtr1","timestamp":"2026-11-23T09:00:03Z","message":{"role":"toolResult","toolCallId":"wg1","toolName":"grep","content":[{"type":"text","text":"LEAKED-WS-SEL-MATCH"}],"isError":false,"timestamp":1795424403000}}`,
		`{"type":"message","id":"wa2","timestamp":"2026-11-23T09:00:04Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795424404000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-WS-SEL-REPLY"}]}}`,
	}, "\n")+"\n")

	// 3. Deleted intermediate subagent (parent/0-worker.jsonl deleted, parent/0-worker/0-helper.jsonl survives)
	// must fail closed on the parent session.
	writeSess("2026-11-23T08-00-00-000Z_s-gap.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-gap","timestamp":"2026-11-23T08:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"gu1","timestamp":"2026-11-23T08:00:01Z","message":{"role":"user","content":"delegate","timestamp":1795420801000}}`,
		`{"type":"message","id":"ga1","timestamp":"2026-11-23T08:00:05Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795420805000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-MISSING-MID-SUBAGENT"}]}}`,
	}, "\n")+"\n")
	writeSess(filepath.Join("2026-11-23T08-00-00-000Z_s-gap", "0-worker", "0-helper.jsonl"),
		`{"type":"session","version":3,"id":"s-helper","timestamp":"2026-11-23T08:00:02Z","cwd":`+mustJSONTest(repoB)+`,"parentSession":"/missing/0-worker.jsonl"}`+"\n",
	)

	// 4. Clean parent forks at 07:05, parent crosses into repoB later at 07:10, fork replies
	// cleanly in repoA at 07:15 -> fork's 07:15 reply MUST upload cleanly.
	writeSess("2026-11-23T07-00-00-000Z_parent-later-cross.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"parent-later-cross","timestamp":"2026-11-23T07:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"pu1","timestamp":"2026-11-23T07:00:01Z","message":{"role":"user","content":"clean","timestamp":1795417201000}}`,
		`{"type":"message","id":"pa1","timestamp":"2026-11-23T07:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795417202000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"message","id":"pa2","timestamp":"2026-11-23T07:10:00Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","timestamp":1795417800000,"usage":{"input":5,"output":5},"content":[{"type":"toolCall","id":"pc1","name":"read","arguments":{"path":` + mustJSONTest(filepath.Join(repoB, "b.txt")) + `}}]}}`,
	}, "\n")+"\n")
	writeSess("2026-11-23T07-05-00-000Z_fork-before-cross.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"fork-before-cross","timestamp":"2026-11-23T07:05:00Z","cwd":` + mustJSONTest(repoA) + `,"parentSession":"parent-later-cross"}`,
		`{"type":"message","id":"fk1","timestamp":"2026-11-23T07:15:00Z","message":{"role":"user","content":"clean fork","timestamp":1795418100000}}`,
		`{"type":"message","id":"fka1","timestamp":"2026-11-23T07:15:01Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795418101000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"CLEAN-FORK-AFTER-PARENT-LATER-CROSS"}]}}`,
	}, "\n")+"\n")

	// 5. OMP '@' / ':' path aliases (e.g. "@/work/org-b/plan.md") must trigger Mixed cutoff.
	writeSess("2026-11-23T06-00-00-000Z_s-at-alias.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-at-alias","timestamp":"2026-11-23T06:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"au1","timestamp":"2026-11-23T06:00:01Z","message":{"role":"user","content":"grep at alias","timestamp":1795413601000}}`,
		`{"type":"message","id":"aa1","timestamp":"2026-11-23T06:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":100,"timestamp":1795413601900,"usage":{"input":5,"output":5},"content":[{"type":"toolCall","id":"ag1","name":"grep","arguments":{"pattern":"secret","path":` + mustJSONTest("@"+filepath.Join(repoB, "b.txt")) + `}}]}}`,
		`{"type":"message","id":"atr1","timestamp":"2026-11-23T06:00:03Z","message":{"role":"toolResult","toolCallId":"ag1","toolName":"grep","content":[{"type":"text","text":"LEAKED-AT-ALIAS-MATCH"}],"isError":false,"timestamp":1795413603000}}`,
		`{"type":"message","id":"aa2","timestamp":"2026-11-23T06:00:04Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795413604000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-AT-ALIAS-REPLY"}]}}`,
	}, "\n")+"\n")

	// 6. Whitespace-separated selector grep targets remain fail-closed even after both files are deleted.
	deletedWSPath := "deleted-local.txt:1-2 " + filepath.Join(repoB, "deleted-b.txt") + ":1-2"
	writeSess("2026-11-23T05-00-00-000Z_s-ws-deleted.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-ws-deleted","timestamp":"2026-11-23T05:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"du1","timestamp":"2026-11-23T05:00:01Z","message":{"role":"user","content":"grep deleted ws","timestamp":1795410001000}}`,
		`{"type":"message","id":"da1","timestamp":"2026-11-23T05:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":100,"timestamp":1795410001900,"usage":{"input":5,"output":5},"content":[{"type":"toolCall","id":"dg1","name":"grep","arguments":{"pattern":"line","path":` + mustJSONTest(deletedWSPath) + `}}]}}`,
		`{"type":"message","id":"dtr1","timestamp":"2026-11-23T05:00:03Z","message":{"role":"toolResult","toolCallId":"dg1","toolName":"grep","content":[{"type":"text","text":"LEAKED-DELETED-WS-MATCH"}],"isError":false,"timestamp":1795410003000}}`,
		`{"type":"message","id":"da2","timestamp":"2026-11-23T05:00:04Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795410004000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"LEAKED-DELETED-WS-REPLY"}]}}`,
	}, "\n")+"\n")

	// 7. A deleted single relative filename with spaces inside repoA still uploads cleanly.
	writeSess("2026-11-23T04-00-00-000Z_s-clean-spaces.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-clean-spaces","timestamp":"2026-11-23T04:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"su1","timestamp":"2026-11-23T04:00:01Z","message":{"role":"user","content":"grep spaces","timestamp":1795406401000}}`,
		`{"type":"message","id":"sa1","timestamp":"2026-11-23T04:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":100,"timestamp":1795406401900,"usage":{"input":5,"output":5},"content":[{"type":"toolCall","id":"sg1","name":"grep","arguments":{"pattern":"note","path":"docs/my design doc.md"}}]}}`,
		`{"type":"message","id":"sa2","timestamp":"2026-11-23T04:00:04Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","timestamp":1795406404000,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"CLEAN-SPACED-FILE-IN-REPO-A"}]}}`,
	}, "\n")+"\n")

	// 8. Round 6 blockers end-to-end: ast_edit preview on repoB, semicolon-split glob.pattern,
	// Windows drive path on Linux, and @../ relative traversal all trigger Mixed cutoff.
	writeSess("2026-11-23T03-00-00-000Z_s-ast-edit.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-ast-edit","timestamp":"2026-11-23T03:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"aeu1","timestamp":"2026-11-23T03:00:01Z","message":{"role":"user","content":"ast edit","timestamp":1795402801000}}`,
		`{"type":"message","id":"aea1","timestamp":"2026-11-23T03:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":100,"timestamp":1795402801900,"usage":{"input":5,"output":5},"content":[{"type":"toolCall","id":"ae1","name":"ast_edit","arguments":{"ops":[{"pat":"x","out":"y"}],"paths":[` + mustJSONTest(filepath.Join(repoB, "b.txt")) + `]}}]}}`,
		`{"type":"message","id":"aetr1","timestamp":"2026-11-23T03:00:03Z","message":{"role":"toolResult","toolCallId":"ae1","toolName":"ast_edit","content":[{"type":"text","text":"LEAKED-AST-EDIT-DIFF"}],"isError":false,"timestamp":1795402803000}}`,
	}, "\n")+"\n")

	writeSess("2026-11-23T02-00-00-000Z_s-glob-semi.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-glob-semi","timestamp":"2026-11-23T02:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"gsu1","timestamp":"2026-11-23T02:00:01Z","message":{"role":"user","content":"glob semi","timestamp":1795399201000}}`,
		`{"type":"message","id":"gsa1","timestamp":"2026-11-23T02:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":100,"timestamp":1795399201900,"usage":{"input":5,"output":5},"content":[{"type":"toolCall","id":"gs1","name":"glob","arguments":{"pattern":` + mustJSONTest("*.go;"+filepath.Join(repoB, "*.txt")) + `}}]}}`,
		`{"type":"message","id":"gstr1","timestamp":"2026-11-23T02:00:03Z","message":{"role":"toolResult","toolCallId":"gs1","toolName":"glob","content":[{"type":"text","text":"LEAKED-GLOB-SEMI"}],"isError":false,"timestamp":1795399203000}}`,
	}, "\n")+"\n")

	writeSess("2026-11-23T01-00-00-000Z_s-win-drive.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-win-drive","timestamp":"2026-11-23T01:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"wdu1","timestamp":"2026-11-23T01:00:01Z","message":{"role":"user","content":"win drive","timestamp":1795395601000}}`,
		`{"type":"message","id":"wda1","timestamp":"2026-11-23T01:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":100,"timestamp":1795395601900,"usage":{"input":5,"output":5},"content":[{"type":"toolCall","id":"wd1","name":"grep","arguments":{"pattern":"x","path":"C:/work/org-b/file.ts"}}]}}`,
		`{"type":"message","id":"wdtr1","timestamp":"2026-11-23T01:00:03Z","message":{"role":"toolResult","toolCallId":"wd1","toolName":"grep","content":[{"type":"text","text":"LEAKED-WIN-DRIVE"}],"isError":false,"timestamp":1795395603000}}`,
	}, "\n")+"\n")

	relToB, relErr := filepath.Rel(repoA, filepath.Join(repoB, "b.txt"))
	if relErr != nil {
		t.Fatal(relErr)
	}
	writeSess("2026-11-23T00-30-00-000Z_s-at-rel.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"s-at-rel","timestamp":"2026-11-23T00:30:00Z","cwd":` + mustJSONTest(repoA) + `}`,
		`{"type":"message","id":"aru1","timestamp":"2026-11-23T00:30:01Z","message":{"role":"user","content":"at rel","timestamp":1795393801000}}`,
		`{"type":"message","id":"ara1","timestamp":"2026-11-23T00:30:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"toolUse","duration":100,"timestamp":1795393801900,"usage":{"input":5,"output":5},"content":[{"type":"toolCall","id":"ar1","name":"grep","arguments":{"pattern":"x","path":` + mustJSONTest("@"+filepath.ToSlash(relToB)) + `}}]}}`,
		`{"type":"message","id":"artr1","timestamp":"2026-11-23T00:30:03Z","message":{"role":"toolResult","toolCallId":"ar1","toolName":"grep","content":[{"type":"text","text":"LEAKED-AT-REL"}],"isError":false,"timestamp":1795393803000}}`,
	}, "\n")+"\n")

	stA := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	stB := singleTeamRoutingState(now, "acc1", "org-b", "team-b", "github.com/org-b/repo-b", true, true)
	st := stA
	st.Teams = append(st.Teams, stB.Teams...)

	reg := builtin.Agents("")
	var ompSrc agents.HistorySource
	for _, s := range reg.With[agents.HistorySource]() {
		if s.SourceSystem() == "omp" {
			ompSrc = s
			break
		}
	}
	var mu sync.Mutex
	var payloads [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		payloads = append(payloads, b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err := Upload(context.Background(), Options{
		StateDir: t.TempDir(), Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "0.4.0-test",
		State: st, Sources: []agents.HistorySource{ompSrc}, Filter: relay.NewContentFilter(reg.With[shape.Capturer]()),
		Holdings: stubHoldings{},
		Transport: &Transport{
			Version: "0.4.0-test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok1"}}, BytesPerSec: 1 << 30,
		},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	foundCleanFork := false
	foundCleanSpaces := false
	for _, b := range payloads {
		s := string(b)
		if strings.Contains(s, "LEAKED-") {
			t.Fatalf("Round 4/5 security regression: leaked data in uploaded payload: %q", s)
		}
		if strings.Contains(s, "CLEAN-FORK-AFTER-PARENT-LATER-CROSS") {
			foundCleanFork = true
		}
		if strings.Contains(s, "CLEAN-SPACED-FILE-IN-REPO-A") {
			foundCleanSpaces = true
		}
	}
	if !foundCleanFork {
		t.Errorf("clean fork split before parent crossed should still upload")
	}
	if !foundCleanSpaces {
		t.Errorf("clean session referencing deleted spaced filename in repoA should still upload")
	}
}

type countingHistorySource struct {
	agents.HistorySource
	sourceSystem string
	reads        int
	cwd          string
}

func (s *countingHistorySource) SourceSystem() string { return s.sourceSystem }
func (s *countingHistorySource) ReadHistory(_ context.Context, f agents.HistoryFile, _ agents.HistoryReadOptions, emit func(agents.HistorySegment) error) error {
	s.reads++
	return emit(agents.HistorySegment{SessionID: f.SessionID, Cwd: s.cwd})
}

func TestFamilyCutoffSharesCacheForBidirectionalFamily(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	resolver := newCwdResolver(st, "https://auth.example.com", false, "", "", now)
	src := &countingHistorySource{sourceSystem: "codex", cwd: repoA}

	files := []agents.HistoryFile{
		{SourceSystem: "codex", SessionID: "parent", Path: filepath.Join(repoA, "p.jsonl")},
		{SourceSystem: "codex", SessionID: "sub-1", ParentSessionID: "parent", Path: filepath.Join(repoA, "s1.jsonl")},
		{SourceSystem: "codex", SessionID: "sub-2", ParentSessionID: "parent", Path: filepath.Join(repoA, "s2.jsonl")},
		{SourceSystem: "codex", SessionID: "sub-3", ParentSessionID: "parent", Path: filepath.Join(repoA, "s3.jsonl")},
	}
	byID := map[string]agents.HistoryFile{}
	for _, f := range files {
		byID[f.SessionID] = f
	}
	children := buildChildrenMap(files)
	cache := map[string]familyVerdict{}
	snap := fileSnapshot{size: 1}
	opts := Options{StateDir: t.TempDir()}

	for _, f := range files {
		comps, missing := collectFamily(f, byID, children)
		if _, _, err := familyCutoff(context.Background(), opts, src, resolver, f, comps, missing, snap, cache); err != nil {
			t.Fatal(err)
		}
	}
	if src.reads != len(files) {
		t.Errorf("ReadHistory calls = %d for %d bidirectional members, want %d (shared cache)", src.reads, len(files), len(files))
	}
}
