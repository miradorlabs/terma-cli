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

	"github.com/miradorlabs/terma-cli/internal/account/auth"
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
	got, err = parseHistorySince("40000d", now)
	if err != nil || !got.Equal(now.Add(-time.Duration(40000)*24*time.Hour)) {
		t.Errorf("40000d = (%v, %v)", got, err)
	}
	got, err = parseHistorySince("48h", now)
	if err != nil || !got.Equal(now.Add(-48*time.Hour)) {
		t.Errorf("48h = (%v, %v)", got, err)
	}
	got, err = parseHistorySince("2026-08-01", now)
	if err != nil || !got.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("2026-08-01 = (%v, %v)", got, err)
	}
	wantErrs := map[string]string{
		"":         "--since cannot be empty",
		"0d":       `invalid --since "0d": expected a positive number of days (e.g. 90d)`,
		"-1d":      `invalid --since "-1d": expected a positive number of days (e.g. 90d)`,
		"-5d":      `invalid --since "-5d": expected a positive number of days (e.g. 90d)`,
		"abcd":     `invalid --since "abcd": expected a positive number of days (e.g. 90d)`,
		"0":        `invalid --since "0": duration must be positive`,
		"-1h":      `invalid --since "-1h": duration must be positive`,
		"nonsense": `invalid --since "nonsense": use a duration like 90d or 48h, or a date like 2026-08-01`,
	}
	for spec, want := range wantErrs {
		if _, err := parseHistorySince(spec, now); err == nil || err.Error() != want {
			t.Errorf("parseHistorySince(%q) err = %v, want %q", spec, err, want)
		}
	}
}

// historyFixture is a signed-in machine with two teams in two organizations, a Claude Code
// session in team A's repository and a Codex session in team B's, both four hours old, and
// a fake API that holds nothing and records what it is sent.
type historyFixture struct {
	cfgDir    string
	stateDir  string
	claudeDir string
	codexDir  string
	repoA     string
	repoB     string
	srv       *httptest.Server
	ccPath    string
	cxPath    string
	account   string
	mu        sync.Mutex
	holdings  int
	received  map[string][]*logspb.LogsData
	// held, holdingsStatus, holdingsBody, logsStatus, logsBody, refreshStatus, refreshBody,
	// and onHoldings/onLogs change what the fake API answers.
	held           map[string][]map[string]any
	holdingsStatus map[string]int
	holdingsBody   map[string]string
	logsStatus     map[string]int
	logsBody       map[string]string
	refreshStatus  int
	refreshBody    string
	onHoldings     func(proj string)
	onLogs         func(proj string)
}

// answer sets what the fake API answers from now on.
func (f *historyFixture) answer(set func(f *historyFixture)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	set(f)
}

func (f *historyFixture) holdingsCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.holdings
}

func (f *historyFixture) receivedBy(project string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received[project])
}

func newHistoryFixture(t *testing.T) *historyFixture {
	t.Helper()
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

	fx := &historyFixture{
		cfgDir:    cfgDir,
		stateDir:  stateDir,
		claudeDir: claudeDir,
		codexDir:  codexDir,
		repoA:     repoA,
		repoB:     repoB,
		received:  map[string][]*logspb.LogsData{},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/user/token":
			fx.mu.Lock()
			status, body := fx.refreshStatus, fx.refreshBody
			fx.mu.Unlock()
			if status == 0 {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if body != "" {
				_, _ = io.WriteString(w, body)
			}
		case "/v1/ai/sessions":
			proj := r.Header.Get(sendas.ProjectHeader)
			fx.mu.Lock()
			fx.holdings++
			status, body, held, hook := fx.holdingsStatus[proj], fx.holdingsBody[proj], fx.held[proj], fx.onHoldings
			fx.mu.Unlock()
			if hook != nil {
				hook(proj)
			}
			if status != 0 {
				if body != "" {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(status)
				if body != "" {
					_, _ = io.WriteString(w, body)
				}
				return
			}
			if held == nil {
				held = []map[string]any{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"project_id": proj,
				"sessions":   held,
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
			fx.mu.Lock()
			status, respBody, hook := fx.logsStatus[proj], fx.logsBody[proj], fx.onLogs
			if status == 0 {
				fx.received[proj] = append(fx.received[proj], &logs)
			}
			fx.mu.Unlock()
			if hook != nil {
				hook(proj)
			}
			if status != 0 {
				if respBody != "" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, respBody)
					return
				}
				http.Error(w, "refused", status)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	fx.srv = srv

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
	}
	if _, err := routing.Replace(stateDir, config.DefaultProfile, nil, cred.Account(), func(s *routing.State) error {
		s.Teams = rtState.Teams
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
	fx.ccPath, fx.cxPath, fx.account = ccPath, cxPath, cred.Account()
	return fx
}

func TestHistoryUploadCLIDryRunAndEndToEndBothHarnesses(t *testing.T) {
	fx := newHistoryFixture(t)
	stateDir := fx.stateDir

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
	if fx.holdingsCalls() != 0 || fx.receivedBy("team-a")+fx.receivedBy("team-b") != 0 {
		t.Fatalf("dry-run touched network: holdings=%d", fx.holdingsCalls())
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
	if fx.holdingsCalls() != 2 {
		t.Errorf("holdingsCalls = %d, want 2 (once per destination team)", fx.holdingsCalls())
	}
	if fx.receivedBy("team-a") != 1 || fx.receivedBy("team-b") != 1 {
		t.Fatalf("receivedByProject counts: team-a=%d team-b=%d, want 1/1", fx.receivedBy("team-a"), fx.receivedBy("team-b"))
	}
	// Verify historical marker on every uploaded record.
	for proj, batches := range fx.received {
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
	prevHoldings := fx.holdingsCalls()
	out, _, err = termaRun{}.exec(t, "history", "upload")
	if err != nil {
		t.Fatalf("repeat upload failed: %v\n%s", err, out)
	}
	if fx.holdingsCalls() != prevHoldings || fx.receivedBy("team-a") != 1 || fx.receivedBy("team-b") != 1 {
		t.Fatalf("repeat run sent unexpected requests: holdings=%d->%d", prevHoldings, fx.holdingsCalls())
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

// A failed session, from this run or an earlier one, comes with how to try it again; a run
// with none does not mention it.
func TestRenderHistorySummaryNamesRetryFailed(t *testing.T) {
	t.Parallel()
	const hint = "Run `terma history upload --retry-failed` to try failed sessions again."
	for name, sum := range map[string]history.Summary{
		"failed now":        {Failed: []history.FailedSession{{SessionID: "s-1", Reason: "HTTP 400"}}},
		"failed previously": {SkippedSessions: map[string]int{history.SkipFailedPreviously: 1}},
	} {
		var b strings.Builder
		renderHistorySummary(&b, sum)
		if !strings.Contains(b.String(), hint) {
			t.Errorf("%s: summary does not say how to retry:\n%s", name, b.String())
		}
	}
	var b strings.Builder
	renderHistorySummary(&b, history.Summary{SentSessions: 1})
	if strings.Contains(b.String(), "--retry-failed") {
		t.Errorf("summary without failures mentions --retry-failed:\n%s", b.String())
	}
}

func TestHistoryUploadHasRetryFailedFlag(t *testing.T) {
	useDirs(t, t.TempDir(), t.TempDir())
	help, _, err := termaRun{}.exec(t, "history", "upload", "--help")
	if err != nil || !strings.Contains(help, "--retry-failed") {
		t.Fatalf("`terma history upload --help` (%v) does not list --retry-failed:\n%s", err, help)
	}
}

// Rechecks during an upload read the routing state of the profile the upload began with:
// a switch of the active profile stops them.
func TestReloadHistoryStateFailsWhenProfileChanges(t *testing.T) {
	useDirs(t, t.TempDir(), t.TempDir())
	t.Setenv("TERMA_PROFILE", "")
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	lane := testApp.profileLane(cfg)
	_, gen := testApp.signInSource(cfg).Lane()
	if _, err := testApp.reloadHistoryState(cfg.ProfileName, lane, gen, false); err == nil || strings.Contains(err.Error(), "profile changed") {
		t.Fatalf("same profile, no routing state: %v, want a routing-state error", err)
	}
	t.Setenv("TERMA_PROFILE", "other")
	if _, err := testApp.reloadHistoryState(cfg.ProfileName, lane, gen, false); err == nil || !strings.Contains(err.Error(), "active profile changed") {
		t.Fatalf("after a profile switch: %v, want active profile changed", err)
	}
}

// A switch to another person and back between two rechecks leaves the lane the same but
// moves the sign-in's generation, so the upload stops, as the relay loses a retry that
// spans one.
func TestReloadHistoryStateFailsWhenTheSignInChangedAndCameBack(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	lane := testApp.profileLane(cfg)
	_, gen := testApp.signInSource(cfg).Lane()
	if lane != auth.AccountKey(f.srv.URL, "u-1") {
		t.Fatalf("lane = %q, want u-1's", lane)
	}
	if _, err := routing.Replace(testApp.stateDir, cfg.ProfileName, nil, lane, func(*routing.State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := testApp.reloadHistoryState(cfg.ProfileName, lane, gen, false); err != nil {
		t.Fatalf("nothing changed: %v, want the routing state", err)
	}
	if err := saveSignIn(testApp.dir, storedSessionOf(f, "u-2")); err != nil {
		t.Fatal(err)
	}
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if cfg, err = testApp.loadConfig(); err != nil {
		t.Fatal(err)
	}
	if got := testApp.profileLane(cfg); got != lane {
		t.Fatalf("after a switch and back, lane = %q, want %q", got, lane)
	}
	if _, err := testApp.reloadHistoryState(cfg.ProfileName, lane, gen, false); err == nil || err.Error() != "active sign-in changed during upload" {
		t.Fatalf("after a switch and back: %v, want active sign-in changed during upload", err)
	}
}
