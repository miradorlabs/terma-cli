package desktoprelay

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/shim"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

const (
	sessionA = "01a0d0ff-0000-7000-8000-000000000001"
	sessionB = "01a0d0ff-0000-7000-8000-000000000002"
	projectA = "project-a"
	projectB = "project-b"
	testKey  = "ter_srv_0123456789abcdef01234567"
)

func keyValue(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func testLog(sessionID, event string) *logspb.LogRecord {
	attrs := []*commonpb.KeyValue{keyValue("event.name", event)}
	if sessionID != "" {
		attrs = append(attrs, keyValue("conversation.id", sessionID))
	}
	return &logspb.LogRecord{Attributes: attrs}
}

func testInput(logs ...*logspb.LogRecord) *collogspb.ExportLogsServiceRequest {
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{keyValue("service.name", "codex_cli_rs")}},
		ScopeLogs: []*logspb.ScopeLogs{{
			Scope:      &commonpb.InstrumentationScope{Name: "codex"},
			LogRecords: logs,
		}},
	}}}
}

func bindTestProject(t *testing.T, projectID, endpoint string, prompts, toolContent bool) {
	t.Helper()
	if err := shim.SaveRecord(shim.Record{
		ProjectID: projectID, Endpoint: endpoint, Signals: []string{"logs"},
		Harnesses: []string{shim.AgentCodex}, IncludePrompts: prompts, IncludeToolContent: toolContent,
	}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.SetFor(shim.AgentCodex, projectID, testKey); err != nil {
		t.Fatal(err)
	}
}

func TestRelaySeparatesConcurrentRepositoriesAndDropsUnroutableRecords(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	var mu sync.Mutex
	seen := map[string][]string{}
	receiver := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/logs" || r.Header.Get("Authorization") != "Bearer "+testKey {
				t.Errorf("%s: wrong destination or key", name)
			}
			data, _ := io.ReadAll(r.Body)
			var batch collogspb.ExportLogsServiceRequest
			if err := proto.Unmarshal(data, &batch); err != nil {
				t.Errorf("%s: decode: %v", name, err)
			}
			mu.Lock()
			for _, rl := range batch.GetResourceLogs() {
				if conversationID(rl.GetResource().GetAttributes()) != "" || len(rl.GetScopeLogs()) != 1 || rl.GetScopeLogs()[0].GetScope().GetName() != "codex" {
					t.Errorf("%s: resource or scope changed", name)
				}
				for _, lr := range rl.GetScopeLogs()[0].GetLogRecords() {
					seen[name] = append(seen[name], conversationID(lr.GetAttributes())+":"+eventName(lr.GetAttributes()))
				}
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
	}
	a := receiver(projectA)
	defer a.Close()
	b := receiver(projectB)
	defer b.Close()
	bindTestProject(t, projectA, a.URL, true, true)
	bindTestProject(t, projectB, b.URL, false, false)
	if err := Register(sessionA, projectA, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := Register(sessionB, projectB, time.Now()); err != nil {
		t.Fatal(err)
	}
	relay, err := New()
	if err != nil {
		t.Fatal(err)
	}
	in := testInput(
		testLog(sessionA, "codex.sse_event"),
		testLog(sessionA, "codex.user_prompt"),
		testLog(sessionA, "codex.tool_result"),
		testLog(sessionB, "codex.sse_event"),
		testLog(sessionB, "codex.user_prompt"),
		testLog(sessionB, "codex.tool_result"),
		testLog("", "codex.api_request"),
	)
	body, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/x-protobuf")
	response := httptest.NewRecorder()
	relay.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("relay response: %d %s", response.Code, response.Body.String())
	}
	count, _, err := relay.queue.Pending()
	if err != nil || count != 2 {
		t.Fatalf("pending = %d, %v; want two project batches", count, err)
	}
	if sent, err := relay.queue.Drain(context.Background(), relay.send); err != nil || sent != 2 {
		t.Fatalf("drain = %d, %v", sent, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := seen[projectA]; len(got) != 3 || got[0] != sessionA+":codex.sse_event" ||
		got[1] != sessionA+":codex.user_prompt" || got[2] != sessionA+":codex.tool_result" {
		t.Fatalf("project A received %v", got)
	}
	if got := seen[projectB]; len(got) != 1 || got[0] != sessionB+":codex.sse_event" {
		t.Fatalf("project B received %v", got)
	}
	if relay.accepted.Load() != 4 || relay.dropped.Load() != 3 {
		t.Fatalf("counts accepted=%d dropped=%d", relay.accepted.Load(), relay.dropped.Load())
	}
}

func TestConflictingSessionRegistrationsCannotLeakAcrossProjects(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	now := time.Now()
	if err := Register(sessionA, projectA, now); err != nil {
		t.Fatal(err)
	}
	if err := Register(sessionA, projectB, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := ProjectFor(sessionA, now.Add(2*time.Second)); got != "" {
		t.Fatalf("ambiguous session routed to %q", got)
	}
	if err := Register(sessionA, projectA, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := ProjectFor(sessionA, now.Add(4*time.Second)); got != "" {
		t.Fatalf("ambiguous session revived as %q", got)
	}
}

func TestRelayWaitsBrieflyForTrustedSessionStart(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	bindTestProject(t, projectA, server.URL, false, false)
	relay, err := New()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := proto.Marshal(testInput(testLog(sessionA, "codex.sse_event")))
	request := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/x-protobuf")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		relay.Handler().ServeHTTP(response, request)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	if err := Register(sessionA, projectA, time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not finish after session registration")
	}
	if response.Code != http.StatusOK || relay.accepted.Load() != 1 || relay.dropped.Load() != 0 {
		t.Fatalf("response=%d accepted=%d dropped=%d", response.Code, relay.accepted.Load(), relay.dropped.Load())
	}
}

func TestQueueRejectsMixedRequestBeforeWritingAnyProject(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	queue, err := OpenQueue()
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.EnqueueMany(map[string][]byte{projectA: []byte("valid"), projectB: nil}); err == nil {
		t.Fatal("accepted a partially invalid mixed request")
	}
	count, _, err := queue.Pending()
	if err != nil || count != 0 {
		t.Fatalf("partial mixed request left %d queued batches: %v", count, err)
	}
}

func TestQueueBacksOffFailedProjectWithoutBlockingOtherProjects(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	queue, err := OpenQueue()
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.EnqueueMany(map[string][]byte{projectA: []byte("a"), projectB: []byte("b")}); err != nil {
		t.Fatal(err)
	}
	called := map[string]int{}
	send := func(_ context.Context, projectID string, _ []byte) error {
		called[projectID]++
		if projectID == projectA && called[projectID] == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}
	if delivered, err := queue.Drain(context.Background(), send); err == nil || delivered != 1 {
		t.Fatalf("first drain = %d, %v", delivered, err)
	}
	if called[projectA] != 1 || called[projectB] != 1 {
		t.Fatalf("failed project blocked a healthy one: %v", called)
	}
	if delivered, err := queue.Drain(context.Background(), send); err != nil || delivered != 0 || called[projectA] != 1 {
		t.Fatalf("backoff drain = %d, %v; calls %v", delivered, err, called)
	}
	state := queue.retry[projectA]
	if state.delay != retryMin {
		t.Fatalf("initial retry delay = %s", state.delay)
	}
	state.until = time.Now().Add(-time.Second)
	queue.retry[projectA] = state
	if delivered, err := queue.Drain(context.Background(), send); err != nil || delivered != 1 || called[projectA] != 2 {
		t.Fatalf("retry drain = %d, %v; calls %v", delivered, err, called)
	}
}

func TestProjectThatSelectedCodexCLIOnlyDoesNotReceiveDesktopLogs(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	enabled := false
	if err := shim.SaveRecord(shim.Record{ProjectID: projectA, Endpoint: "https://otel.terma.ai", Signals: []string{"logs"},
		Harnesses: []string{shim.AgentCodex}, Desktop: &enabled}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.SetFor(shim.AgentCodex, projectA, testKey); err != nil {
		t.Fatal(err)
	}
	if err := Register(sessionA, projectA, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ReadyForProject(projectA) {
		t.Fatal("CLI-only project is ready for desktop telemetry")
	}
	outputs, accepted, dropped := splitByProject(testInput(testLog(sessionA, "codex.sse_event")), time.Now())
	if len(outputs) != 0 || accepted != 0 || dropped != 1 {
		t.Fatalf("CLI-only project was routed: %v accepted=%d dropped=%d", outputs, accepted, dropped)
	}
}
