package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

func TestClaudeDiscoverHistoryFiltersWindowAndActive(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	projDir := filepath.Join(cfgDir, "projects", "-work-demo")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	writeFileWithMtime := func(path, content string, mtime time.Time) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	// Eligible session (2 days old) with a subagent file (3 hours old).
	eligiblePath := filepath.Join(projDir, "11111111-1111-1111-1111-111111111111.jsonl")
	subPath := filepath.Join(projDir, "11111111-1111-1111-1111-111111111111", "subagents", "agent-sub1.jsonl")
	writeFileWithMtime(eligiblePath, `{"type":"user","sessionId":"11111111-1111-1111-1111-111111111111","cwd":"/repo/a","timestamp":"2026-11-23T10:00:00Z","message":{"role":"user","content":"hi"}}`+"\n", now.Add(-48*time.Hour))
	writeFileWithMtime(subPath, `{"type":"user","sessionId":"11111111-1111-1111-1111-111111111111","agentId":"sub1","cwd":"/repo/a","timestamp":"2026-11-25T09:00:00Z","message":{"role":"user","content":"sub"}}`+"\n", now.Add(-3*time.Hour))

	// Active session (modified 20 minutes ago via subagent even though main is 2 hours old).
	activeMain := filepath.Join(projDir, "22222222-2222-2222-2222-222222222222.jsonl")
	activeSub := filepath.Join(projDir, "22222222-2222-2222-2222-222222222222", "subagents", "agent-live.jsonl")
	writeFileWithMtime(activeMain, `{"type":"user","sessionId":"22222222-2222-2222-2222-222222222222"}`+"\n", now.Add(-2*time.Hour))
	writeFileWithMtime(activeSub, `{"type":"user","sessionId":"22222222-2222-2222-2222-222222222222"}`+"\n", now.Add(-20*time.Minute))

	// Outside --since (120 days old).
	oldPath := filepath.Join(projDir, "33333333-3333-3333-3333-333333333333.jsonl")
	writeFileWithMtime(oldPath, `{"type":"user","sessionId":"33333333-3333-3333-3333-333333333333"}`+"\n", now.Add(-120*24*time.Hour))

	files, err := (Agent{}).DiscoverHistory(context.Background(), agents.HistoryDiscoverOptions{
		Since:        now.Add(-90 * 24 * time.Hour),
		Now:          now,
		ActiveWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]agents.HistoryFile{}
	for _, f := range files {
		byID[f.SessionID] = f
	}
	if len(byID) != 3 {
		t.Fatalf("discovered %d files, want 3: %+v", len(byID), files)
	}
	if f := byID["11111111-1111-1111-1111-111111111111"]; f.Active || f.OutsideSince || len(f.ExtraPaths) != 1 {
		t.Errorf("eligible file = %+v, want !Active, !OutsideSince, 1 ExtraPath", f)
	}
	if f := byID["22222222-2222-2222-2222-222222222222"]; !f.Active || f.OutsideSince {
		t.Errorf("active file = %+v, want Active=true, OutsideSince=false", f)
	}
	if f := byID["33333333-3333-3333-3333-333333333333"]; f.Active || !f.OutsideSince {
		t.Errorf("old file = %+v, want Active=false, OutsideSince=true", f)
	}
}

func TestClaudeReadHistoryFidelitySegmentsAndContentFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-cc-1.jsonl")
	lines := []string{
		`{"type":"api-request","sessionId":"sess-cc-1","id":"reqref-1","querySource":"repl_main_thread","params":{"model":"claude-opus-5-5","output_config":{"effort":"high"}}}`,
		`{"type":"user","uuid":"u-1","promptId":"p-1","sessionId":"sess-cc-1","cwd":"/work/repo-a","version":"2.1.301","entrypoint":"cli","timestamp":"2026-10-01T10:00:00.123Z","message":{"role":"user","content":"SECRET_PROMPT_REPO_A"}}`,
		// Streamed assistant chunks for the same requestId: thinking -> tool_use -> final stop_reason.
		`{"type":"assistant","uuid":"a-1a","sessionId":"sess-cc-1","cwd":"/work/repo-a","version":"2.1.301","timestamp":"2026-10-01T10:00:01.000Z","requestId":"req_01ABC","requestRef":"reqref-1","message":{"id":"msg_01","role":"assistant","model":"claude-opus-5-5","content":[{"type":"thinking","thinking":"hmm"}],"usage":{"input_tokens":100,"output_tokens":5,"cache_read_input_tokens":500,"cache_creation_input_tokens":200}}}`,
		`{"type":"assistant","uuid":"a-1b","sessionId":"sess-cc-1","cwd":"/work/repo-a","version":"2.1.301","timestamp":"2026-10-01T10:00:02.500Z","requestId":"req_01ABC","requestRef":"reqref-1","costUSD":0.0125,"durationMs":2300,"message":{"id":"msg_01","role":"assistant","model":"claude-opus-5-5","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"echo SECRET_TOOL_CMD"}}],"usage":{"input_tokens":100,"output_tokens":42,"cache_read_input_tokens":500,"cache_creation_input_tokens":200,"cache_creation":{"ephemeral_5m_input_tokens":150,"ephemeral_1h_input_tokens":50},"speed":"standard"}}}`,
		`{"type":"user","uuid":"u-tr-1","promptId":"p-1","sessionId":"sess-cc-1","cwd":"/work/repo-a","timestamp":"2026-10-01T10:00:03.000Z","permissionDecision":{"decision":"accept","source":"user_temporary"},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"SECRET_TOOL_OUT","is_error":false}]}}`,
		// Final text reply in repo-a.
		`{"type":"assistant","uuid":"a-2","sessionId":"sess-cc-1","cwd":"/work/repo-a","timestamp":"2026-10-01T10:00:04.000Z","requestId":"req_02DEF","message":{"id":"msg_02","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn","content":[{"type":"text","text":"SECRET_REPLY_REPO_A"}],"usage":{"input_tokens":120,"output_tokens":18,"cache_read_input_tokens":600,"cache_creation_input_tokens":0}}}`,
		// Entry with missing cwd (must be emitted in its own Cwd=="" segment).
		`{"type":"user","uuid":"u-nocwd","promptId":"p-2","sessionId":"sess-cc-1","timestamp":"2026-10-01T10:01:00.000Z","message":{"role":"user","content":"NO_CWD_PROMPT"}}`,
		// Resumed in a second repo (/work/repo-b) using requestRef without requestId.
		`{"type":"user","uuid":"u-3","promptId":"p-3","sessionId":"sess-cc-1","cwd":"/work/repo-b","timestamp":"2026-10-01T10:02:00.000Z","message":{"role":"user","content":"PROMPT_REPO_B"}}`,
		`{"type":"assistant","uuid":"a-3","sessionId":"sess-cc-1","cwd":"/work/repo-b","timestamp":"2026-10-01T10:02:02.000Z","requestRef":"reqref-b","effort":"medium","message":{"id":"msg_03","role":"assistant","model":"claude-sonnet-5-5","stop_reason":"end_turn","content":[{"type":"text","text":"REPLY_REPO_B"}],"usage":{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var segs []agents.HistorySegment
	err := (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{
		SourceSystem: "claude",
		SessionID:    "sess-cc-1",
		Path:         path,
	}, agents.HistoryReadOptions{IncludeSupplemental: true}, func(seg agents.HistorySegment) error {
		segs = append(segs, seg)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("got %d segments, want 3 (repo-a, empty cwd, repo-b)", len(segs))
	}
	if segs[0].Cwd != "/work/repo-a" || segs[1].Cwd != "" || segs[2].Cwd != "/work/repo-b" {
		t.Fatalf("segment cwds = [%q, %q, %q], want [/work/repo-a, \"\", /work/repo-b]", segs[0].Cwd, segs[1].Cwd, segs[2].Cwd)
	}

	// Verify repo-a records match Claude Code's live OTLP log structure and deterministic IDs.
	rl := segs[0].NativeLogs.ResourceLogs[0]
	if got := kvStr(rl.Resource.Attributes, "service.name"); got != "claude-code" {
		t.Errorf("service.name = %q, want claude-code", got)
	}
	if got := kvStr(rl.Resource.Attributes, "service.version"); got != "2.1.301" {
		t.Errorf("service.version = %q, want 2.1.301", got)
	}
	if !kvBoolTrue(rl.Resource.Attributes, semconv.TermaSessionHistoricalKey) {
		t.Errorf("resource missing %s=true", semconv.TermaSessionHistoricalKey)
	}
	sl := rl.ScopeLogs[0]
	if sl.Scope.Name != "com.anthropic.claude_code.events" {
		t.Errorf("scope = %q, want com.anthropic.claude_code.events", sl.Scope.Name)
	}

	counts := map[string]int{}
	var apiReq1 *logspb.LogRecord
	var toolRes *logspb.LogRecord
	for _, lr := range sl.LogRecords {
		if !kvBoolTrue(lr.Attributes, semconv.TermaSessionHistoricalKey) {
			t.Errorf("record %q missing %s=true", kvStr(lr.Attributes, "event.name"), semconv.TermaSessionHistoricalKey)
		}
		if lr.TimeUnixNano == 0 {
			t.Errorf("record %q has zero TimeUnixNano", kvStr(lr.Attributes, "event.name"))
		}
		name := kvStr(lr.Attributes, "event.name")
		counts[name]++
		if name == "api_request" && kvStr(lr.Attributes, "request_id") == "req_01ABC" {
			apiReq1 = lr
		}
		if name == "tool_result" {
			toolRes = lr
		}
	}
	// Streamed chunks for req_01ABC must coalesce into 1 api_request, plus req_02DEF = 2 total.
	if counts["api_request"] != 2 || counts["user_prompt"] != 1 || counts["assistant_response"] != 1 || counts["tool_result"] != 1 || counts["tool_decision"] != 1 {
		t.Fatalf("repo-a event counts = %v, want api_request=2 user_prompt=1 assistant_response=1 tool_result=1 tool_decision=1", counts)
	}
	if apiReq1 == nil {
		t.Fatal("missing api_request for req_01ABC")
	}
	if got := kvInt(apiReq1.Attributes, "input_tokens"); got != 100 {
		t.Errorf("input_tokens = %d, want 100", got)
	}
	if got := kvInt(apiReq1.Attributes, "output_tokens"); got != 42 {
		t.Errorf("output_tokens = %d, want 42 (final streamed usage)", got)
	}
	if got := kvInt(apiReq1.Attributes, "cache_read_tokens"); got != 500 {
		t.Errorf("cache_read_tokens = %d, want 500", got)
	}
	if got := kvInt(apiReq1.Attributes, "cache_creation_tokens"); got != 200 {
		t.Errorf("cache_creation_tokens = %d, want 200", got)
	}
	if got := kvInt(apiReq1.Attributes, "cache_creation_5m_tokens"); got != 150 {
		t.Errorf("cache_creation_5m_tokens = %d, want 150", got)
	}
	if got := kvInt(apiReq1.Attributes, "cache_creation_1h_tokens"); got != 50 {
		t.Errorf("cache_creation_1h_tokens = %d, want 50", got)
	}
	if got := kvStr(apiReq1.Attributes, "effort"); got != "high" {
		t.Errorf("effort = %q, want high", got)
	}
	if got := kvStr(apiReq1.Attributes, "query_source"); got != "repl_main_thread" {
		t.Errorf("query_source = %q, want repl_main_thread", got)
	}
	wantTs := time.Date(2026, 10, 1, 10, 0, 2, 500_000_000, time.UTC).UnixNano()
	if apiReq1.TimeUnixNano != uint64(wantTs) {
		t.Errorf("api_request timestamp = %d, want %d", apiReq1.TimeUnixNano, wantTs)
	}
	if toolRes == nil || kvStr(toolRes.Attributes, "tool_use_id") != "toolu_1" || kvStr(toolRes.Attributes, "tool_name") != "Bash" {
		t.Fatalf("unexpected tool_result record: %+v", toolRes)
	}
	if got := kvInt(toolRes.Attributes, "duration_ms"); got != 500 {
		t.Errorf("tool_result duration_ms = %d, want 500", got)
	}

	// Verify repo-b uses client_request_id when only requestRef is present.
	repoBRecs := segs[2].NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords
	foundRef := false
	for _, lr := range repoBRecs {
		if kvStr(lr.Attributes, "event.name") == "api_request" && kvStr(lr.Attributes, "client_request_id") == "reqref-b" {
			foundRef = true
		}
	}
	if !foundRef {
		t.Errorf("repo-b api_request missing client_request_id=reqref-b")
	}

	// Verify relay content filter leaves zero unclassified keys and strips withheld secrets.
	filter := relay.NewContentFilter([]shape.Capturer{Agent{}})
	filtered := proto.CloneOf(segs[0].NativeLogs)
	_, unclassified := filter.FilterLogs(filtered, false, false)
	if len(unclassified) != 0 {
		t.Fatalf("restrictive content filter reported unclassified keys: %v", unclassified)
	}
	wire, err := proto.Marshal(filtered)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "SECRET_") {
		t.Fatalf("withheld content survived content filter")
	}
	for _, lr := range filtered.ResourceLogs[0].ScopeLogs[0].LogRecords {
		if !kvBoolTrue(lr.Attributes, semconv.TermaSessionHistoricalKey) {
			t.Errorf("historical marker dropped by content filter on %s", kvStr(lr.Attributes, "event.name"))
		}
	}
}

func TestClaudeAPIErrorEmitsOnlyAPIErrorRecord(t *testing.T) {
	t.Parallel()
	line := `{"type":"assistant","uuid":"u-err","sessionId":"sess-err","cwd":"/work/a","timestamp":"2026-11-20T10:00:00Z","isApiErrorMessage":true,"message":{"id":"msg_err","model":"<synthetic>","content":[{"type":"text","text":"API error: overloaded"}]}}` + "\n"
	path := filepath.Join(t.TempDir(), "sess-err.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	var segs []agents.HistorySegment
	if err := (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{
		SourceSystem: "claude", SessionID: "sess-err", Path: path,
	}, agents.HistoryReadOptions{}, func(seg agents.HistorySegment) error {
		segs = append(segs, seg)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 {
		t.Fatalf("got %d segments, want 1", len(segs))
	}
	recs := segs[0].NativeLogs.ResourceLogs[0].ScopeLogs[0].LogRecords
	if len(recs) != 1 || kvStr(recs[0].Attributes, "event.name") != "api_error" {
		t.Fatalf("expected only 1 api_error record, got %d", len(recs))
	}
}

func TestClaudeCrossCwdToolArgsDroppedAndToolReachesCaptured(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"type":"assistant","uuid":"a1","sessionId":"sess-xc","cwd":"/work/repo-a","timestamp":"2026-11-20T10:00:00Z","requestId":"r1","message":{"id":"m1","role":"assistant","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"tu-1","name":"Read","input":{"file_path":"/work/repo-b/SECRET_ORG_A_ARG.go"}}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"user","uuid":"u2","sessionId":"sess-xc","cwd":"/work/repo-b","timestamp":"2026-11-20T10:05:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-1","content":"SECRET_ORG_A_OUTPUT"}]}}`,
		`{"type":"user","uuid":"u3","sessionId":"sess-xc","cwd":"/work/repo-b","timestamp":"2026-11-20T10:06:00Z","message":{"role":"user","content":"normal prompt in b"}}`,
	}
	path := filepath.Join(t.TempDir(), "sess-xc.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var segs []agents.HistorySegment
	if err := (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{
		SourceSystem: "claude", SessionID: "sess-xc", Path: path,
	}, agents.HistoryReadOptions{}, func(seg agents.HistorySegment) error {
		segs = append(segs, seg)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
	if len(segs[0].ToolReaches) == 0 {
		t.Errorf("segment 0 missing ToolReaches for Read call")
	}
	wireB, _ := proto.Marshal(segs[1].NativeLogs)
	if strings.Contains(string(wireB), "SECRET_ORG_A") {
		t.Fatalf("org A tool_input or delayed tool_result leaked into org B segment")
	}
}

func TestClaudeForkSessionIDBranchHistoricalEnvAndBareReach(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	repoA := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoA, "notes"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Uploader's live shell has WORKSPACE=repoA, which must NOT be trusted for historical Bash reach.
	t.Setenv("WORKSPACE", repoA)

	projDir := filepath.Join(cfgDir, "projects", "-work-repo-a")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	forkID := "11111111-2222-3333-4444-555555555555"
	origID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	forkPath := filepath.Join(projDir, forkID+".jsonl")
	lines := []string{
		`{"type":"assistant","uuid":"a1","sessionId":"` + origID + `","cwd":"` + repoA + `","gitBranch":"feature-branch","timestamp":"2026-11-20T10:00:00Z","requestId":"r1","message":{"id":"m1","role":"assistant","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"tu-env","name":"Bash","input":{"command":"cat \"$WORKSPACE/notes\""}}]}}`,
	}
	if err := os.WriteFile(forkPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(forkPath, old, old); err != nil {
		t.Fatal(err)
	}

	files, err := (Agent{}).DiscoverHistory(context.Background(), agents.HistoryDiscoverOptions{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].SessionID != forkID {
		t.Fatalf("got files=%+v, want single file with filename SessionID %q", files, forkID)
	}
	var segs []agents.HistorySegment
	if err := (Agent{}).ReadHistory(context.Background(), files[0], agents.HistoryReadOptions{}, func(seg agents.HistorySegment) error {
		segs = append(segs, seg)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 || segs[0].RecordCount != 0 || segs[0].FallbackBranch != "feature-branch" {
		t.Fatalf("got segs=%+v, want 1 reach-only segment with FallbackBranch=feature-branch", segs)
	}
	sawUnknown := false
	for _, r := range segs[0].ToolReaches {
		for _, p := range r.Paths {
			if strings.Contains(p, "\x00unknown") {
				sawUnknown = true
			}
		}
	}
	if !sawUnknown {
		t.Fatalf("expected UnknownReach for historical $WORKSPACE env var, got %+v", segs[0].ToolReaches)
	}
}

func TestClaudeBranchSwitchFlushesAndLongSingleCwdIsBounded(t *testing.T) {
	t.Parallel()
	var lines []string
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	lines = append(lines,
		`{"type":"user","uuid":"u1","sessionId":"sess-br","cwd":"/work/repo-a","gitBranch":"branch-one","timestamp":"2026-11-20T10:00:00Z","message":{"role":"user","content":"first"}}`,
		`{"type":"user","uuid":"u2","sessionId":"sess-br","cwd":"/work/repo-a","gitBranch":"branch-two","timestamp":"2026-11-20T10:01:00Z","message":{"role":"user","content":"second"}}`,
	)
	for i := range 270 {
		ts := t0.Add(time.Duration(i+120) * time.Second).Format(time.RFC3339)
		lines = append(lines, `{"type":"user","uuid":"ub-`+string(rune('a'+(i%26)))+`","sessionId":"sess-br","cwd":"/work/repo-a","gitBranch":"branch-two","timestamp":"`+ts+`","message":{"role":"user","content":"msg"}}`)
	}
	path := filepath.Join(t.TempDir(), "sess-br.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var segs []agents.HistorySegment
	if err := (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{
		SourceSystem: "claude", SessionID: "sess-br", Path: path,
	}, agents.HistoryReadOptions{}, func(seg agents.HistorySegment) error {
		segs = append(segs, seg)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(segs) < 3 {
		t.Fatalf("expected >= 3 segments (branch switch + bounded flush), got %d", len(segs))
	}
	if segs[0].FallbackBranch != "branch-one" || segs[1].FallbackBranch != "branch-two" {
		t.Fatalf("branches = (%q, %q), want (branch-one, branch-two)", segs[0].FallbackBranch, segs[1].FallbackBranch)
	}
}

func TestClaudeUnreadableSubagentsDirSetsCompanionReadError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory mode 0000")
	}
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	projDir := filepath.Join(cfgDir, "projects", "-work-repo-a")
	sid := "22222222-3333-4444-5555-666666666666"
	subDir := filepath.Join(projDir, sid, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(projDir, sid+".jsonl")
	if err := os.WriteFile(mainPath, []byte(`{"type":"user","cwd":"/work/repo-a","message":{"role":"user","content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(mainPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(subDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(subDir, 0o755) })

	files, err := (Agent{}).DiscoverHistory(context.Background(), agents.HistoryDiscoverOptions{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].CompanionReadError != subDir {
		t.Fatalf("expected CompanionReadError to name the unreadable subagents dir, got %+v", files)
	}
}

func kvStr(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func kvInt(attrs []*commonpb.KeyValue, key string) int64 {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetIntValue()
		}
	}
	return -1
}

func kvBoolTrue(attrs []*commonpb.KeyValue, key string) bool {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			b, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_BoolValue)
			return ok && b.BoolValue
		}
	}
	return false
}

// A tool_result whose tool_use was never seen is dropped: its call's working directory is
// unknown. One whose tool_use came before the bounded mid-session flush is kept.
func TestClaudeToolResultWithNoKnownCallIsDropped(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"type":"assistant","uuid":"a1","sessionId":"sess-or","cwd":"/work/repo-a","timestamp":"2026-11-20T10:00:00Z","requestId":"r1","message":{"id":"m1","role":"assistant","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"tu-long","name":"Bash","input":{"command":"echo hi"}}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
	}
	t0 := time.Date(2026, 11, 20, 10, 1, 0, 0, time.UTC)
	for i := range 270 {
		lines = append(lines, `{"type":"user","uuid":"ub-`+string(rune('a'+(i%26)))+`","sessionId":"sess-or","cwd":"/work/repo-a","timestamp":"`+t0.Add(time.Duration(i)*time.Second).Format(time.RFC3339)+`","message":{"role":"user","content":"msg"}}`)
	}
	lines = append(lines,
		`{"type":"user","uuid":"u2","sessionId":"sess-or","cwd":"/work/repo-a","timestamp":"2026-11-20T11:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-never","content":"ORPHAN_OUTPUT"}]}}`,
		`{"type":"user","uuid":"u3","sessionId":"sess-or","cwd":"/work/repo-a","timestamp":"2026-11-20T11:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-long","content":"KEPT_LONG_OUTPUT"}]}}`,
	)
	path := filepath.Join(t.TempDir(), "sess-or.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	results := map[string]bool{}
	segs := 0
	if err := (Agent{}).ReadHistory(context.Background(), agents.HistoryFile{
		SourceSystem: "claude", SessionID: "sess-or", Path: path,
	}, agents.HistoryReadOptions{IncludeSupplemental: true}, func(seg agents.HistorySegment) error {
		segs++
		for _, rl := range seg.NativeLogs.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					attrs := map[string]string{}
					for _, kv := range lr.GetAttributes() {
						attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
					}
					if attrs["event.name"] == "tool_result" {
						results[attrs["tool_use_id"]] = true
					}
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if segs < 2 {
		t.Fatalf("got %d segments, want the bounded flush to split the session", segs)
	}
	if results["tu-never"] {
		t.Errorf("a tool_result with no known tool_use was emitted")
	}
	if !results["tu-long"] {
		t.Errorf("a tool_result after the bounded flush was dropped")
	}
}
