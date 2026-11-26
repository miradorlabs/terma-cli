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
	"strings"
	"sync"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

type stubHistorySource struct {
	source   string
	files    []agents.HistoryFile
	segments map[string][]agents.HistorySegment
}

func (s *stubHistorySource) Name() string                      { return s.source }
func (s *stubHistorySource) DisplayName() string               { return s.source }
func (s *stubHistorySource) Installed(context.Context) bool    { return true }
func (s *stubHistorySource) Events() map[string]agents.Handler { return nil }
func (s *stubHistorySource) FlushAfter() []string              { return nil }
func (s *stubHistorySource) SourceSystem() string              { return s.source }
func (s *stubHistorySource) DiscoverHistory(_ context.Context, _ agents.HistoryDiscoverOptions) ([]agents.HistoryFile, error) {
	return s.files, nil
}
func (s *stubHistorySource) ReadHistory(_ context.Context, file agents.HistoryFile, _ agents.HistoryReadOptions, emit func(agents.HistorySegment) error) error {
	for _, seg := range s.segments[file.SessionID] {
		if err := emit(seg); err != nil {
			return err
		}
	}
	return nil
}

type stubHoldings struct {
	heldByTeam map[string]HeldSet
	errByTeam  map[string]error
	mu         *sync.Mutex
	calls      *map[string][]string
}

func (h stubHoldings) Check(_ context.Context, team routing.Team, sessionIDs []string) (HeldSet, error) {
	if h.mu != nil && h.calls != nil {
		h.mu.Lock()
		(*h.calls)[team.TeamID] = append((*h.calls)[team.TeamID], sessionIDs...)
		h.mu.Unlock()
	}
	if err := h.errByTeam[team.TeamID]; err != nil {
		return HeldSet{}, err
	}
	if hs, ok := h.heldByTeam[team.TeamID]; ok {
		return hs, nil
	}
	return NewHeldSet(), nil
}

func initTestGitRepo(t *testing.T, remoteURL string) string {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("remote", "add", "origin", remoteURL)
	return dir
}

func TestDryRunListsTeamsAndSizesWithoutNetworkOrStateWrites(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	networkCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		networkCalled = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "s-1", ModTime: now.Add(-48 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-active", ModTime: now.Add(-10 * time.Minute), Active: true},
			{SourceSystem: "claude", SessionID: "s-old", ModTime: now.Add(-120 * 24 * time.Hour), OutsideSince: true},
		},
		segments: map[string][]agents.HistorySegment{
			"s-1": {makeNativeSegment("claude", "s-1", repoA, "hello team a", now.Add(-48*time.Hour))},
		},
	}

	holdingsCalls := map[string][]string{}
	var mu sync.Mutex
	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir,
		Profile:  "default",
		Account:  "acc1",
		AuthURL:  "https://auth.example.com",
		Version:  "test",
		State:    st,
		Sources:  []agents.HistorySource{src},
		Filter:   relay.NewContentFilter(nil),
		Holdings: stubHoldings{mu: &mu, calls: &holdingsCalls},
		Transport: &Transport{
			Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}},
		},
		EndpointFor: func(string) string { return srv.URL },
		TeamLabel:   func(tm routing.Team) string { return "OrgA " + tm.TeamID },
		DryRun:      true,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if networkCalled || len(holdingsCalls) != 0 {
		t.Fatalf("dry-run made network or holdings calls: network=%v holdings=%v", networkCalled, holdingsCalls)
	}
	if _, err := os.Stat(filepath.Join(stateDir, Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run created history state directory: err=%v", err)
	}
	if !sum.DryRun || len(sum.Planned) != 1 {
		t.Fatalf("dry-run summary = %+v, want 1 planned route", sum)
	}
	p := sum.Planned[0]
	if p.TeamID != "team-a" || p.TeamLabel != "OrgA team-a" || p.Sessions != 1 || p.Records != 1 || p.EstimatedBytes <= 0 {
		t.Errorf("unexpected route plan: %+v", p)
	}
	if sum.SkippedSessions[SkipActive] != 1 || sum.SkippedSessions[SkipOutsideSince] != 1 {
		t.Errorf("skipped sessions = %v, want active=1 outside_since=1", sum.SkippedSessions)
	}
}

func TestCrossRepoRoutingIsolatesTwoTeamsAndSkipsMissingOrDeletedCwd(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	repoB := initTestGitRepo(t, "https://github.com/org-b/repo-b.git")
	deletedDir := filepath.Join(t.TempDir(), "gone-work-repo") // does not exist on disk!
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	st := routing.State{
		Account: "acc1",
		Teams: []routing.Team{
			{
				EnrollmentID: "enr-a", TeamID: "team-a", OrganizationID: "org-a", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-a/repo-a"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-a", TeamID: "team-a", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
			{
				EnrollmentID: "enr-b", TeamID: "team-b", OrganizationID: "org-b", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-b/repo-b"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-b", TeamID: "team-b", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
			{
				// Personal collects everything the org teams do not list — deletedDir MUST NOT leak here!
				EnrollmentID: "enr-personal", TeamID: "team-personal", OrganizationID: "org-personal", Personal: true, AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeGlobal, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-personal", TeamID: "team-personal", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
		},
	}

	var mu sync.Mutex
	received := map[string][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		proj := r.Header.Get(sendas.ProjectHeader)
		mu.Lock()
		received[proj] = append(received[proj], string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	src := &stubHistorySource{
		source: "claude",
		files:  []agents.HistoryFile{{SourceSystem: "claude", SessionID: "resumed-sess", ModTime: now.Add(-2 * time.Hour)}},
		segments: map[string][]agents.HistorySegment{
			"resumed-sess": {
				makeNativeSegment("claude", "resumed-sess", repoA, "DISTINGUISHABLE_ORG_A_WORK", now.Add(-3*time.Hour)),
				makeNativeSegment("claude", "resumed-sess", "", "DISTINGUISHABLE_NO_CWD_WORK", now.Add(-2*time.Hour-30*time.Minute)),
				makeNativeSegment("claude", "resumed-sess", deletedDir, "DISTINGUISHABLE_DELETED_DIR_WORK", now.Add(-2*time.Hour-15*time.Minute)),
				makeNativeSegment("claude", "resumed-sess", repoB, "DISTINGUISHABLE_ORG_B_WORK", now.Add(-2*time.Hour)),
			},
		},
	}

	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil),
		Holdings: stubHoldings{},
		Transport: &Transport{
			Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}, BytesPerSec: 1 << 30,
		},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum.SentSessions != 2 {
		t.Fatalf("sent sessions = %d, want 2 (one per destination team)", sum.SentSessions)
	}
	if sum.SkippedSegments[SkipNoCwd] != 1 {
		t.Errorf("skipped no_cwd = %d, want 1", sum.SkippedSegments[SkipNoCwd])
	}
	if sum.SkippedSegments[SkipMissingCwd] != 1 {
		t.Errorf("skipped missing_cwd = %d, want 1", sum.SkippedSegments[SkipMissingCwd])
	}
	teamABody := strings.Join(received["team-a"], "\n")
	teamBBody := strings.Join(received["team-b"], "\n")
	personalBody := strings.Join(received["team-personal"], "\n")
	if !strings.Contains(teamABody, "DISTINGUISHABLE_ORG_A_WORK") || strings.Contains(teamABody, "DISTINGUISHABLE_ORG_B_WORK") || strings.Contains(teamABody, "NO_CWD") {
		t.Errorf("team-a payload contamination or missing content")
	}
	if !strings.Contains(teamBBody, "DISTINGUISHABLE_ORG_B_WORK") || strings.Contains(teamBBody, "DISTINGUISHABLE_ORG_A_WORK") || strings.Contains(teamBBody, "NO_CWD") {
		t.Errorf("team-b payload contamination or missing content")
	}
	if personalBody != "" {
		t.Errorf("deleted directory leaked into personal team!")
	}
}

func TestHoldingsChunkingMultiOrgSkipAndFailClosed(t *testing.T) {
	t.Parallel()
	// 1. Verify 125 session IDs split into chunks of <= 50 and filter length <= 3800 (< 4096).
	var ids []string
	for i := range 125 {
		ids = append(ids, fmt.Sprintf("sess-%03d-abcdef0123456789", i))
	}
	chunks, err := BuildSessionIDFilterChunks(ids, HoldingsChunkSize, HoldingsMaxFilterBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("125 IDs produced %d chunks, want 3 (50, 50, 25)", len(chunks))
	}
	for i, ch := range chunks {
		if len(ch) > 4096 {
			t.Errorf("chunk %d length %d exceeds 4096-byte gateway limit", i, len(ch))
		}
	}

	// 2. Real APIHoldingsChecker hitting httptest GET /v1/ai/sessions with pagination + two orgs:
	// Org A holds sess-held (sent live weeks ago with no local claim) and returns 503 on Org C (fail closed!).
	var mu sync.Mutex
	seenFilters := map[string][]string{}
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ai/sessions" {
			http.NotFound(w, r)
			return
		}
		proj := r.Header.Get(sendas.ProjectHeader)
		if proj == "team-fail" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"UNAVAILABLE","message":"clickhouse unavailable"}}`))
			return
		}
		mu.Lock()
		seenFilters[proj] = append(seenFilters[proj], r.URL.Query().Get("filter"))
		mu.Unlock()
		page := r.URL.Query().Get("page")
		w.Header().Set("Content-Type", "application/json")
		if proj == "team-a" {
			if page == "1" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"project_id": proj,
					"sessions":   []map[string]any{{"source_system": "claude", "session_id": "sess-old-live", "historical": false}},
					"pagination": map[string]any{"page": 1, "per_page": 200, "total": 2, "total_pages": 2},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"project_id": proj,
				"sessions":   []map[string]any{{"source_system": "claude", "session_id": "sess-pre-feature", "historical": false}},
				"pagination": map[string]any{"page": 2, "per_page": 200, "total": 2, "total_pages": 2},
			})
			return
		}
		// team-b holds nothing -> sess-old-live MUST still upload to team-b!
		_ = json.NewEncoder(w).Encode(map[string]any{
			"project_id": proj,
			"sessions":   []any{},
			"pagination": map[string]any{"page": 1, "per_page": 200, "total": 0, "total_pages": 1},
		})
	}))
	defer apiSrv.Close()

	var uploadedByTeam = map[string][]string{}
	otlpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var logs logspb.LogsData
		_ = proto.Unmarshal(body, &logs)
		proj := r.Header.Get(sendas.ProjectHeader)
		mu.Lock()
		for _, rl := range logs.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					for _, kv := range lr.Attributes {
						if kv.Key == "session.id" {
							uploadedByTeam[proj] = append(uploadedByTeam[proj], kv.Value.GetStringValue())
						}
					}
				}
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer otlpSrv.Close()

	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	repoB := initTestGitRepo(t, "https://github.com/org-b/repo-b.git")
	repoFail := initTestGitRepo(t, "https://github.com/org-c/repo-c.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	st := routing.State{
		Account: "acc1",
		Teams: []routing.Team{
			{
				EnrollmentID: "enr-a", TeamID: "team-a", OrganizationID: "org-a", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-a/repo-a"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-a", TeamID: "team-a", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
			{
				EnrollmentID: "enr-b", TeamID: "team-b", OrganizationID: "org-b", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-b/repo-b"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-b", TeamID: "team-b", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
			{
				EnrollmentID: "enr-fail", TeamID: "team-fail", OrganizationID: "org-c", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-c/repo-c"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-c", TeamID: "team-fail", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
		},
	}

	checker := &APIHoldingsChecker{
		ClientFor: func(teamID string) (*api.Client, error) {
			return api.New(&config.Config{
				APIURL: apiSrv.URL, AuthURL: "https://auth.example.com", APIKey: "k",
			}, api.Options{Version: "test", ProjectID: teamID})
		},
	}
	// Note: api.Client only sends X-Mirador-Project when apiKey==""; override via custom checker wrapping apiSrv with user cred:
	checker.ClientFor = func(teamID string) (*api.Client, error) {
		cfg := &config.Config{APIURL: apiSrv.URL, AuthURL: "https://auth.example.com", Dir: stateDir, ProfileName: "default"}
		cred := newTestUserCred("https://auth.example.com", now)
		return api.New(cfg, api.Options{Version: "test", ProjectID: teamID, Credential: cred})
	}

	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "sess-old-live", ModTime: now.Add(-30 * 24 * time.Hour)},
			{SourceSystem: "claude", SessionID: "sess-pre-feature", ModTime: now.Add(-60 * 24 * time.Hour)},
			{SourceSystem: "claude", SessionID: "sess-new-a", ModTime: now.Add(-5 * time.Hour)},
			{SourceSystem: "claude", SessionID: "sess-fail-team", ModTime: now.Add(-5 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"sess-old-live": {
				makeNativeSegment("claude", "sess-old-live", repoA, "in org a", now.Add(-30*24*time.Hour)),
				makeNativeSegment("claude", "sess-old-live", repoB, "in org b", now.Add(-30*24*time.Hour)),
			},
			"sess-pre-feature": {makeNativeSegment("claude", "sess-pre-feature", repoA, "before feature", now.Add(-60*24*time.Hour))},
			"sess-new-a":       {makeNativeSegment("claude", "sess-new-a", repoA, "new session", now.Add(-5*time.Hour))},
			"sess-fail-team":   {makeNativeSegment("claude", "sess-fail-team", repoFail, "must not upload unchecked", now.Add(-5*time.Hour))},
		},
	}

	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil),
		Holdings: checker,
		Transport: &Transport{
			Version: "test", HTTP: otlpSrv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}, BytesPerSec: 1 << 30,
		},
		EndpointFor: func(string) string { return otlpSrv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum.SkippedSessions[SkipAlreadyHeld] != 2 {
		t.Errorf("already_held = %d, want 2 (sess-old-live & sess-pre-feature on team-a)", sum.SkippedSessions[SkipAlreadyHeld])
	}
	if sum.SkippedSessions[SkipHoldingsFailed] != 1 {
		t.Errorf("holdings_check_failed = %d, want 1 (team-fail skipped fail-closed)", sum.SkippedSessions[SkipHoldingsFailed])
	}
	if len(uploadedByTeam["team-fail"]) != 0 {
		t.Fatalf("team-fail uploaded unchecked sessions: %v", uploadedByTeam["team-fail"])
	}
	if len(uploadedByTeam["team-a"]) != 1 || uploadedByTeam["team-a"][0] != "sess-new-a" {
		t.Errorf("team-a uploaded = %v, want [sess-new-a]", uploadedByTeam["team-a"])
	}
	if len(uploadedByTeam["team-b"]) != 1 || uploadedByTeam["team-b"][0] != "sess-old-live" {
		t.Errorf("team-b uploaded = %v, want [sess-old-live] (not held in org B)", uploadedByTeam["team-b"])
	}
}

func TestRestrictivePolicyFiltersContentAndKeepsHistoricalMarker(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Restrictive policy: IncludePrompts=false, IncludeToolContent=false.
	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", false, false)
	seg := makeNativeSegment("claude", "s-redacted", repoA, "SECRET_USER_PROMPT", now.Add(-3*time.Hour))
	// Add tool_input and supplemental reply that must both be withheld!
	seg.NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes = append(
		seg.NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes,
		&commonpb.KeyValue{Key: "tool_input", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: `{"cmd":"SECRET_CMD"}`}}},
	)
	seg.SupplementalLogs = &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: semconv.ServiceNameKey, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "terma-cli"}}},
		}},
		ScopeLogs: []*logspb.ScopeLogs{{
			Scope: &commonpb.InstrumentationScope{Name: "terma-cli"},
			LogRecords: []*logspb.LogRecord{{
				EventName: semconv.TermaAssistantMessageEvent,
				Attributes: []*commonpb.KeyValue{
					{Key: semconv.TermaMessageTextKey, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "SECRET_SUPPLEMENTAL_REPLY"}}},
				},
			}},
		}},
	}}}

	reg := builtin.Agents("")
	filter := relay.NewContentFilter(reg.With[shape.Capturer]())
	src := &stubHistorySource{
		source:   "claude",
		files:    []agents.HistoryFile{{SourceSystem: "claude", SessionID: "s-redacted", ModTime: now.Add(-3 * time.Hour)}},
		segments: map[string][]agents.HistorySegment{"s-redacted": {seg}},
	}

	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: filter, Holdings: stubHoldings{},
		Transport: &Transport{
			Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}, BytesPerSec: 1 << 30,
		},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum.SentSessions != 1 || len(received) == 0 {
		t.Fatalf("expected 1 sent session, got %+v", sum)
	}
	if strings.Contains(string(received), "SECRET_") {
		t.Fatalf("withheld content reached upstream: %q", string(received))
	}
	if strings.Contains(string(received), repoA) {
		t.Fatalf("local checkout path leaked under IncludeToolContent=false")
	}
	var got logspb.LogsData
	if err := proto.Unmarshal(received, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.ResourceLogs) != 1 {
		t.Fatalf("supplemental reply should have been dropped; ResourceLogs=%d", len(got.ResourceLogs))
	}
	checkHist := func(where string, attrs []*commonpb.KeyValue) {
		for _, kv := range attrs {
			if kv.Key == semconv.TermaSessionHistoricalKey && kv.Value.GetBoolValue() {
				return
			}
		}
		t.Errorf("%s missing %s=true", where, semconv.TermaSessionHistoricalKey)
	}
	checkHist("resource", got.ResourceLogs[0].Resource.Attributes)
	checkHist("record", got.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes)
}

func TestResumeOrderRepeatRunPartialSuccessFailureAndAccountIsolation(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)

	// Session 1 spans 2 chunks (using small MaxReqBytes); cancel context after chunk 1 to test resume!
	makeTwoChunkSeg := func(sid string) agents.HistorySegment {
		seg := makeNativeSegment("claude", sid, repoA, strings.Repeat("A", 500), now.Add(-5*time.Hour))
		rec2 := proto.CloneOf(seg.NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords[0])
		seg.NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords = append(seg.NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords, rec2)
		seg.RecordCount = 2
		return seg
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	var mu sync.Mutex
	var postOrder []string
	failPartial := false
	cancelAfterFirst := true

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var logs logspb.LogsData
		_ = proto.Unmarshal(body, &logs)
		sid := ""
		for _, kv := range logs.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes {
			if kv.Key == "session.id" {
				sid = kv.Value.GetStringValue()
			}
		}
		mu.Lock()
		postOrder = append(postOrder, sid)
		doCancel := cancelAfterFirst
		cancelAfterFirst = false
		doPartial := failPartial && sid == "s-bad"
		mu.Unlock()
		if doPartial {
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(encodePartialSuccess(1, "oversized log record"))
			return
		}
		w.WriteHeader(http.StatusOK)
		if doCancel {
			cancelRun()
		}
	}))
	defer srv.Close()

	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "s-interrupted", ModTime: now.Add(-6 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-bad", ModTime: now.Add(-5 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-good", ModTime: now.Add(-4 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"s-interrupted": {makeTwoChunkSeg("s-interrupted")},
			"s-bad":         {makeNativeSegment("claude", "s-bad", repoA, "bad record", now.Add(-5*time.Hour))},
			"s-good":        {makeNativeSegment("claude", "s-good", repoA, "good record", now.Add(-4*time.Hour))},
		},
	}

	makeOpts := func(account string, holdings HoldingsChecker) Options {
		rs := st
		rs.Account = account
		return Options{
			StateDir: stateDir, Profile: "default", Account: account, AuthURL: "https://auth.example.com", Version: "test",
			State: rs, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil),
			Holdings: holdings,
			Transport: &Transport{
				Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: account, tokens: []string{"tok"}}, BytesPerSec: 1 << 30,
			},
			EndpointFor: func(string) string { return srv.URL },
			MaxReqBytes: 1650, // forces s-interrupted (2 records ~1400B + ~600B) into 2 chunks
			Now:         func() time.Time { return now },
		}
	}

	// Run 1: interrupts after chunk 1 of s-interrupted.
	sum1, err1 := Upload(runCtx, makeOpts("acc1", stubHoldings{}))
	if !errors.Is(err1, context.Canceled) || !sum1.Interrupted {
		t.Fatalf("run 1 = (%+v, %v), want Interrupted + context.Canceled", sum1, err1)
	}
	loaded, found, err := LoadState(stateDir, "acc1", "default", "enr-team-a", "team-a")
	if err != nil || !found || !loaded.IsInProgress(SessionKey("claude", "s-interrupted")) || loaded.IsDone(SessionKey("claude", "s-interrupted")) {
		t.Fatalf("after interrupt state = %+v (found=%v err=%v), want s-interrupted in_progress", loaded, found, err)
	}

	// Run 2: resume! Server holdings claims s-interrupted is ALREADY held (because chunk 1 landed),
	// AND s-bad returns OTLP partial_success rejection.
	// Expected:
	// - s-interrupted resumes FIRST and SKIPS holdings check!
	// - s-bad is marked failed with reason, upload continues to s-good!
	mu.Lock()
	postOrder = nil
	failPartial = true
	mu.Unlock()
	heldAfterChunk1 := NewHeldSet()
	heldAfterChunk1.Add("claude", "s-interrupted")
	holdingsChecked := map[string][]string{}
	sum2, err2 := Upload(context.Background(), makeOpts("acc1", stubHoldings{
		heldByTeam: map[string]HeldSet{"team-a": heldAfterChunk1},
		mu:         &mu,
		calls:      &holdingsChecked,
	}))
	if err2 != nil {
		t.Fatal(err2)
	}
	if sum2.SentSessions != 2 {
		t.Fatalf("run 2 sent = %d, want 2 (s-interrupted + s-good)", sum2.SentSessions)
	}
	for _, checkedID := range holdingsChecked["team-a"] {
		if checkedID == "s-interrupted" {
			t.Errorf("in-progress session s-interrupted must skip holdings check on resume")
		}
	}
	if len(postOrder) < 4 || postOrder[0] != "s-interrupted" || postOrder[1] != "s-interrupted" || postOrder[2] != "s-bad" || postOrder[3] != "s-good" {
		t.Fatalf("post order on resume = %v, want [s-interrupted, s-interrupted, s-bad, s-good]", postOrder)
	}
	if len(sum2.Failed) != 1 || sum2.Failed[0].SessionID != "s-bad" || sum2.Failed[0].RejectedRecords != 1 || !strings.Contains(sum2.Failed[0].Reason, "oversized log record") {
		t.Fatalf("run 2 failed = %+v, want s-bad with partial_success reason", sum2.Failed)
	}

	// Run 3: repeat run sends NOTHING, skips done & failed, and still lists s-bad in Failed!
	mu.Lock()
	postOrder = nil
	holdingsChecked = map[string][]string{}
	mu.Unlock()
	sum3, err3 := Upload(context.Background(), makeOpts("acc1", stubHoldings{mu: &mu, calls: &holdingsChecked}))
	if err3 != nil {
		t.Fatal(err3)
	}
	if len(postOrder) != 0 || len(holdingsChecked) != 0 || sum3.SentSessions != 0 {
		t.Fatalf("repeat run sent posts=%v holdings=%v sum=%+v, want zero sends", postOrder, holdingsChecked, sum3)
	}
	if sum3.SkippedSessions[SkipAlreadyDone] != 2 || sum3.SkippedSessions[SkipFailedPreviously] != 1 {
		t.Errorf("repeat run skipped = %v, want already_done=2 failed_previously=1", sum3.SkippedSessions)
	}
	if len(sum3.Failed) != 1 || sum3.Failed[0].SessionID != "s-bad" || !sum3.Failed[0].Previously {
		t.Errorf("repeat run failed = %+v, want s-bad with Previously=true", sum3.Failed)
	}

	// Account isolation: switching sign-in to acc2 has clean state and does not inherit acc1's done/failed state.
	st2, found2, err := LoadState(stateDir, "acc2", "default", "enr-team-a", "team-a")
	if err != nil || found2 || len(st2.Done) != 0 || len(st2.Failed) != 0 {
		t.Fatalf("acc2 state leaked acc1 data: found=%v st=%+v err=%v", found2, st2, err)
	}
}

func newTestUserCred(authURL string, _ time.Time) *auth.Credential {
	return &auth.Credential{
		SessionID:    "test-session",
		UserID:       "user-1",
		AuthURL:      authURL,
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
}

func TestOriginMismatchFailsClosed(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	// Directory on disk now points at org-b/repo-b, while transcript recorded org-a/repo-a.
	reusedDir := initTestGitRepo(t, "https://github.com/org-b/repo-b.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	st := routing.State{
		Account: "acc1",
		Teams: []routing.Team{
			{
				EnrollmentID: "enr-a", TeamID: "team-a", OrganizationID: "org-a", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-a/repo-a"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-a", TeamID: "team-a", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
			{
				EnrollmentID: "enr-b", TeamID: "team-b", OrganizationID: "org-b", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-b/repo-b"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-b", TeamID: "team-b", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
		},
	}

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	seg := makeNativeSegment("codex", "s-reused", reusedDir, "SECRET_ORG_A_CONTENT", now.Add(-4*time.Hour))
	seg.FallbackRepoURL = "https://github.com/org-a/repo-a.git"
	src := &stubHistorySource{
		source:   "codex",
		files:    []agents.HistoryFile{{SourceSystem: "codex", SessionID: "s-reused", ModTime: now.Add(-4 * time.Hour)}},
		segments: map[string][]agents.HistorySegment{"s-reused": {seg}},
	}

	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if called || sum.SentSessions != 0 {
		t.Fatalf("origin mismatch leaked to upstream: called=%v sum=%+v", called, sum)
	}
	if sum.SkippedSegments[SkipOriginMismatch] != 1 {
		t.Errorf("skipped origin_mismatch = %d, want 1", sum.SkippedSegments[SkipOriginMismatch])
	}
}

func TestRunsMixedCutoffDropsPostMixedRecordsAndUnreadableLedgerFailsClosed(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-5 * time.Hour)
	mixedAt := t0.Add(10 * time.Minute)

	if _, err := runs.Start(stateDir, "s-mixed", t0); err != nil {
		t.Fatal(err)
	}
	if err := runs.MarkMixed(stateDir, "s-mixed", mixedAt, runs.Why{Reason: runs.Crossed}); err != nil {
		t.Fatal(err)
	}
	// Plant corrupt unreadable ledger for s-corrupt -> must fail closed!
	if err := os.WriteFile(filepath.Join(stateDir, runs.Dir, "s-corrupt.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "s-mixed", ModTime: now.Add(-4 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-corrupt", ModTime: now.Add(-4 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"s-mixed": {
				makeNativeSegment("claude", "s-mixed", repoA, "BEFORE_MIXED_OK", t0.Add(time.Minute)),
				makeNativeSegment("claude", "s-mixed", repoA, "AFTER_MIXED_SECRET", mixedAt.Add(time.Second)),
			},
			"s-corrupt": {
				makeNativeSegment("claude", "s-corrupt", repoA, "CORRUPT_LEDGER_SECRET", t0.Add(time.Minute)),
			},
		},
	}

	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}, BytesPerSec: 1 << 30},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(received, "\n")
	if !strings.Contains(all, "BEFORE_MIXED_OK") {
		t.Errorf("pre-Mixed record was unexpectedly dropped")
	}
	if strings.Contains(all, "AFTER_MIXED_SECRET") || strings.Contains(all, "CORRUPT_LEDGER_SECRET") {
		t.Fatalf("post-Mixed or corrupt-ledger content leaked upstream!")
	}
	if sum.SentSessions != 1 || sum.SkippedSessions[SkipMixed] != 1 {
		t.Errorf("summary = sent:%d skipped:%v, want sent=1 mixed=1", sum.SentSessions, sum.SkippedSessions)
	}
}

func TestLivePolicyRecheckStopsBeforeSendingStaleChunks(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	stOpen := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	stTight := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", false, false)

	var posts int
	var mu sync.Mutex
	curState := stOpen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		posts++
		// Tighten policy immediately after chunk 1 succeeds!
		curState = stTight
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	seg := makeNativeSegment("claude", "s-multi", repoA, strings.Repeat("P", 500), now.Add(-4*time.Hour))
	rec2 := proto.CloneOf(seg.NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords[0])
	seg.NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords = append(seg.NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords, rec2)
	seg.RecordCount = 2

	src := &stubHistorySource{
		source:   "claude",
		files:    []agents.HistoryFile{{SourceSystem: "claude", SessionID: "s-multi", ModTime: now.Add(-4 * time.Hour)}},
		segments: map[string][]agents.HistorySegment{"s-multi": {seg}},
	}

	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: stOpen,
		ReloadState: func() (routing.State, error) {
			mu.Lock()
			defer mu.Unlock()
			return curState, nil
		},
		Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}, BytesPerSec: 1 << 30},
		EndpointFor: func(string) string { return srv.URL },
		MaxReqBytes: 1650,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("posts = %d, want 1 (chunk 2 must not leave after policy tightened)", posts)
	}
	if sum.SkippedSessions[SkipPolicyChanged] != 1 || len(sum.TeamIssues) != 1 {
		t.Fatalf("expected policy_changed skip + TeamIssue, got %+v", sum)
	}
	loaded, _, _ := LoadState(stateDir, "acc1", "default", "enr-team-a", "team-a")
	if !loaded.IsInProgress(SessionKey("claude", "s-multi")) || loaded.IsDone(SessionKey("claude", "s-multi")) {
		t.Fatalf("session should remain in_progress after mid-session policy change: %+v", loaded)
	}
}

func TestResumedFileSkippedAndNotMarkedDoneIfModifiedMidUpload(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	f1 := filepath.Join(t.TempDir(), "s-resumed-before.jsonl")
	f2 := filepath.Join(t.TempDir(), "s-modified-during.jsonl")
	for _, p := range []string{f1, f2} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, now.Add(-3*time.Hour), now.Add(-3*time.Hour))
	}
	// Simulate user resuming f1 AFTER discovery classified it inactive (mtime is now 5m ago).
	_ = os.Chtimes(f1, now.Add(-5*time.Minute), now.Add(-5*time.Minute))

	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts++
		// Simulate user appending to f2 while its upload is in flight!
		_ = os.WriteFile(f2, []byte("{}\n{\"new\":true}\n"), 0o600)
		_ = os.Chtimes(f2, now.Add(-1*time.Minute), now.Add(-1*time.Minute))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "s-resumed-before", Path: f1, ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-modified-during", Path: f2, ModTime: now.Add(-3 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"s-resumed-before":  {makeNativeSegment("claude", "s-resumed-before", repoA, "turn 1", now.Add(-3*time.Hour))},
			"s-modified-during": {makeNativeSegment("claude", "s-modified-during", repoA, "turn 1", now.Add(-3*time.Hour))},
		},
	}

	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}, BytesPerSec: 1 << 30},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("posts = %d, want 1 (f1 must skip before upload)", posts)
	}
	if sum.SentSessions != 0 || sum.SkippedSessions[SkipActive] != 2 {
		t.Fatalf("expected sent=0 active=2, got %+v", sum)
	}
	loaded, _, _ := LoadState(stateDir, "acc1", "default", "enr-team-a", "team-a")
	if loaded.IsDone(SessionKey("claude", "s-modified-during")) || !loaded.IsInProgress(SessionKey("claude", "s-modified-during")) {
		t.Fatalf("s-modified-during must remain in_progress (not done): %+v", loaded)
	}
}

func TestConcurrentUploadLockAndRouteErrorResumableAndResumeQueueAccounting(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)

	// 1. Concurrent lock blocks second non-dry-run upload, while dry-run still works.
	unlock, err := LockUpload(stateDir, "acc1", "default")
	if err != nil {
		t.Fatal(err)
	}
	_, lockErr := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com",
		State: st, Now: func() time.Time { return now },
	})
	if !errors.Is(lockErr, ErrUploadRunning) {
		t.Fatalf("concurrent upload error = %v, want ErrUploadRunning", lockErr)
	}
	if _, dryErr := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com",
		State: st, DryRun: true, Now: func() time.Time { return now },
	}); dryErr != nil {
		t.Fatalf("dry-run under lock failed: %v", dryErr)
	}
	unlock()

	// 2. Pre-seed s-resume as in_progress + 2 candidates; server returns 401 -> stops route with
	// ONE TeamIssue covering all 3 sessions, leaves s-resume in_progress (NOT failed), and next run succeeds!
	rs := NewRouteState("acc1", "default", "enr-team-a", "team-a")
	rs.MarkInProgress("claude", "s-resume", now.Add(-time.Hour), 1, 100)
	if err := SaveState(stateDir, rs); err != nil {
		t.Fatal(err)
	}

	denyAuth := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if denyAuth {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("invalid token"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "s-resume", ModTime: now.Add(-5 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-cand-1", ModTime: now.Add(-4 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-cand-2", ModTime: now.Add(-3 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"s-resume": {makeNativeSegment("claude", "s-resume", repoA, "r", now.Add(-5*time.Hour))},
			"s-cand-1": {makeNativeSegment("claude", "s-cand-1", repoA, "c1", now.Add(-4*time.Hour))},
			"s-cand-2": {makeNativeSegment("claude", "s-cand-2", repoA, "c2", now.Add(-3*time.Hour))},
		},
	}

	opts := Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"t1", "t2"}}, BytesPerSec: 1 << 30},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	}
	sum1, err := Upload(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum1.Failed) != 0 {
		t.Fatalf("401 must not mark session failed: %+v", sum1.Failed)
	}
	if len(sum1.TeamIssues) != 1 || sum1.TeamIssues[0].Skipped != 3 || sum1.SkippedSessions[SkipSendFailed] != 3 {
		t.Fatalf("expected 1 TeamIssue skipping 3 sessions, got issues=%+v skipped=%v", sum1.TeamIssues, sum1.SkippedSessions)
	}

	// Fix credentials and re-run -> all 3 sessions upload cleanly!
	denyAuth = false
	sum2, err := Upload(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum2.SentSessions != 3 || len(sum2.TeamIssues) != 0 {
		t.Fatalf("recovery run = %+v, want 3 sent sessions", sum2)
	}
}

func TestHoldingsRetiredErrorAndServerKeyWrongEnv(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	src := &stubHistorySource{
		source:   "claude",
		files:    []agents.HistoryFile{{SourceSystem: "claude", SessionID: "s-1", ModTime: now.Add(-3 * time.Hour)}},
		segments: map[string][]agents.HistorySegment{"s-1": {makeNativeSegment("claude", "s-1", repoA, "hi", now.Add(-3*time.Hour))}},
	}

	// Pre-marked auth.ErrSignInRetired (without *api.Error) must abort with *SignInRetiredError.
	_, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil),
		Holdings:  stubHoldings{errByTeam: map[string]error{"team-a": auth.ErrSignInRetired}},
		Transport: &Transport{Version: "test", SignIn: &fakeSignIn{account: "acc1", tokens: []string{"t"}}},
		Now:       func() time.Time { return now },
	})
	if _, ok := errors.AsType[*SignInRetiredError](err); !ok {
		t.Fatalf("retired holdings error = %v, want *SignInRetiredError", err)
	}

	// Server-key profile with mismatched ServerKeyAuthURL must skip as wrong_environment even when team.AuthURL is empty.
	stSK := singleTeamRoutingState(now, sendas.NoPersonLane, "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	stSK.Teams[0].AuthURL = ""
	sumSK, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: sendas.NoPersonLane,
		ServerKey: true, ServerKeyTeam: "team-a", ServerKeyAuthURL: "https://prod.auth.example.com",
		AuthURL: "https://dev.auth.example.com", Version: "test",
		State: stSK, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil),
		DryRun: true, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sumSK.Planned) != 0 || sumSK.SkippedSegments[SkipWrongEnvironment] != 1 {
		t.Fatalf("server-key wrong env = planned:%+v skipped:%v, want wrong_environment=1", sumSK.Planned, sumSK.SkippedSegments)
	}
}

func TestHoldingsEmptyEnvelopeOrWrongProjectFailsClosed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"empty_json", http.StatusOK, `{}`},
		{"no_content", http.StatusNoContent, ``},
		{"wrong_project_id", http.StatusOK, `{"project_id":"team-other","sessions":[],"pagination":{"page":1,"per_page":200,"total":0,"total_pages":1}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			checker := &APIHoldingsChecker{
				ClientFor: func(teamID string) (*api.Client, error) {
					cfg := &config.Config{APIURL: srv.URL, AuthURL: "https://auth.example.com", Dir: t.TempDir(), ProfileName: "default"}
					return api.New(cfg, api.Options{Version: "test", ProjectID: teamID, Credential: newTestUserCred("https://auth.example.com", now)})
				},
			}
			_, err := checker.Check(context.Background(), routing.Team{TeamID: "team-a"}, []string{"sess-1"})
			if err == nil {
				t.Fatalf("%s: expected fail-closed error, got nil", tc.name)
			}
		})
	}
}

func TestHistoricalToolReachDropsCrossOrgWithoutRunsLedgerAndIgnoresTmp(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	repoB := initTestGitRepo(t, "https://github.com/org-b/repo-b.git")
	secretFile := filepath.Join(repoB, "secret.go")
	if err := os.WriteFile(secretFile, []byte("package secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-6 * time.Hour)
	crossAt := t0.Add(2 * time.Minute)

	st := routing.State{
		Account: "acc1",
		Teams: []routing.Team{
			{
				EnrollmentID: "enr-a", TeamID: "team-a", OrganizationID: "org-a", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-a/repo-a"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-a", TeamID: "team-a", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
			{
				EnrollmentID: "enr-b", TeamID: "team-b", OrganizationID: "org-b", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-b/repo-b"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-b", TeamID: "team-b", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
		},
	}

	var mu sync.Mutex
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Build one segment in repoA with NO runs ledger:
	// - t0+1m: benign prompt + /dev/null reach (nobody's dir -> stays clean!)
	// - t0+2m: Read secretFile in orgB -> triggers cross-org Mixed cutoff at t0+2m!
	// - t0+3m: assistant reply quoting orgB secret -> MUST BE DROPPED!
	seg := makeNativeSegment("claude", "s-pre-terma", repoA, "BEFORE_CROSSING_OK", t0.Add(time.Minute))
	afterRec := proto.CloneOf(seg.NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords[0])
	afterRec.TimeUnixNano = uint64(crossAt.Add(time.Minute).UnixNano())
	afterRec.ObservedTimeUnixNano = afterRec.TimeUnixNano
	for _, kv := range afterRec.Attributes {
		if kv.Key == "prompt" {
			kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "AFTER_CROSSING_ORG_B_SECRET"}}
		}
	}
	seg.NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords = append(seg.NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords, afterRec)
	seg.RecordCount = 2
	seg.ToolReaches = []agents.HistoryToolReach{
		{At: t0.Add(30 * time.Second), Cwd: repoA, Paths: []string{"/dev/null"}},
		{At: crossAt, Cwd: repoA, Paths: []string{secretFile}},
	}

	src := &stubHistorySource{
		source:   "claude",
		files:    []agents.HistoryFile{{SourceSystem: "claude", SessionID: "s-pre-terma", ModTime: now.Add(-5 * time.Hour)}},
		segments: map[string][]agents.HistorySegment{"s-pre-terma": {seg}},
	}
	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}, BytesPerSec: 1 << 30},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(received, "\n")
	if sum.SentSessions != 1 || !strings.Contains(all, "BEFORE_CROSSING_OK") {
		t.Fatalf("pre-crossing record should have uploaded: sum=%+v", sum)
	}
	if strings.Contains(all, "AFTER_CROSSING_ORG_B_SECRET") {
		t.Fatalf("post-crossing org B content leaked to org A without a runs ledger!")
	}
}

func TestPreAttemptStopsOnRebindAndOnPolicyTightenDuring429Retry(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	stOpen := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	stOpen.StateID = "st-1"
	stOpen.ConsentGeneration = 1
	stTight := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", false, false)
	stTight.StateID = "st-1"
	stTight.ConsentGeneration = 1

	var mu sync.Mutex
	curState := stOpen
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		posts++
		attempt := posts
		mu.Unlock()
		if attempt == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	src := &stubHistorySource{
		source:   "claude",
		files:    []agents.HistoryFile{{SourceSystem: "claude", SessionID: "s-retry", ModTime: now.Add(-3 * time.Hour)}},
		segments: map[string][]agents.HistorySegment{"s-retry": {makeNativeSegment("claude", "s-retry", repoA, "PROMPT_SECRET", now.Add(-3*time.Hour))}},
	}

	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: stOpen,
		ReloadState: func() (routing.State, error) {
			mu.Lock()
			defer mu.Unlock()
			return curState, nil
		},
		Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport: &Transport{
			Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}},
			BytesPerSec: 1 << 30, InitialBackoff: time.Millisecond,
			Sleep: func(_ context.Context, _ time.Duration) error {
				// Policy disables prompts while waiting on 429 Retry-After!
				mu.Lock()
				curState = stTight
				mu.Unlock()
				return nil
			},
		},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("posts = %d, want 1 (retry must abort after policy tightened during 429 sleep)", posts)
	}
	if sum.SentSessions != 0 || sum.SkippedSessions[SkipPolicyChanged] != 1 {
		t.Fatalf("expected policy_changed skip, got %+v", sum)
	}

	// Now verify a ConsentGeneration bump (a change to the selected teams) aborts before sending!
	stRebound := stOpen
	stRebound.ConsentGeneration = 2
	posts = 0
	curState = stOpen
	rebindReads := 0
	sum2, err := Upload(context.Background(), Options{
		StateDir: t.TempDir(), Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: stOpen,
		ReloadState: func() (routing.State, error) {
			mu.Lock()
			defer mu.Unlock()
			rebindReads++
			if rebindReads >= 3 {
				return stRebound, nil
			}
			return stOpen, nil
		},
		Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}, BytesPerSec: 1 << 30},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if posts != 0 || sum2.SkippedSessions[SkipDeselected] != 1 {
		t.Fatalf("a team selection change should abort before POST: posts=%d sum=%+v", posts, sum2)
	}
}

type failingReadSource struct {
	stubHistorySource
	failID map[string]error
}

func (f *failingReadSource) ReadHistory(ctx context.Context, file agents.HistoryFile, opts agents.HistoryReadOptions, emit func(agents.HistorySegment) error) error {
	if err := f.failID[file.SessionID]; err != nil {
		return err
	}
	return f.stubHistorySource.ReadHistory(ctx, file, opts, emit)
}

func TestUnreadableFileSkippedAndOutsideGitHasNoRepoRootAndEmptyResumeCleared(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	outsideDir := t.TempDir()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	// Personal team collecting OutsideGit so we also assert CheckoutInfo.Root is NOT set outside git!
	st := routing.State{
		Account: "acc1",
		Teams: []routing.Team{{
			EnrollmentID: "enr-p", TeamID: "team-p", OrganizationID: "org-p", Personal: true, AuthURL: "https://auth.example.com",
			Policy: config.Policy{Mode: config.ModeGlobal, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-p", TeamID: "team-p", AuthURL: "https://auth.example.com", FetchedAt: now},
		}},
	}

	// Pre-seed s-empty-resume in_progress + corrupt its runs ledger so it becomes wholly Mixed and must clear in_progress!
	rs := NewRouteState("acc1", "default", "enr-p", "team-p")
	rs.MarkInProgress("claude", "s-empty-resume", now.Add(-time.Hour), 1, 50)
	if err := SaveState(stateDir, rs); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, runs.Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, runs.Dir, "s-empty-resume.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}

	var received *logspb.LogsData
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var logs logspb.LogsData
		_ = proto.Unmarshal(body, &logs)
		received = &logs
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	src := &failingReadSource{
		stubHistorySource: stubHistorySource{
			source: "claude",
			files: []agents.HistoryFile{
				{SourceSystem: "claude", SessionID: "s-unreadable", ModTime: now.Add(-5 * time.Hour)},
				{SourceSystem: "claude", SessionID: "s-empty-resume", ModTime: now.Add(-4 * time.Hour)},
				{SourceSystem: "claude", SessionID: "s-good-outside", ModTime: now.Add(-3 * time.Hour)},
			},
			segments: map[string][]agents.HistorySegment{
				"s-empty-resume": {makeNativeSegment("claude", "s-empty-resume", outsideDir, "will drop", now.Add(-4*time.Hour))},
				"s-good-outside": {makeNativeSegment("claude", "s-good-outside", outsideDir, "outside git ok", now.Add(-3*time.Hour))},
			},
		},
		failID: map[string]error{"s-unreadable": os.ErrPermission},
	}

	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}, BytesPerSec: 1 << 30},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("unreadable file must not abort upload: %v", err)
	}
	if sum.SentSessions != 1 || sum.SkippedSessions[SkipUnreadable] != 1 || sum.SkippedSessions[SkipMixed] != 1 {
		t.Fatalf("unexpected summary: %+v", sum)
	}
	// Verify OutsideGit resource did NOT stamp terma.repository.root or worktree.linked.
	if received == nil || len(received.ResourceLogs) == 0 {
		t.Fatal("expected uploaded outside-git logs")
	}
	for _, kv := range received.ResourceLogs[0].Resource.Attributes {
		if kv.Key == semconv.TermaRepositoryRootKey || kv.Key == semconv.TermaRepositoryWorktreeLinkedKey {
			t.Errorf("outside-git session stamped bogus repo attribute %s=%v", kv.Key, kv.Value)
		}
	}
	// Verify s-empty-resume was removed from InProgress.
	after, _, _ := LoadState(stateDir, "acc1", "default", "enr-p", "team-p")
	if after.IsInProgress(SessionKey("claude", "s-empty-resume")) {
		t.Errorf("wholly-mixed resumed session remained stuck in InProgress: %+v", after)
	}
}

func TestSessionWideReachCutoffOutOfOrderSubagentDeletedPathsAndAgedFailure(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	repoASub := filepath.Join(repoA, "subpkg")
	if err := os.MkdirAll(repoASub, 0o755); err != nil {
		t.Fatal(err)
	}
	repoB := initTestGitRepo(t, "https://github.com/org-b/repo-b.git")
	secretB1 := filepath.Join(repoB, "a.go")
	secretB2 := filepath.Join(repoB, "b.go")
	for _, p := range []string{secretB1, secretB2} {
		if err := os.WriteFile(p, []byte("package b\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-6 * time.Hour)

	// No Personal team selected! Deleted external path MUST still fail closed, while deleted
	// subdir inside repoA MUST NOT falsely trigger Mixed!
	st := routing.State{
		Account: "acc1",
		Teams: []routing.Team{
			{
				EnrollmentID: "enr-a", TeamID: "team-a", OrganizationID: "org-a", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-a/repo-a"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-a", TeamID: "team-a", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
			{
				EnrollmentID: "enr-b", TeamID: "team-b", OrganizationID: "org-b", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-b/repo-b"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-b", TeamID: "team-b", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
		},
	}

	// Pre-seed an aged-out permanent failure whose transcript is no longer discovered -> must still appear in Summary.Failed!
	rs := NewRouteState("acc1", "default", "enr-a", "team-a")
	rs.MarkFailed("claude", "s-aged-out-failure", now.Add(-100*24*time.Hour), "record too large", 1)
	if err := SaveState(stateDir, rs); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 1. Out-of-order subagent reach at T0+1m BEFORE main reach at T0+5m + subdir switch at T0+6m:
	//    T0+30s stays; T0+2m (between T1 and T5) AND T0+6m (segment 2 in repoASub) MUST BOTH DROP!
	seg1 := makeNativeSegment("claude", "s-ooo", repoA, "OOO_BEFORE_OK", t0.Add(30*time.Second))
	midRec := proto.CloneOf(seg1.NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords[0])
	midRec.TimeUnixNano = uint64(t0.Add(2 * time.Minute).UnixNano())
	midRec.ObservedTimeUnixNano = midRec.TimeUnixNano
	for _, kv := range midRec.Attributes {
		if kv.Key == "prompt" {
			kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "OOO_BETWEEN_T1_AND_T5_LEAK"}}
		}
	}
	seg1.NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords = append(seg1.NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords, midRec)
	seg1.RecordCount = 2
	// Main reach at T+5m appears BEFORE subagent reach at T+1m to the SAME orgB dir!
	seg1.ToolReaches = []agents.HistoryToolReach{
		{At: t0.Add(5 * time.Minute), Cwd: repoA, Paths: []string{secretB2}},
		{At: t0.Add(1 * time.Minute), Cwd: repoA, Paths: []string{secretB1}},
	}
	seg2Subdir := makeNativeSegment("claude", "s-ooo", repoASub, "SUBDIR_AFTER_CROSSING_LEAK", t0.Add(6*time.Minute))

	// 2. Deleted subdir inside own repoA MUST stay clean; deleted external path MUST fail closed!
	segOwnDeleted := makeNativeSegment("claude", "s-own-del", repoA, "OWN_DELETED_SUBDIR_OK", t0.Add(2*time.Minute))
	segOwnDeleted.ToolReaches = []agents.HistoryToolReach{
		{At: t0.Add(time.Minute), Cwd: repoA, Paths: []string{filepath.Join(repoA, "removed-dir", "gone.go")}},
	}
	extDeletedPath := filepath.Join("/workspace", "terma-nonexistent-deleted-orgb-repo", "secret.go")
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != filepath.Dir(home) {
		extDeletedPath = filepath.Join(home, "terma-nonexistent-deleted-orgb-repo-"+filepath.Base(stateDir), "secret.go")
	}
	segExtDeleted := makeNativeSegment("claude", "s-ext-del", repoA, "EXT_DELETED_SECRET", t0.Add(2*time.Minute))
	segExtDeleted.ToolReaches = []agents.HistoryToolReach{
		{At: t0.Add(time.Minute), Cwd: repoA, Paths: []string{extDeletedPath}},
	}

	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "s-ooo", ModTime: now.Add(-5 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-own-del", ModTime: now.Add(-4 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-ext-del", ModTime: now.Add(-3 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"s-ooo":     {seg1, seg2Subdir},
			"s-own-del": {segOwnDeleted},
			"s-ext-del": {segExtDeleted},
		},
	}

	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}, BytesPerSec: 1 << 30},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(received, "\n")
	if !strings.Contains(all, "OOO_BEFORE_OK") || !strings.Contains(all, "OWN_DELETED_SUBDIR_OK") {
		t.Errorf("legitimate records dropped unexpectedly")
	}
	for _, leak := range []string{"OOO_BETWEEN_T1_AND_T5_LEAK", "SUBDIR_AFTER_CROSSING_LEAK", "EXT_DELETED_SECRET"} {
		if strings.Contains(all, leak) {
			t.Fatalf("leaked %s upstream!", leak)
		}
	}
	if len(sum.Failed) != 1 || sum.Failed[0].SessionID != "s-aged-out-failure" || !sum.Failed[0].Previously {
		t.Errorf("aged-out permanent failure missing from summary: %+v", sum.Failed)
	}
}

func TestOtherTeamPolicyRefreshClaimingPersonalRepoAbortsRetry(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoClaimed := initTestGitRepo(t, "https://github.com/org-a/newly-claimed.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	stBefore := routing.State{
		StateID: "st-1", Generation: 1, ConsentGeneration: 1, Account: "acc1",
		Teams: []routing.Team{
			{
				EnrollmentID: "enr-a", TeamID: "team-a", OrganizationID: "org-a", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-a/other"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-a", TeamID: "team-a", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
			{
				EnrollmentID: "enr-p", TeamID: "team-p", OrganizationID: "org-p", Personal: true, AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeGlobal, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-p", TeamID: "team-p", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
		},
	}
	// Background refresh of Org A adds newly-claimed (bumps Generation only, NOT ConsentGeneration!).
	stAfter := stBefore
	stAfter.Generation = 2
	stAfter.Teams = append([]routing.Team(nil), stBefore.Teams...)
	stAfter.Teams[0].Policy.Repositories = []string{"github.com/org-a/other", "github.com/org-a/newly-claimed"}

	var mu sync.Mutex
	curState := stBefore
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		posts++
		p := posts
		mu.Unlock()
		if p == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	src := &stubHistorySource{
		source:   "claude",
		files:    []agents.HistoryFile{{SourceSystem: "claude", SessionID: "s-claim", ModTime: now.Add(-3 * time.Hour)}},
		segments: map[string][]agents.HistorySegment{"s-claim": {makeNativeSegment("claude", "s-claim", repoClaimed, "work", now.Add(-3*time.Hour))}},
	}
	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: stBefore,
		ReloadState: func() (routing.State, error) {
			mu.Lock()
			defer mu.Unlock()
			return curState, nil
		},
		Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport: &Transport{
			Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}},
			BytesPerSec: 1 << 30, InitialBackoff: time.Millisecond,
			Sleep: func(context.Context, time.Duration) error {
				mu.Lock()
				curState = stAfter
				mu.Unlock()
				return nil
			},
		},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if posts != 1 || sum.SentSessions != 0 || len(sum.TeamIssues) != 1 {
		t.Fatalf("expected retry abort after org-a claimed repo: posts=%d sum=%+v", posts, sum)
	}
}

func TestHeldSubagentUnroutedReachSpawnedChildAndHistoricalBranch(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/app.git")
	repoB := initTestGitRepo(t, "https://github.com/org-b/secret.git")
	scratchDir := t.TempDir()
	secretB := filepath.Join(repoB, "secret.go")
	if err := os.WriteFile(secretB, []byte("package secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-4 * time.Hour)

	// Org A is active; Org B is blocked but still holds its repository (Suspended: a
	// Refused team is passed over, so its repository would belong to no organization);
	// no Personal team selected.
	st := routing.State{
		StateID: "st-1", Generation: 1, ConsentGeneration: 1, Account: "acc1",
		Teams: []routing.Team{
			{
				EnrollmentID: "enr-a", TeamID: "team-a", OrganizationID: "org-a", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-a/app"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-a", TeamID: "team-a", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
			{
				EnrollmentID: "enr-b", TeamID: "team-b", OrganizationID: "org-b", AuthURL: "https://auth.example.com",
				Refusals: 1,
				Policy:   config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-b/secret"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-b", TeamID: "team-b", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
		},
	}

	var mu sync.Mutex
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 1. Held/blocked companion subagent in Org B must taint parent session in Org A.
	segMainBefore := makeNativeSegment("claude", "s-held-sub", repoA, "HELD_SUB_BEFORE_OK", t0.Add(30*time.Second))
	segMainBefore.FallbackBranch = "historical-feature-branch"
	segSubInBlockedB := makeNativeSegment("claude", "s-held-sub", repoB, "SUB_IN_B", t0.Add(time.Minute))
	segSubInBlockedB.Subagent = true
	segMainAfterSub := makeNativeSegment("claude", "s-held-sub", repoA, "HELD_SUB_AFTER_LEAK", t0.Add(2*time.Minute))

	// 2. Unrouted main segment (scratch outside git, no Personal team) reading Org B must taint later Org A segment.
	segUnrouted1 := makeNativeSegment("claude", "s-unrouted-reach", repoA, "UNROUTED_BEFORE_OK", t0.Add(30*time.Second))
	segUnroutedMid := makeNativeSegment("claude", "s-unrouted-reach", scratchDir, "IN_SCRATCH", t0.Add(time.Minute))
	segUnroutedMid.ToolReaches = []agents.HistoryToolReach{{At: t0.Add(time.Minute), Cwd: scratchDir, Paths: []string{secretB}}}
	segUnroutedAfter := makeNativeSegment("claude", "s-unrouted-reach", repoA, "UNROUTED_AFTER_LEAK", t0.Add(2*time.Minute))

	// 3. Spawned child rollout reading Org B must taint parent rollout's summary at T+2m.
	segParent1 := makeNativeSegment("codex", "s-parent", repoA, "PARENT_BEFORE_OK", t0.Add(30*time.Second))
	segParent2 := makeNativeSegment("codex", "s-parent", repoA, "PARENT_AFTER_CHILD_LEAK", t0.Add(2*time.Minute))
	segChild := makeNativeSegment("codex", "s-child", repoA, "CHILD_WORK", t0.Add(time.Minute))
	segChild.ToolReaches = []agents.HistoryToolReach{{At: t0.Add(time.Minute), Cwd: repoA, Paths: []string{secretB}}}

	// 4. Parent with an active spawned child rollout must skip as SkipActive.
	segActiveParent := makeNativeSegment("codex", "s-parent-active-child", repoA, "ACTIVE_CHILD_PARENT_LEAK", t0)

	srcClaude := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "s-held-sub", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-unrouted-reach", ModTime: now.Add(-3 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"s-held-sub":       {segMainBefore, segSubInBlockedB, segMainAfterSub},
			"s-unrouted-reach": {segUnrouted1, segUnroutedMid, segUnroutedAfter},
		},
	}
	srcCodex := &stubHistorySource{
		source: "codex",
		files: []agents.HistoryFile{
			{SourceSystem: "codex", SessionID: "s-parent", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "codex", SessionID: "s-child", ParentSessionID: "s-parent", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "codex", SessionID: "s-parent-active-child", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "codex", SessionID: "s-active-child", ParentSessionID: "s-parent-active-child", ModTime: now.Add(-5 * time.Minute), Active: true},
		},
		segments: map[string][]agents.HistorySegment{
			"s-parent":              {segParent1, segParent2},
			"s-child":               {segChild},
			"s-parent-active-child": {segActiveParent},
		},
	}

	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{srcClaude, srcCodex}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}, BytesPerSec: 1 << 30},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(received, "\n")
	for _, want := range []string{"HELD_SUB_BEFORE_OK", "UNROUTED_BEFORE_OK", "PARENT_BEFORE_OK", "historical-feature-branch"} {
		if !strings.Contains(all, want) {
			t.Errorf("expected %q in uploaded payloads", want)
		}
	}
	for _, leak := range []string{"HELD_SUB_AFTER_LEAK", "UNROUTED_AFTER_LEAK", "PARENT_AFTER_CHILD_LEAK", "ACTIVE_CHILD_PARENT_LEAK"} {
		if strings.Contains(all, leak) {
			t.Fatalf("leaked %q upstream!", leak)
		}
	}
	if sum.SkippedSessions[SkipActive] < 2 {
		t.Errorf("expected both active child and its parent skipped as active, got SkippedSessions=%+v", sum.SkippedSessions)
	}
}

func TestSpawnFamilyChildSiblingOrphanDeletedShellTargetGitDashCAndFetchedAtTick(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/app.git")
	repoB := initTestGitRepo(t, "https://github.com/org-b/secret.git")
	secretB := filepath.Join(repoB, "secret.go")
	if err := os.WriteFile(secretB, []byte("package secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-4 * time.Hour)

	stBefore := routing.State{
		StateID: "st-1", Generation: 1, ConsentGeneration: 1, Account: "acc1",
		Teams: []routing.Team{
			{
				EnrollmentID: "enr-a", TeamID: "team-a", OrganizationID: "org-a", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-a/app"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-a", TeamID: "team-a", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
			{
				EnrollmentID: "enr-b", TeamID: "team-b", OrganizationID: "org-b", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-b/secret"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-b", TeamID: "team-b", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
		},
	}
	// Routine 1-minute TeamRefresher tick: bumps Generation and FetchedAt ONLY (no policy/repo change).
	stTick := stBefore
	stTick.Generation = 2
	stTick.Teams = append([]routing.Team(nil), stBefore.Teams...)
	stTick.Teams[0].Policy.FetchedAt = now.Add(time.Minute)

	var mu sync.Mutex
	curState := stBefore
	posts := 0
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts++
		p := posts
		mu.Unlock()
		if p == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		mu.Lock()
		received = append(received, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 1. Parent reads Org B at T+1m then spawns child at T+2m in Org A -> child MUST DROP!
	segPReadB := makeNativeSegment("codex", "s-p-read-b", repoA, "P_BEFORE_B_OK", t0.Add(30*time.Second))
	segPReadB.ToolReaches = []agents.HistoryToolReach{{At: t0.Add(time.Minute), Cwd: repoA, Paths: []string{secretB}}}
	segChildOfB := makeNativeSegment("codex", "s-c-of-b", repoA, "CHILD_QUOTING_B_LEAK", t0.Add(2*time.Minute))

	// 2. Sibling 1 reads Org B at T+1m; Sibling 2 runs in Org A at T+2m -> Sibling 2 MUST DROP!
	segSibRoot := makeNativeSegment("codex", "s-sib-root", repoA, "SIB_ROOT_OK", t0.Add(15*time.Second))
	segSib1 := makeNativeSegment("codex", "s-sib-1", repoA, "SIB1_LEAK", t0.Add(time.Minute))
	segSib1.ToolReaches = []agents.HistoryToolReach{{At: t0.Add(time.Minute), Cwd: repoA, Paths: []string{secretB}}}
	segSib2 := makeNativeSegment("codex", "s-sib-2", repoA, "SIB2_LEAK", t0.Add(2*time.Minute))

	// 3. Orphan child whose parent rollout is missing from disk -> fails closed!
	segOrphan := makeNativeSegment("codex", "s-orphan", repoA, "ORPHAN_LEAK", t0.Add(time.Minute))

	// 4. `git -C . status` stays clean (no IFS false positive); deleted external target via shell fails closed!
	segGitC := makeNativeSegment("codex", "s-git-c", repoA, "GIT_DASH_C_OK", t0.Add(2*time.Minute))
	segGitC.ToolReaches = []agents.HistoryToolReach{{At: t0.Add(time.Minute), Cwd: repoA, Paths: hookrun.Alone(hookrun.ShellPathsHistorical("git -C . status", repoA)...)}}

	deletedExtTarget := filepath.Join("/workspace", "deleted-b-worktree", "notes")
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != filepath.Dir(home) {
		deletedExtTarget = filepath.Join(home, "terma-nonexistent-deleted-b-"+filepath.Base(stateDir), "notes")
	}
	segDelShell := makeNativeSegment("codex", "s-del-shell", repoA, "DELETED_SHELL_TARGET_LEAK", t0.Add(2*time.Minute))
	segDelShell.ToolReaches = []agents.HistoryToolReach{{At: t0.Add(time.Minute), Cwd: repoA, Paths: hookrun.Alone(hookrun.ShellPathsHistorical("grep TODO "+deletedExtTarget, repoA)...)}}

	src := &stubHistorySource{
		source: "codex",
		files: []agents.HistoryFile{
			{SourceSystem: "codex", SessionID: "s-p-read-b", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "codex", SessionID: "s-c-of-b", ParentSessionID: "s-p-read-b", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "codex", SessionID: "s-sib-root", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "codex", SessionID: "s-sib-1", ParentSessionID: "s-sib-root", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "codex", SessionID: "s-sib-2", ParentSessionID: "s-sib-root", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "codex", SessionID: "s-orphan", ParentSessionID: "s-deleted-parent", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "codex", SessionID: "s-git-c", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "codex", SessionID: "s-del-shell", ModTime: now.Add(-3 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"s-p-read-b":  {segPReadB},
			"s-c-of-b":    {segChildOfB},
			"s-sib-root":  {segSibRoot},
			"s-sib-1":     {segSib1},
			"s-sib-2":     {segSib2},
			"s-orphan":    {segOrphan},
			"s-git-c":     {segGitC},
			"s-del-shell": {segDelShell},
		},
	}

	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: stBefore,
		ReloadState: func() (routing.State, error) {
			mu.Lock()
			defer mu.Unlock()
			return curState, nil
		},
		Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport: &Transport{
			Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}},
			BytesPerSec: 1 << 30, InitialBackoff: time.Millisecond,
			Sleep: func(context.Context, time.Duration) error {
				mu.Lock()
				curState = stTick
				mu.Unlock()
				return nil
			},
		},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.TeamIssues) != 0 {
		t.Fatalf("routine FetchedAt-only refresh must not abort route; got TeamIssues=%+v", sum.TeamIssues)
	}
	all := strings.Join(received, "\n")
	for _, want := range []string{"P_BEFORE_B_OK", "SIB_ROOT_OK", "GIT_DASH_C_OK"} {
		if !strings.Contains(all, want) {
			t.Errorf("expected %q in uploaded payloads", want)
		}
	}
	for _, leak := range []string{"CHILD_QUOTING_B_LEAK", "SIB1_LEAK", "SIB2_LEAK", "ORPHAN_LEAK", "DELETED_SHELL_TARGET_LEAK"} {
		if strings.Contains(all, leak) {
			t.Fatalf("leaked %q upstream!", leak)
		}
	}
}

func TestUnroutedMainWithSubagentsResumeAcrossOrgsAndCompanionError(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/app.git")
	repoB := initTestGitRepo(t, "https://github.com/org-b/other.git")
	if err := os.WriteFile(filepath.Join(repoA, "secret-a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoA, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	day1 := now.Add(-48 * time.Hour)
	day2 := now.Add(-24 * time.Hour)

	// Org A and Org B active; NO Personal team selected.
	st := routing.State{
		StateID: "st-1", Generation: 1, ConsentGeneration: 1, Account: "acc1",
		Teams: []routing.Team{
			{
				EnrollmentID: "enr-a", TeamID: "team-a", OrganizationID: "org-a", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-a/app"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-a", TeamID: "team-a", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
			{
				EnrollmentID: "enr-b", TeamID: "team-b", OrganizationID: "org-b", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-b/other"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-b", TeamID: "team-b", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
		},
	}

	var mu sync.Mutex
	byTeam := map[string][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		byTeam[r.Header.Get("X-Mirador-Project")] = append(byTeam[r.Header.Get("X-Mirador-Project")], string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 1. Unrouted main in scratch reads Org A at T+1m then spawns subagent in Org B at T+2m -> subagent MUST DROP!
	s1Main := makeNativeSegment("claude", "s-unrouted-main-sub", scratch, "MAIN_SCRATCH", day1)
	s1Main.ToolReaches = []agents.HistoryToolReach{{At: day1.Add(time.Minute), Cwd: scratch, Paths: []string{filepath.Join(repoA, "secret-a.txt")}}}
	s1Sub := makeNativeSegment("claude", "s-unrouted-main-sub", repoB, "UNROUTED_PARENT_SUB_B_LEAK", day1.Add(2*time.Minute))
	s1Sub.Subagent = true

	// 2. Unrouted main in scratch spawns sibling subagents in Org A (T+1m) and Org B (T+2m) -> Org B sibling MUST DROP!
	s2Main := makeNativeSegment("claude", "s-sib-sub", scratch, "MAIN_SCRATCH", day1)
	s2SubA := makeNativeSegment("claude", "s-sib-sub", repoA, "SIB_SUB_A_BEFORE_OK", day1.Add(time.Minute))
	s2SubA.Subagent = true
	s2SubB := makeNativeSegment("claude", "s-sib-sub", repoB, "SIB_SUB_B_CROSS_LEAK", day1.Add(2*time.Minute))
	s2SubB.Subagent = true

	// 3. Deleted main cwd spawns subagent in Org B -> fails closed, subagent MUST DROP!
	s3Main := makeNativeSegment("claude", "s-del-main-sub", filepath.Join(t.TempDir(), "gone-repo"), "GONE", day1)
	s3SubB := makeNativeSegment("claude", "s-del-main-sub", repoB, "DEL_MAIN_SUB_B_LEAK", day1.Add(time.Minute))
	s3SubB.Subagent = true

	// 4. Parent + child finish in Org A on Day 1; parent resumes in Org B on Day 2. A family
	// with subagents must sit in one organization, so Day 1 uploads and Day 2 is withheld.
	s4MainA := makeNativeSegment("claude", "s-resumed-with-sub", repoA, "DAY1_MAIN_A_OK", day1)
	s4SubA := makeNativeSegment("claude", "s-resumed-with-sub", repoA, "DAY1_SUB_A_OK", day1.Add(time.Minute))
	s4SubA.Subagent = true
	s4MainB := makeNativeSegment("claude", "s-resumed-with-sub", repoB, "DAY2_MAIN_B_LEAK", day2)

	// 5. A missing path inside the segment's own checkout (`grep 'go.mod/v2' docs.md`) stays clean.
	s5Clean := makeNativeSegment("claude", "s-route-regex", repoA, "ROUTE_REGEX_CLEAN_OK", day1.Add(5*time.Minute))
	s5Clean.ToolReaches = []agents.HistoryToolReach{
		{At: day1.Add(2 * time.Minute), Cwd: repoA, Paths: hookrun.Alone(hookrun.ShellPathsHistorical("grep 'go.mod/v2' docs.md", repoA)...)},
	}

	// 6. CompanionReadError fails closed as SkipUnreadable!
	s6Err := makeNativeSegment("claude", "s-comp-err", repoA, "COMP_ERR_LEAK", day1)

	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "s-unrouted-main-sub", ModTime: day1},
			{SourceSystem: "claude", SessionID: "s-sib-sub", ModTime: day1},
			{SourceSystem: "claude", SessionID: "s-del-main-sub", ModTime: day1},
			{SourceSystem: "claude", SessionID: "s-resumed-with-sub", ModTime: day2},
			{SourceSystem: "claude", SessionID: "s-route-regex", ModTime: day1},
			{SourceSystem: "claude", SessionID: "s-comp-err", ModTime: day1, CompanionReadError: "/nonexistent/subagents"},
		},
		segments: map[string][]agents.HistorySegment{
			"s-unrouted-main-sub": {s1Main, s1Sub},
			"s-sib-sub":           {s2Main, s2SubA, s2SubB},
			"s-del-main-sub":      {s3Main, s3SubB},
			"s-resumed-with-sub":  {s4MainA, s4SubA, s4MainB},
			"s-route-regex":       {s5Clean},
			"s-comp-err":          {s6Err},
		},
	}

	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}, BytesPerSec: 1 << 30},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	gotA := strings.Join(byTeam["team-a"], "\n")
	gotB := strings.Join(byTeam["team-b"], "\n")
	for _, want := range []string{"SIB_SUB_A_BEFORE_OK", "DAY1_MAIN_A_OK", "DAY1_SUB_A_OK", "ROUTE_REGEX_CLEAN_OK"} {
		if !strings.Contains(gotA, want) {
			t.Errorf("expected %q uploaded to team-a", want)
		}
	}
	all := gotA + "\n" + gotB
	for _, leak := range []string{"UNROUTED_PARENT_SUB_B_LEAK", "SIB_SUB_B_CROSS_LEAK", "DEL_MAIN_SUB_B_LEAK", "COMP_ERR_LEAK", "DAY2_MAIN_B_LEAK"} {
		if strings.Contains(all, leak) {
			t.Fatalf("leaked %q upstream!", leak)
		}
	}
	if sum.SkippedSessions[SkipUnreadable] != 1 {
		t.Errorf("expected 1 unreadable skip for CompanionReadError, got %+v", sum.SkippedSessions)
	}
	if len(sum.Unreadable) != 1 || sum.Unreadable[0] != "/nonexistent/subagents" {
		t.Errorf("Summary.Unreadable = %v, want the companion path", sum.Unreadable)
	}
}
