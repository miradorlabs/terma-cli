package history

import (
	"context"
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

func TestOmpHistoryUploadEndToEndPolicyHoldingsAndOutboxIsolation(t *testing.T) {
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
	if sum.SentSessions != 1 {
		t.Errorf("SentSessions = %d, want 1 (omp-clean)", sum.SentSessions)
	}

	// Verify spans: omp-clean (chat + tool) + omp-clean-sub (chat) = 3 spans; omp-held and
	// omp-held-sub must NOT appear.
	var spanConvIDs []string
	for _, sp := range traceSpans {
		spanConvIDs = append(spanConvIDs, spanAttrString(sp, "gen_ai.conversation.id"))
		if !hasHistoricalBool(sp.GetAttributes()) {
			t.Errorf("span missing terma.session.historical=true")
		}
	}
	for _, forbidden := range []string{"omp-held", "omp-held-sub"} {
		if slices.Contains(spanConvIDs, forbidden) {
			t.Errorf("forbidden session %q appeared in uploaded traces: %v", forbidden, spanConvIDs)
		}
	}
	if len(traceSpans) != 3 {
		t.Errorf("uploaded %d trace spans (%v), want 3", len(traceSpans), spanConvIDs)
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
	wantCheckoutID := hookrun.CheckoutID(resolvedTestPath(t, repoA))
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

// An edit in a linked worktree is attributed to that worktree's checkout, an edit in a
// nested checkout of another repository is dropped, and a copied stale subagent in a fork's
// folder does not hold the session back as active.
func TestOmpWorktreeAttributionAndCopiedSubagent(t *testing.T) {
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
	if sum.SkippedSessions[SkipActive] != 0 {
		t.Errorf("copied stale subagent caused SkipActive=%d, want 0", sum.SkippedSessions[SkipActive])
	}
	// s-wt must upload with terma.files.touched stamped to wtDir's CheckoutID (not repoA's).
	wantWTID := hookrun.CheckoutID(resolvedTestPath(t, wtDir))
	foundWTTouch := false
	for _, lr := range logRecords {
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

func TestUploadOmpWithChangesJournalSidecar(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-2 * time.Hour)
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	projDir := filepath.Join(agentDir, "sessions", "--repo-a--")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAged := func(rel, content string) {
		full := filepath.Join(projDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}

	makeSess := func(id, reply string) string {
		return strings.Join([]string{
			`{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-11-23T10:00:00Z","cwd":` + mustJSONTest(repoA) + `}`,
			`{"type":"message","id":"u1","timestamp":"2026-11-23T10:00:01Z","message":{"role":"user","content":"edit file","timestamp":1795428001000}}`,
			`{"type":"message","id":"a1","timestamp":"2026-11-23T10:00:02Z","message":{"role":"assistant","model":"claude-sonnet-4-6","stopReason":"stop","duration":100,"timestamp":1795428001900,"usage":{"input":5,"output":5},"content":[{"type":"text","text":"` + reply + `"}]}}`,
		}, "\n") + "\n"
	}

	// 1. Session with normal OMP changes.jsonl sidecar.
	writeAged("2026-11-23T10-00-00-000Z_s-journal.jsonl", makeSess("s-journal", "UPLOADED-WITH-CHANGES-JOURNAL"))
	writeAged(filepath.Join("2026-11-23T10-00-00-000Z_s-journal", "changes.jsonl"),
		"{\"type\":\"change\",\"path\":\"main.go\"}\n{\"type\":\"commit\",\"sha\":\"deadbeef\"}\n")

	// 2. Session with oversized (> 1 MiB first line) changes.jsonl sidecar.
	writeAged("2026-11-23T09-00-00-000Z_s-big-journal.jsonl", makeSess("s-big-journal", "UPLOADED-WITH-BIG-CHANGES-JOURNAL"))
	writeAged(filepath.Join("2026-11-23T09-00-00-000Z_s-big-journal", "changes.jsonl"),
		`{"type":"change","diff":"`+strings.Repeat("D", (1<<20)+64)+`"}`+"\n")

	// 3. Session with freshly-started incomplete subagent transcript (must still skip as active).
	writeAged("2026-11-23T08-00-00-000Z_s-incomplete-sub.jsonl", makeSess("s-incomplete-sub", "MUST-SKIP-INCOMPLETE-SUBAGENT"))
	writeAged(filepath.Join("2026-11-23T08-00-00-000Z_s-incomplete-sub", "0-worker.jsonl"), "")

	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	reg := builtin.Agents("")
	var ompSrc agents.HistorySource
	for _, s := range reg.With[agents.HistorySource]() {
		if s.SourceSystem() == "omp" {
			ompSrc = s
			break
		}
	}

	// Verify --dry-run reports 2 sessions ready and 1 skipped as active.
	drySum, err := Upload(context.Background(), Options{
		StateDir: t.TempDir(), Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "0.4.0-test",
		State: st, Sources: []agents.HistorySource{ompSrc}, Filter: relay.NewContentFilter(reg.With[shape.Capturer]()),
		Holdings: stubHoldings{}, DryRun: true, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	plannedSessions := 0
	for _, p := range drySum.Planned {
		plannedSessions += p.Sessions
	}
	if plannedSessions != 2 || drySum.SkippedSessions[SkipActive] != 1 {
		t.Fatalf("dry-run summary = {Planned:%d Skipped:%v}, want Planned=2 Skipped[active]=1", plannedSessions, drySum.SkippedSessions)
	}

	// Verify real upload sends both changes.jsonl sessions and skips the incomplete-subagent session.
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
	if sum.SentSessions != 2 || sum.SkippedSessions[SkipActive] != 1 {
		t.Fatalf("upload summary = {SentSessions:%d Skipped:%v}, want SentSessions=2 Skipped[active]=1", sum.SentSessions, sum.SkippedSessions)
	}
	var foundNormal, foundBig bool
	for _, b := range payloads {
		s := string(b)
		if strings.Contains(s, "MUST-SKIP-INCOMPLETE-SUBAGENT") {
			t.Fatalf("incomplete subagent session must not upload: %q", s)
		}
		if strings.Contains(s, "UPLOADED-WITH-CHANGES-JOURNAL") {
			foundNormal = true
		}
		if strings.Contains(s, "UPLOADED-WITH-BIG-CHANGES-JOURNAL") {
			foundBig = true
		}
	}
	if !foundNormal || !foundBig {
		t.Errorf("missing expected uploaded sessions: normal=%v big=%v", foundNormal, foundBig)
	}
}

func resolvedTestPath(t *testing.T, p string) string {
	t.Helper()
	if realPath, err := filepath.EvalSymlinks(p); err == nil {
		return realPath
	}
	return p
}
