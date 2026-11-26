package history

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// TestHistoryUploadNeverEvictsLiveOutbox verifies that even when the shared live relay
// outbox is already backlogged (upstream returning 503), running a multi-session history
// upload never writes to the live outbox and causes zero live telemetry drops.
func TestHistoryUploadNeverEvictsLiveOutbox(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	outboxDir := filepath.Join(stateDir, "relay", "outbox-v2")
	repoDir := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")

	var liveUpstreamBlocked atomic.Bool
	liveUpstreamBlocked.Store(true)
	var liveDelivered atomic.Int32
	liveUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if liveUpstreamBlocked.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		liveDelivered.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer liveUpstream.Close()

	reg := builtin.Agents("")
	// Start a real live relay whose upstream is temporarily 503 so live parts sit in the outbox.
	r := relay.New(relay.Options{
		Dir:         outboxDir,
		Token:       "relay-tok",
		Version:     "test",
		Correlators: reg.With[shape.Correlator](),
		Capturers:   reg.With[shape.Capturer](),
		Lookup: func(sessionID string, _ time.Time) (claim.Claim, bool) {
			if sessionID == "live-sess-1" {
				return claim.Claim{ProjectID: "team-a", Tool: "test", Account: "no_person"}, true
			}
			return claim.Claim{}, false
		},
		Resolve: func(c claim.Claim) (relay.Policy, error) {
			return relay.Policy{Endpoint: liveUpstream.URL, Key: "srv-key", IncludePrompts: true, IncludeToolContent: true}, nil
		},
	})
	relayCtx := t.Context()
	go r.Run(relayCtx)
	relaySrv := httptest.NewServer(r.Handler())
	defer relaySrv.Close()

	// Push 5 live exports into the relay outbox while upstream is returning 503.
	for i := range 5 {
		liveMsg := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "live-agent"}}},
			}},
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
				TimeUnixNano: uint64(time.Now().UnixNano() + int64(i)),
				Attributes: []*commonpb.KeyValue{
					{Key: "session.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "live-sess-1"}}},
					{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "live_event"}}},
				},
			}}}},
		}}}
		wire, err := proto.Marshal(liveMsg)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, relaySrv.URL+"/v1/logs", bytes.NewReader(wire))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer relay-tok")
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := relaySrv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("live relay POST status = %d, want 200", resp.StatusCode)
		}
	}

	outboxFilesBefore := countFiles(t, outboxDir)
	if outboxFilesBefore == 0 {
		t.Fatal("expected live outbox files queued while upstream returns 503")
	}

	// Now run a history upload with sizable payloads in the same stateDir.
	var histMu sync.Mutex
	histPosts := 0
	histUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		histMu.Lock()
		histPosts++
		histMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer histUpstream.Close()

	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	bigText := strings.Repeat("H", 200<<10)
	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "hist-1", ModTime: now.Add(-48 * time.Hour)},
			{SourceSystem: "claude", SessionID: "hist-2", ModTime: now.Add(-24 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"hist-1": {makeNativeSegment("claude", "hist-1", repoDir, bigText, now.Add(-48*time.Hour))},
			"hist-2": {makeNativeSegment("claude", "hist-2", repoDir, bigText, now.Add(-24*time.Hour))},
		},
	}

	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir,
		Profile:  "default",
		Account:  "acc1",
		AuthURL:  "https://auth.example.com",
		Version:  "test",
		State:    st,
		Sources:  []agents.HistorySource{src},
		Filter:   relay.NewContentFilter(nil),
		Holdings: stubHoldings{},
		Transport: &Transport{
			Version:     "test",
			HTTP:        histUpstream.Client(),
			SignIn:      &fakeSignIn{account: "acc1", tokens: []string{"tok-1"}},
			BytesPerSec: 1 << 30,
		},
		EndpointFor: func(string) string { return histUpstream.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum.SentSessions != 2 || histPosts != 2 {
		t.Fatalf("history upload sent=%d posts=%d, want 2/2", sum.SentSessions, histPosts)
	}

	// Verify live outbox had zero evictions/drops and zero extra files written by history upload.
	outboxFilesAfter := countFiles(t, outboxDir)
	if outboxFilesAfter != outboxFilesBefore {
		t.Errorf("live outbox file count changed during history upload: before=%d after=%d", outboxFilesBefore, outboxFilesAfter)
	}
	for k, v := range r.Stats().Snapshot().Counters {
		if strings.HasPrefix(k, "dropped.") && v > 0 {
			t.Errorf("live relay dropped telemetry during history upload: %s=%d", k, v)
		}
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func singleTeamRoutingState(now time.Time, account, orgID, teamID, repo string, includePrompts, includeTool bool) routing.State {
	return routing.State{
		Account: account,
		Teams: []routing.Team{{
			EnrollmentID:   "enr-" + teamID,
			TeamID:         teamID,
			OrganizationID: orgID,
			AuthURL:        "https://auth.example.com",
			Policy: config.Policy{
				Mode:               config.ModeRepo,
				Repositories:       []string{repo},
				IncludePrompts:     includePrompts,
				IncludeToolContent: includeTool,
				OrganizationID:     orgID,
				TeamID:             teamID,
				AuthURL:            "https://auth.example.com",
				FetchedAt:          now,
			},
		}},
	}
}

func makeNativeSegment(source, sessionID, cwd, text string, ts time.Time) agents.HistorySegment {
	boolTrue := &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}
	nanos := uint64(ts.UnixNano())
	return agents.HistorySegment{
		SourceSystem: source,
		SessionID:    sessionID,
		Cwd:          cwd,
		RecordCount:  1,
		NativeLogs: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude-code"}}},
				{Key: semconv.TermaSessionHistoricalKey, Value: proto.CloneOf(boolTrue)},
			}},
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope: &commonpb.InstrumentationScope{Name: "com.anthropic.claude_code.events"},
				LogRecords: []*logspb.LogRecord{{
					TimeUnixNano:         nanos,
					ObservedTimeUnixNano: nanos,
					Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.user_prompt"}},
					Attributes: []*commonpb.KeyValue{
						{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "user_prompt"}}},
						{Key: "session.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: sessionID}}},
						{Key: "prompt", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: text}}},
						{Key: semconv.TermaSessionHistoricalKey, Value: proto.CloneOf(boolTrue)},
					},
				}},
			}},
		}}},
	}
}
