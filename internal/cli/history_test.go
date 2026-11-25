package cli

import (
	"encoding/json"
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

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/history"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

func TestParseHistorySince(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	got, err := parseHistorySince("90d", now)
	if err != nil || !got.Equal(now.Add(-90*24*time.Hour)) {
		t.Errorf("90d = (%v, %v)", got, err)
	}
	got, err = parseHistorySince("48h", now)
	if err != nil || !got.Equal(now.Add(-48*time.Hour)) {
		t.Errorf("48h = (%v, %v)", got, err)
	}
	got, err = parseHistorySince("2026-08-01", now)
	if err != nil || !got.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("2026-08-01 = (%v, %v)", got, err)
	}
	for _, bad := range []string{"", "0d", "-5d", "-1h", "nonsense"} {
		if _, err := parseHistorySince(bad, now); err == nil {
			t.Errorf("expected error for invalid --since %q", bad)
		}
	}
}

func TestHistoryUploadCLIDryRunAndEndToEndBothHarnesses(t *testing.T) {
	// Sandboxed config/state/agent directories.
	cfgDir := t.TempDir()
	stateDir := t.TempDir()
	useDirs(t, cfgDir, stateDir)
	claudeDir := t.TempDir()
	codexDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Setenv("CODEX_HOME", codexDir)
	t.Setenv("TERMA_API_KEY", "")
	t.Setenv("TERMA_ENV", "")
	t.Setenv("TERMA_PROFILE", "")

	// Two git repos routed to Team A (Org A) and Team B (Org B).
	repoA := t.TempDir()
	repoB := t.TempDir()
	for _, pair := range [][2]string{
		{repoA, "https://github.com/acme/repo-a.git"},
		{repoB, "https://github.com/beta/repo-b.git"},
	} {
		for _, args := range [][]string{{"init", "-q", pair[0]}, {"-C", pair[0], "remote", "add", "origin", pair[1]}} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
	}

	var mu sync.Mutex
	var holdingsCalls int
	receivedByProject := map[string][]*logspb.LogsData{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ai/sessions":
			mu.Lock()
			holdingsCalls++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"project_id": r.Header.Get(sendas.ProjectHeader),
				"sessions":   []any{},
				"pagination": map[string]any{"page": 1, "per_page": 200, "total": 0, "total_pages": 1},
			})
		case "/v1/logs":
			body, _ := io.ReadAll(r.Body)
			var logs logspb.LogsData
			if err := proto.Unmarshal(body, &logs); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			proj := r.Header.Get(sendas.ProjectHeader)
			mu.Lock()
			receivedByProject[proj] = append(receivedByProject[proj], &logs)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("TERMA_API_URL", srv.URL)
	t.Setenv("TERMA_AUTH_URL", srv.URL)
	t.Setenv("TERMA_APP_URL", srv.URL)
	t.Setenv("TERMA_OTLP_URL", srv.URL)

	// Sign in and write routing state for two org teams.
	cred := storedSession(&fakeAuth{srv: srv})
	if err := saveSignIn(cfgDir, cred); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rtState := routing.State{
		Account: cred.Account(),
		Teams: []routing.Team{
			{
				EnrollmentID: "enr-a", TeamID: "team-a", OrganizationID: "org-a", AuthURL: srv.URL,
				Policy: config.Policy{
					Mode: config.ModeRepo, Repositories: []string{"github.com/acme/repo-a"},
					IncludePrompts: true, IncludeToolContent: true,
					OrganizationID: "org-a", TeamID: "team-a", AuthURL: srv.URL, FetchedAt: now,
				},
			},
			{
				EnrollmentID: "enr-b", TeamID: "team-b", OrganizationID: "org-b", AuthURL: srv.URL,
				Policy: config.Policy{
					Mode: config.ModeRepo, Repositories: []string{"github.com/beta/repo-b"},
					IncludePrompts: true, IncludeToolContent: true,
					OrganizationID: "org-b", TeamID: "team-b", AuthURL: srv.URL, FetchedAt: now,
				},
			},
		},
		Bindings: map[string]routing.Binding{
			"github.com/acme/repo-a": {Organization: "org-a"},
			"github.com/beta/repo-b": {Organization: "org-b"},
		},
	}
	if _, err := routing.Replace(stateDir, config.DefaultProfile, nil, cred.Account(), func(s *routing.State) error {
		s.Teams = rtState.Teams
		s.Bindings = rtState.Bindings
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	oldTime := now.Add(-4 * time.Hour)
	// Write a Claude Code transcript in repoA.
	ccPath := filepath.Join(claudeDir, "projects", "-acme-repo-a", "11111111-1111-1111-1111-111111111111.jsonl")
	if err := os.MkdirAll(filepath.Dir(ccPath), 0o755); err != nil {
		t.Fatal(err)
	}
	ccLines := strings.Join([]string{
		`{"type":"user","uuid":"u1","promptId":"p1","sessionId":"11111111-1111-1111-1111-111111111111","cwd":` + jsonQuote(repoA) + `,"version":"2.1.301","timestamp":"` + oldTime.Format(time.RFC3339Nano) + `","message":{"role":"user","content":"claude prompt repo a"}}`,
		`{"type":"assistant","uuid":"a1","sessionId":"11111111-1111-1111-1111-111111111111","cwd":` + jsonQuote(repoA) + `,"version":"2.1.301","timestamp":"` + oldTime.Add(time.Second).Format(time.RFC3339Nano) + `","requestId":"req_cc_1","message":{"id":"msg_1","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn","content":[{"type":"text","text":"claude reply repo a"}],"usage":{"input_tokens":80,"output_tokens":20,"cache_read_input_tokens":10,"cache_creation_input_tokens":0}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(ccPath, []byte(ccLines), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(ccPath, oldTime, oldTime)

	// Write a Codex rollout in repoB.
	cxPath := filepath.Join(codexDir, "sessions", "2026", "11", "25", "rollout-2026-11-25T08-00-00-22222222-2222-2222-2222-222222222222.jsonl")
	if err := os.MkdirAll(filepath.Dir(cxPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cxLines := strings.Join([]string{
		`{"timestamp":"` + oldTime.Format(time.RFC3339Nano) + `","type":"session_meta","payload":{"id":"22222222-2222-2222-2222-222222222222","timestamp":"` + oldTime.Format(time.RFC3339Nano) + `","cwd":` + jsonQuote(repoB) + `,"originator":"codex_cli_rs","cli_version":"0.159.3","model_provider":"OpenAI"}}`,
		`{"timestamp":"` + oldTime.Add(time.Second).Format(time.RFC3339Nano) + `","type":"turn_context","payload":{"turn_id":"t1","cwd":` + jsonQuote(repoB) + `,"model":"gpt-6-astra","effort":"high"}}`,
		`{"timestamp":"` + oldTime.Add(2*time.Second).Format(time.RFC3339Nano) + `","type":"event_msg","payload":{"type":"user_message","message":"codex prompt repo b"}}`,
		`{"timestamp":"` + oldTime.Add(3*time.Second).Format(time.RFC3339Nano) + `","type":"token_usage_record","payload":{"usage":{"input_tokens":1000,"cached_input_tokens":200,"output_tokens":50,"reasoning_output_tokens":10,"total_tokens":1050}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(cxPath, []byte(cxLines), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(cxPath, oldTime, oldTime)

	// 1. Dry-run JSON: lists both teams, zero network calls, zero state writes.
	out, _, err := termaRun{}.exec(t, "history", "upload", "--dry-run", "-o", "json")
	if err != nil {
		t.Fatalf("dry-run failed: %v\n%s", err, out)
	}
	var drySum history.Summary
	if err := json.Unmarshal([]byte(out), &drySum); err != nil {
		t.Fatalf("unmarshal dry-run json: %v\n%s", err, out)
	}
	if !drySum.DryRun || len(drySum.Planned) != 2 {
		t.Fatalf("dry-run planned = %+v, want 2 routes", drySum)
	}
	if holdingsCalls != 0 || len(receivedByProject) != 0 {
		t.Fatalf("dry-run touched network: holdings=%d otlp=%d", holdingsCalls, len(receivedByProject))
	}
	if _, err := os.Stat(filepath.Join(stateDir, history.Dir)); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote state dir: %v", err)
	}

	// 2. Real upload: sends Claude session to team-a and Codex session to team-b, both tagged historical=true.
	out, _, err = termaRun{}.exec(t, "history", "upload", "--since", "90d")
	if err != nil {
		t.Fatalf("upload failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Sent: 2 session(s)") {
		t.Errorf("unexpected upload summary:\n%s", out)
	}
	if holdingsCalls != 2 {
		t.Errorf("holdingsCalls = %d, want 2 (once per destination team)", holdingsCalls)
	}
	if len(receivedByProject["team-a"]) != 1 || len(receivedByProject["team-b"]) != 1 {
		t.Fatalf("receivedByProject counts: team-a=%d team-b=%d, want 1/1", len(receivedByProject["team-a"]), len(receivedByProject["team-b"]))
	}
	// Verify historical marker on every uploaded record.
	for proj, batches := range receivedByProject {
		for _, b := range batches {
			for _, rl := range b.ResourceLogs {
				for _, sl := range rl.ScopeLogs {
					for _, lr := range sl.LogRecords {
						hasHist := false
						for _, kv := range lr.Attributes {
							if kv.Key == semconv.TermaSessionHistoricalKey && kv.Value.GetBoolValue() {
								hasHist = true
							}
						}
						if !hasHist {
							t.Errorf("%s record missing %s=true", proj, semconv.TermaSessionHistoricalKey)
						}
					}
				}
			}
		}
	}

	// 3. Repeat run: sends nothing (0 new OTLP requests, 0 new holdings requests).
	prevHoldings := holdingsCalls
	out, _, err = termaRun{}.exec(t, "history", "upload")
	if err != nil {
		t.Fatalf("repeat upload failed: %v\n%s", err, out)
	}
	if holdingsCalls != prevHoldings || len(receivedByProject["team-a"]) != 1 || len(receivedByProject["team-b"]) != 1 {
		t.Fatalf("repeat run sent unexpected requests: holdings=%d->%d", prevHoldings, holdingsCalls)
	}
	if !strings.Contains(out, "Sent: 0 session(s)") || !strings.Contains(out, "already_done=2") {
		t.Errorf("repeat run summary unexpected:\n%s", out)
	}
}

func TestHistoryUploadFailsClosedWithoutRoutingState(t *testing.T) {
	useDirs(t, t.TempDir(), t.TempDir())
	_, _, err := termaRun{}.exec(t, "history", "upload", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "terma setup") {
		t.Fatalf("expected fail-closed error naming terma setup, got: %v", err)
	}
}

// git answers for GIT_DIR's repository whatever folder is asked about, so history upload
// refuses to route sessions while it is set.
func TestHistoryUploadRefusesGitRepositoryOverride(t *testing.T) {
	useDirs(t, t.TempDir(), t.TempDir())
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), ".git"))
	_, _, err := termaRun{}.exec(t, "history", "upload", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "GIT_DIR is set") {
		t.Fatalf("expected a refusal naming GIT_DIR, got: %v", err)
	}
}

func TestHistoryUploadExitsNonZeroOnHoldingsFailure(t *testing.T) {
	cfgDir := t.TempDir()
	stateDir := t.TempDir()
	useDirs(t, cfgDir, stateDir)
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_API_KEY", "")
	t.Setenv("TERMA_ENV", "")
	t.Setenv("TERMA_PROFILE", "")

	repoA := t.TempDir()
	for _, args := range [][]string{{"init", "-q", repoA}, {"-C", repoA, "remote", "add", "origin", "https://github.com/org-a/repo-a.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer apiSrv.Close()

	t.Setenv("TERMA_API_URL", apiSrv.URL)
	t.Setenv("TERMA_AUTH_URL", apiSrv.URL)
	t.Setenv("TERMA_APP_URL", apiSrv.URL)
	t.Setenv("TERMA_OTLP_URL", apiSrv.URL)

	cred := storedSession(&fakeAuth{srv: apiSrv})
	if err := saveSignIn(cfgDir, cred); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := routing.Replace(stateDir, config.DefaultProfile, nil, cred.Account(), func(s *routing.State) error {
		s.Teams = []routing.Team{{
			EnrollmentID: "enr-a", TeamID: "team-a", OrganizationID: "org-a", AuthURL: apiSrv.URL,
			Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-a/repo-a"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-a", TeamID: "team-a", AuthURL: apiSrv.URL, FetchedAt: now},
		}}
		s.Bindings = map[string]routing.Binding{"github.com/org-a/repo-a": {Organization: "org-a"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	oldTime := now.Add(-3 * time.Hour)
	clPath := filepath.Join(claudeDir, "projects", "-repo-a", "sess-503.jsonl")
	if err := os.MkdirAll(filepath.Dir(clPath), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","uuid":"u1","sessionId":"sess-503","cwd":` + jsonQuote(repoA) + `,"version":"2.1.0","timestamp":"` + oldTime.Format(time.RFC3339Nano) + `","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(clPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(clPath, oldTime, oldTime)

	out, _, err := termaRun{}.exec(t, "history", "upload")
	if code, ok := exitCodeOf(err); !ok || code != 1 {
		t.Fatalf("expected exit code 1 on holdings 503 failure, got err=%v\nstdout:\n%s", err, out)
	}
	if !strings.Contains(out, "Team issues:") || !strings.Contains(out, "holdings_check_failed=1") {
		t.Errorf("expected summary with Team issues and holdings_check_failed=1:\n%s", out)
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestRenderHistorySummaryNamesUnreadablePaths(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	renderHistorySummary(&b, history.Summary{
		SkippedSessions: map[string]int{history.SkipUnreadable: 4},
		WithheldRecords: 7,
		Unreadable:      []string{"/home/u/.codex/sessions/2026/rollout-x.jsonl", "/home/u/.claude/projects/p/s/subagents"},
	})
	want := "Could not read: /home/u/.codex/sessions/2026/rollout-x.jsonl, /home/u/.claude/projects/p/s/subagents (sessions that may depend on them were skipped)\n"
	want = "Withheld after a Mixed cutoff: 7 record(s)\n" + want
	if !strings.Contains(b.String(), "Skipped sessions: unreadable=4\n"+want) {
		t.Errorf("summary output:\n%s\nwant it to contain:\n%s", b.String(), want)
	}
}
